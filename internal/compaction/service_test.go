package compaction

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

type fakeSummaryCompactor struct {
	calls         int
	previous      *memory.SessionSummary
	turns         []memory.SessionTurn
	preempt       func()
	err           error
	results       []error
	lastErrorCode string
}

type unavailableLowPriorityGate struct{}

func (unavailableLowPriorityGate) TryAcquireLowPriority(context.Context) (context.Context, func(), bool) {
	return nil, nil, false
}

type canceledLowPriorityGate struct {
	cancel context.CancelFunc
}

func (g *canceledLowPriorityGate) TryAcquireLowPriority(parent context.Context) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	return ctx, func() {}, true
}

func (f *fakeSummaryCompactor) Compact(_ context.Context, previous *memory.SessionSummary, turns []memory.SessionTurn, lastErrorCode string) (memory.SummaryArtifact, error) {
	f.calls++
	f.lastErrorCode = lastErrorCode
	f.previous = previous
	f.turns = append([]memory.SessionTurn(nil), turns...)
	if f.preempt != nil {
		f.preempt()
	}
	if f.calls <= len(f.results) && f.results[f.calls-1] != nil {
		return memory.SummaryArtifact{}, f.results[f.calls-1]
	}
	if f.err != nil {
		return memory.SummaryArtifact{}, f.err
	}
	commitments := []string{"Report progress"}
	if previous != nil {
		commitments = append(append([]string(nil), previous.Commitments...), "Finish review")
	}
	return memory.SummaryArtifact{
		Narrative: "The user is progressing through Atlas work.",
		OpenTasks: []string{"Continue Atlas"}, Commitments: commitments,
		Entities: []string{"Atlas"}, Decisions: []string{"Work sequentially"}, TopicTags: []string{"project"},
	}, nil
}

func TestServicePlansCompactsAllEligibleTurns(t *testing.T) {
	store := newCompactionTestStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		text := fmt.Sprintf("I am working on Atlas item %d.", i)
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, text, "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	compactor := &fakeSummaryCompactor{}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("plan job=%d err=%v", jobID, err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.Model != "model" || job.GeneratorVersion != SummaryGeneratorVersion {
		t.Fatalf("job model=%q generator version=%q", job.Model, job.GeneratorVersion)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if compactor.calls != 1 || compactor.previous != nil || len(compactor.turns) != 25 {
		t.Fatalf("compactor calls=%d previous=%+v turns=%d", compactor.calls, compactor.previous, len(compactor.turns))
	}
	summary, err := store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 25 || summary.CoveredThroughTurnID != compactor.turns[len(compactor.turns)-1].ID {
		t.Fatalf("summary=%+v", summary)
	}
	tail, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != 0 {
		t.Fatalf("tail=%d err=%v", len(tail), err)
	}
	for i := 26; i <= 42; i++ {
		text := fmt.Sprintf("I am working on Atlas item %d.", i)
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, text, "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	jobID, err = service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("incremental plan job=%d err=%v", jobID, err)
	}
	job, err = store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if compactor.calls != 2 || compactor.previous == nil || len(compactor.previous.Commitments) != 1 || compactor.previous.Commitments[0] != "Report progress" {
		t.Fatalf("previous checkpoint was not supplied: calls=%d previous=%+v", compactor.calls, compactor.previous)
	}
	summary, err = store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 42 || len(summary.Commitments) != 2 || summary.Commitments[0] != "Report progress" || summary.Commitments[1] != "Finish review" {
		t.Fatalf("incremental checkpoint lost continuity: %+v", summary)
	}
	tail, err = store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != 0 {
		t.Fatalf("incremental tail=%d err=%v", len(tail), err)
	}
	active, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(active) != 0 {
		t.Fatalf("compaction created durable memory: %+v err=%v", active, err)
	}
}

func TestServicePublishesLegacyArtifactWithoutStagingCandidates(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeSummaryCompactor{}, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	legacy := memory.SummaryArtifact{
		Narrative: "Legacy summary", GenerationModel: job.Model, GeneratorVersion: job.GeneratorVersion,
		Candidates: []memory.CompactionCandidateArtifact{{
			SourceTurnID: job.CoveredThroughTurnID + 1000, Statement: "Invalid legacy candidate", Evidence: "not in the source turn",
			Scope: "invalid", Category: "invalid", Context: "invalid", Provenance: "invalid", Sensitivity: "invalid",
			Confidence: 2, Importance: 99, ClaimSlot: "invalid", ClaimValue: "invalid",
		}},
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, legacy); err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatalf("publish legacy artifact: %v", err)
	}
	summary, err := store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || summary.Narrative != legacy.Narrative {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	var candidateCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = ?`, "user-1").Scan(&candidateCount); err != nil {
		t.Fatal(err)
	}
	if candidateCount != 0 {
		t.Fatalf("compaction staged %d memory candidates", candidateCount)
	}
}

func TestServiceCompactionYieldsWhenForegroundWorkIsBusy(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	compactor := &fakeSummaryCompactor{}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(unavailableLowPriorityGate{})
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); !errors.Is(err, errLowPriorityUnavailable) || compactor.calls != 0 {
		t.Fatalf("process error=%v compactor calls=%d", err, compactor.calls)
	}
	if err := store.DeferSessionCompactionJob(context.Background(), job, time.Second); err != nil {
		t.Fatal(err)
	}
	var submissions int
	if err := db.SQL().QueryRow(`SELECT model_submission_count FROM durable_jobs WHERE id = ?`, job.ID).Scan(&submissions); err != nil || submissions != 0 {
		t.Fatalf("deferred submissions=%d err=%v", submissions, err)
	}
}

func TestServiceDiscardsSuccessfulCompactionAfterForegroundPreemption(t *testing.T) {
	store := newCompactionTestStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	gate := &canceledLowPriorityGate{}
	compactor := &fakeSummaryCompactor{preempt: func() { gate.cancel() }}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(gate)
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); !errors.Is(err, errLowPriorityUnavailable) {
		t.Fatalf("process error=%v", err)
	}
	if _, err := store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation); err == nil {
		t.Fatal("preempted compaction published a summary")
	}
}

func TestServiceCompactionSuccessCannotResurrectResetSession(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			ctx := context.Background()
			store, db := newCompactionTestStoreWithDB(t)
			profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 3)
			if err != nil {
				t.Fatal(err)
			}
			compactor := &fakeSummaryCompactor{}
			service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
			jobID, err := service.plan(ctx, "user-1", "session-1", profile.Generation)
			if err != nil || jobID == 0 {
				t.Fatalf("plan job=%d err=%v", jobID, err)
			}
			var callbackErr error
			var state string
			var submissions int
			var resetProfile memory.SessionProfile
			compactor.preempt = func() {
				callbackErr = db.SQL().QueryRow(`SELECT state, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &submissions)
				if callbackErr == nil && reset {
					resetProfile, callbackErr = store.ResetSession(ctx, "user-1", "session-1", time.Hour)
				}
			}
			service.drain(ctx)
			service.drain(ctx)
			if callbackErr != nil || state != "running" || submissions != 1 || compactor.calls != 1 || len(compactor.turns) != 3 {
				t.Fatalf("accepted compaction call: state=%q submissions=%d calls=%d turns=%d err=%v", state, submissions, compactor.calls, len(compactor.turns), callbackErr)
			}
			if reset && resetProfile.Generation != profile.Generation+1 {
				t.Fatalf("reset generation=%d", resetProfile.Generation)
			}
			want := 1
			if reset {
				want = 0
			}
			for _, query := range []string{
				`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction'`,
				`SELECT COUNT(*) FROM session_summaries`,
			} {
				var count int
				if err := db.SQL().QueryRow(query).Scan(&count); err != nil || count != want {
					t.Fatalf("%s: count=%d want=%d err=%v", query, count, want, err)
				}
			}
			var memories int
			if err := db.SQL().QueryRow(`SELECT (SELECT COUNT(*) FROM memory_entries) + (SELECT COUNT(*) FROM memory_candidates)`).Scan(&memories); err != nil || memories != 0 {
				t.Fatalf("compaction memory artifacts=%d err=%v", memories, err)
			}
			if !reset {
				if err := db.SQL().QueryRow(`SELECT state FROM durable_jobs WHERE id = ?`, jobID).Scan(&state); err != nil || state != "succeeded" {
					t.Fatalf("control state=%q err=%v", state, err)
				}
			}
		})
	}
}

func TestServiceRefundsCompactionSubmissionBeforeProviderAcceptance(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	gate := &canceledLowPriorityGate{}
	compactor := &fakeSummaryCompactor{preempt: func() { gate.cancel() }, err: context.Canceled}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(gate)
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	var state string
	var submissions int
	if err := db.SQL().QueryRow(`SELECT state, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &submissions); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || submissions != 0 || compactor.calls != 1 {
		t.Fatalf("state=%q submissions=%d calls=%d", state, submissions, compactor.calls)
	}
}

func TestServiceBoundsInvalidCompactionOutput(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	compactor := &fakeSummaryCompactor{err: invalidCompactionOutput("missing_tool_call")}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	var state string
	var attempts, invalidRetries, submissions int
	if err := db.SQL().QueryRow(`SELECT state, attempt_count, compaction_invalid_output_retry_count, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &invalidRetries, &submissions); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attempts != 1 || invalidRetries != 1 || submissions != 1 || compactor.calls != 1 {
		t.Fatalf("first state=%q attempts=%d invalid_retries=%d submissions=%d calls=%d", state, attempts, invalidRetries, submissions, compactor.calls)
	}
	makeCompactionJobReady(t, db, jobID)
	service.drain(context.Background())
	makeCompactionJobReady(t, db, jobID)
	service.drain(context.Background())
	makeCompactionJobReady(t, db, jobID)
	service.drain(context.Background())
	if err := db.SQL().QueryRow(`SELECT state, attempt_count, compaction_invalid_output_retry_count, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &invalidRetries, &submissions); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || attempts != 4 || invalidRetries != memory.SessionCompactionInvalidOutputRetryLimit || submissions != memory.SessionCompactionModelSubmissionLimit || compactor.calls != memory.SessionCompactionModelSubmissionLimit || compactor.lastErrorCode != "missing_tool_call" {
		t.Fatalf("terminal state=%q attempts=%d invalid_retries=%d submissions=%d calls=%d", state, attempts, invalidRetries, submissions, compactor.calls)
	}
}

func TestServiceSkipsPermanentCompactionProviderFailure(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	compactor := &fakeSummaryCompactor{err: &permanentProviderError{statusCode: 400}}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	var state, code string
	if err := db.SQL().QueryRow(`SELECT state, last_error_code FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || code != "provider_request_rejected" || compactor.calls != 1 {
		t.Fatalf("state=%q code=%q calls=%d", state, code, compactor.calls)
	}
}

func TestServiceForegroundPreemptionNeverConsumesCompactionBudget(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	gate := &canceledLowPriorityGate{}
	compactor := &fakeSummaryCompactor{preempt: func() { gate.cancel() }, err: fmt.Errorf("provider stream canceled: %w", context.Canceled)}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(gate)
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		service.drain(context.Background())
		var state, code string
		var attempts, submissions int
		if err := db.SQL().QueryRow(`SELECT state, attempt_count, last_error_code, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &code, &submissions); err != nil {
			t.Fatal(err)
		}
		if state != "retry" || attempts != 0 || submissions != 0 || code != "foreground_preempted" || compactor.calls != i+1 {
			t.Fatalf("iteration=%d state=%q attempts=%d submissions=%d code=%q calls=%d", i, state, attempts, submissions, code, compactor.calls)
		}
		makeCompactionJobReady(t, db, jobID)
	}
}

func TestServiceCompactionProviderCallsHaveAbsoluteBound(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	compactor := &fakeSummaryCompactor{results: []error{invalidCompactionOutput("missing_tool_call")}, err: errors.New("provider unavailable")}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for {
		service.drain(context.Background())
		var state string
		var submissions int
		if err := db.SQL().QueryRow(`SELECT state, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &submissions); err != nil {
			t.Fatal(err)
		}
		switch state {
		case "retry":
			makeCompactionJobReady(t, db, jobID)
		case "dead":
			var attempts, invalidRetries int
			if err := db.SQL().QueryRow(`SELECT attempt_count, compaction_invalid_output_retry_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&attempts, &invalidRetries); err != nil {
				t.Fatal(err)
			}
			if compactor.calls != memory.SessionCompactionModelSubmissionLimit || submissions != memory.SessionCompactionModelSubmissionLimit || attempts != memory.SessionCompactionModelSubmissionLimit || invalidRetries != 1 || compactor.lastErrorCode != "missing_tool_call" {
				t.Fatalf("calls=%d submissions=%d attempts=%d invalid_retries=%d corrective_code=%q", compactor.calls, submissions, attempts, invalidRetries, compactor.lastErrorCode)
			}
			return
		default:
			t.Fatalf("unexpected state=%q", state)
		}
	}
}

func TestServicePlannerWaitsBelowThreshold(t *testing.T) {
	store := newCompactionTestStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, "short", "short", 69999, 100000); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(store, &fakeSummaryCompactor{}, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID != 0 {
		t.Fatalf("unexpected plan job=%d err=%v", jobID, err)
	}
}

func TestServicePlannerTriggersAtPressureBoundary(t *testing.T) {
	store := newCompactionTestStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("turn %d", i), "answer", 70000, 100000); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(store, &fakeSummaryCompactor{}, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("boundary plan job=%d err=%v", jobID, err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.CoveredThroughTurnID != job.TargetTurnID {
		t.Fatalf("single-head campaign job=%+v", job)
	}
}

func TestServiceCampaignContinuesAfterFirstChunk(t *testing.T) {
	store := newCompactionTestStore(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", maximumCompactionRange+6)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeSummaryCompactor{}, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.CoveredThroughTurnID >= job.TargetTurnID {
		t.Fatalf("first chunk unexpectedly covered campaign: %+v", job)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	nextID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || nextID == 0 || nextID == job.ID {
		t.Fatalf("continuation job=%d first=%d err=%v", nextID, job.ID, err)
	}
	next, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if next.TargetTurnID != job.TargetTurnID || next.CoveredThroughTurnID != job.TargetTurnID {
		t.Fatalf("continuation target changed: first=%+v next=%+v", job, next)
	}
}

func TestServiceCampaignTargetExceedsPlannerPage(t *testing.T) {
	ctx := context.Background()
	store := newCompactionTestStore(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 1001)
	if err != nil {
		t.Fatal(err)
	}
	newest, err := store.LatestDeliveredSessionPromptPressure(ctx, "user-1", "session-1", profile.Generation, promptPressureVersion("model", 100000))
	if err != nil {
		t.Fatal(err)
	}
	// A pending gap still bounds the target even when later turns are delivered.
	if _, err := memorytest.AppendPendingTurn(ctx, store, "session-1", "user-1", profile.Generation, "pending", "answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, "after gap", "answer", 70000, 100000); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeSummaryCompactor{}, "model", budget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	if id, err := service.plan(ctx, "user-1", "session-1", profile.Generation); err != nil || id == 0 {
		t.Fatalf("plan=%d err=%v", id, err)
	}
	job, err := store.ClaimSessionCompactionJob(ctx, service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.TargetTurnID != newest.TurnID || job.CoveredThroughTurnID >= job.TargetTurnID {
		t.Fatalf("job=%+v want target=%d", job, newest.TurnID)
	}
}

func TestServiceRecordsUncompactableCompleteExchangeWithoutProviderCall(t *testing.T) {
	store, db := newCompactionTestStoreWithDB(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, strings.Repeat("large ", 1000), "answer", 100, 100); err != nil {
			t.Fatal(err)
		}
	}
	compactor := &fakeSummaryCompactor{}
	service := NewService(store, compactor, "model", budget.ContextBudget{PromptLimit: 100}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("receipt job=%d err=%v", jobID, err)
	}
	var state, code string
	if err := db.SQL().QueryRow(`SELECT state, last_error_code FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || code != "uncompactable_complete_exchange" || compactor.calls != 0 {
		t.Fatalf("state=%q code=%q compactor_calls=%d", state, code, compactor.calls)
	}
}

func newCompactionTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, _ := newCompactionTestStoreWithDB(t)
	return store
}

func newCompactionTestStoreWithDB(t *testing.T) (*memory.Store, *database.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	db, err := database.Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('user-1')`); err != nil {
		t.Fatal(err)
	}
	store := memorytest.NewStore(t, path, log)
	t.Cleanup(func() {
		store.Close() // nolint:errcheck
		db.Close()    // nolint:errcheck
	})
	return store, db
}

func seedCompactionRuntimeTurns(t *testing.T, store *memory.Store, sessionID string, count int) (memory.SessionProfile, error) {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", sessionID, time.Hour)
	if err != nil {
		return memory.SessionProfile{}, err
	}
	for i := 1; i <= count; i++ {
		if err := appendDeliveredPressureTurn(store, sessionID, "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			return memory.SessionProfile{}, err
		}
	}
	return profile, nil
}

func appendDeliveredPressureTurn(store *memory.Store, sessionID, userID string, generation int, userText, assistantText string, tokens, limit int) error {
	turn, err := store.AppendPendingSessionTurn(context.Background(), memory.SessionTurnWrite{SessionID: sessionID, UserID: userID, Generation: generation, UserText: userText, AssistantText: assistantText, ToolNames: nil, History: memory.EmptyToolHistory(), Staged: nil, TTL: time.Hour, Pressure: memory.SessionPromptPressure{Tokens: tokens, Limit: limit, Version: promptPressureVersion("model", limit)}})
	if err != nil {
		return err
	}
	return store.MarkSessionTurnDelivered(context.Background(), userID, turn.ID)
}

func makeCompactionJobReady(t *testing.T, db *database.DB, jobID int64) {
	t.Helper()
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
}
