package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type fakeSummaryExtractor struct {
	calls         int
	previous      *usermemory.SessionSummary
	turns         []usermemory.SessionTurn
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

func (f *fakeSummaryExtractor) Compact(_ context.Context, previous *usermemory.SessionSummary, turns []usermemory.SessionTurn, lastErrorCode string) (usermemory.SummaryArtifact, error) {
	f.calls++
	f.lastErrorCode = lastErrorCode
	f.previous = previous
	f.turns = append([]usermemory.SessionTurn(nil), turns...)
	if f.preempt != nil {
		f.preempt()
	}
	if f.calls <= len(f.results) && f.results[f.calls-1] != nil {
		return usermemory.SummaryArtifact{}, f.results[f.calls-1]
	}
	if f.err != nil {
		return usermemory.SummaryArtifact{}, f.err
	}
	commitments := []string{"Report progress"}
	if previous != nil {
		commitments = append(append([]string(nil), previous.Commitments...), "Finish review")
	}
	return usermemory.SummaryArtifact{
		Narrative: "The user is progressing through Atlas work.",
		OpenTasks: []string{"Continue Atlas"}, Commitments: commitments,
		Entities: []string{"Atlas"}, Decisions: []string{"Work sequentially"}, TopicTags: []string{"project"},
	}, nil
}

func TestServicePlansCompactsAndPreservesRecentTail(t *testing.T) {
	store := newSessionRuntimeStore(t)
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
	extractor := &fakeSummaryExtractor{}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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
	if extractor.calls != 1 || extractor.previous != nil || len(extractor.turns) != 23 {
		t.Fatalf("extractor calls=%d previous=%+v turns=%d", extractor.calls, extractor.previous, len(extractor.turns))
	}
	summary, err := store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 23 || summary.CoveredThroughTurnID != extractor.turns[len(extractor.turns)-1].ID {
		t.Fatalf("summary=%+v", summary)
	}
	tail, err := store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != maximumRecentTail {
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
	if extractor.calls != 2 || extractor.previous == nil || len(extractor.previous.Commitments) != 1 || extractor.previous.Commitments[0] != "Report progress" {
		t.Fatalf("previous checkpoint was not supplied: calls=%d previous=%+v", extractor.calls, extractor.previous)
	}
	summary, err = store.LatestSessionSummary(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.SourceTurnIDs) != 40 || len(summary.Commitments) != 2 || summary.Commitments[0] != "Report progress" || summary.Commitments[1] != "Finish review" {
		t.Fatalf("incremental checkpoint lost continuity: %+v", summary)
	}
	tail, err = store.RecentCompletedExchangesAfter(context.Background(), "user-1", "session-1", profile.Generation, summary.CoveredThroughTurnID, 100)
	if err != nil || len(tail) != maximumRecentTail {
		t.Fatalf("incremental tail=%d err=%v", len(tail), err)
	}
	active, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(active) != 0 {
		t.Fatalf("compaction created durable memory: %+v err=%v", active, err)
	}
}

func TestServicePublishesLegacyArtifactWithoutStagingCandidates(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	legacy := usermemory.SummaryArtifact{
		Narrative: "Legacy summary", GenerationModel: job.Model, GeneratorVersion: job.GeneratorVersion,
		Candidates: []usermemory.CompactionCandidateArtifact{{
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
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			t.Fatal(err)
		}
	}
	extractor := &fakeSummaryExtractor{}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(unavailableLowPriorityGate{})
	if _, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), service.owner, time.Minute, "model", SummaryGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); !errors.Is(err, errLowPriorityUnavailable) || extractor.calls != 0 {
		t.Fatalf("process error=%v extractor calls=%d", err, extractor.calls)
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
	store := newSessionRuntimeStore(t)
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
	extractor := &fakeSummaryExtractor{preempt: func() { gate.cancel() }}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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

func TestServiceRefundsCompactionSubmissionBeforeProviderAcceptance(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	gate := &canceledLowPriorityGate{}
	extractor := &fakeSummaryExtractor{preempt: func() { gate.cancel() }, err: context.Canceled}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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
	if state != "retry" || submissions != 0 || extractor.calls != 1 {
		t.Fatalf("state=%q submissions=%d calls=%d", state, submissions, extractor.calls)
	}
}

func TestServiceBoundsInvalidCompactionOutput(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeSummaryExtractor{err: invalidCompactionOutput("missing_tool_call")}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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
	if state != "retry" || attempts != 1 || invalidRetries != 1 || submissions != 1 || extractor.calls != 1 {
		t.Fatalf("first state=%q attempts=%d invalid_retries=%d submissions=%d calls=%d", state, attempts, invalidRetries, submissions, extractor.calls)
	}
	makeCompactionJobReady(t, db, jobID)
	service.drain(context.Background())
	if err := db.SQL().QueryRow(`SELECT state, attempt_count, compaction_invalid_output_retry_count, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &invalidRetries, &submissions); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || attempts != 2 || invalidRetries != 1 || submissions != 2 || extractor.calls != 2 || extractor.lastErrorCode != "missing_tool_call" {
		t.Fatalf("terminal state=%q attempts=%d invalid_retries=%d submissions=%d calls=%d", state, attempts, invalidRetries, submissions, extractor.calls)
	}
}

func TestServiceSkipsPermanentCompactionProviderFailure(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeSummaryExtractor{err: &permanentProviderError{statusCode: 400}}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	var state, code string
	if err := db.SQL().QueryRow(`SELECT state, last_error_code FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || code != "provider_request_rejected" || extractor.calls != 1 {
		t.Fatalf("state=%q code=%q calls=%d", state, code, extractor.calls)
	}
}

func TestServiceForegroundPreemptionNeverConsumesCompactionBudget(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	gate := &canceledLowPriorityGate{}
	extractor := &fakeSummaryExtractor{preempt: func() { gate.cancel() }, err: &llm.ProviderRequestStartedError{Cause: context.Canceled}}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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
		if state != "retry" || attempts != 0 || submissions != 0 || code != "foreground_preempted" || extractor.calls != i+1 {
			t.Fatalf("iteration=%d state=%q attempts=%d submissions=%d code=%q calls=%d", i, state, attempts, submissions, code, extractor.calls)
		}
		makeCompactionJobReady(t, db, jobID)
	}
}

func TestServiceCompactionProviderCallsHaveAbsoluteBound(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", 25)
	if err != nil {
		t.Fatal(err)
	}
	extractor := &fakeSummaryExtractor{results: []error{invalidCompactionOutput("missing_tool_call")}, err: errors.New("provider unavailable")}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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
			if extractor.calls != usermemory.DurableModelSubmissionLimit || submissions != usermemory.DurableModelSubmissionLimit || attempts != usermemory.DurableModelSubmissionLimit || invalidRetries != 1 || extractor.lastErrorCode != "missing_tool_call" {
				t.Fatalf("calls=%d submissions=%d attempts=%d invalid_retries=%d corrective_code=%q", extractor.calls, submissions, attempts, invalidRetries, extractor.lastErrorCode)
			}
			return
		default:
			t.Fatalf("unexpected state=%q", state)
		}
	}
}

func TestServicePlannerWaitsBelowThreshold(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maximumRecentTail+1; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, "short", "short", 99999, 100000); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID != 0 {
		t.Fatalf("unexpected plan job=%d err=%v", jobID, err)
	}
}

func TestServicePlannerTriggersAtPressureBoundary(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, fmt.Sprintf("turn %d", i), "answer", 100000, 100000); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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

func TestPreservedRecentTailIsAtMostTwoCompleteExchanges(t *testing.T) {
	small := usermemory.SessionTurn{UserText: "short", AssistantText: "short"}
	if got := preservedRecentTailCount([]usermemory.SessionTurn{small, small, small}, 100000); got != 2 {
		t.Fatalf("small tail count=%d", got)
	}
	oversized := usermemory.SessionTurn{UserText: strings.Repeat("x", 9000), AssistantText: strings.Repeat("y", 9000)}
	if got := preservedRecentTailCount([]usermemory.SessionTurn{small, oversized}, 4000); got != 0 {
		t.Fatalf("oversized newest tail count=%d", got)
	}
}

func TestServiceCampaignContinuesAfterFirstChunk(t *testing.T) {
	store := newSessionRuntimeStore(t)
	profile, err := seedCompactionRuntimeTurns(t, store, "session-1", maximumCompactionRange+6)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, &fakeSummaryExtractor{}, "model", promptbudget.ContextBudget{PromptLimit: 100000}, config.NewLogger(config.LevelError))
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

func TestServiceRecordsUncompactableCompleteExchangeWithoutProviderCall(t *testing.T) {
	store, db := newSessionRuntimeStoreWithDB(t)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendDeliveredPressureTurn(store, "session-1", "user-1", profile.Generation, strings.Repeat("large ", 1000), "answer", 100, 100); err != nil {
			t.Fatal(err)
		}
	}
	extractor := &fakeSummaryExtractor{}
	service := NewService(store, extractor, "model", promptbudget.ContextBudget{PromptLimit: 100}, config.NewLogger(config.LevelError))
	jobID, err := service.plan(context.Background(), "user-1", "session-1", profile.Generation)
	if err != nil || jobID == 0 {
		t.Fatalf("receipt job=%d err=%v", jobID, err)
	}
	var state, code string
	if err := db.SQL().QueryRow(`SELECT state, last_error_code FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || code != "uncompactable_complete_exchange" || extractor.calls != 0 {
		t.Fatalf("state=%q code=%q extractor_calls=%d", state, code, extractor.calls)
	}
}

func newSessionRuntimeStore(t *testing.T) *usermemory.Store {
	t.Helper()
	store, _ := newSessionRuntimeStoreWithDB(t)
	return store
}

func newSessionRuntimeStoreWithDB(t *testing.T) (*usermemory.Store, *database.DB) {
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
	store := usermemory.NewStore(path, log)
	t.Cleanup(func() {
		store.Close() // nolint:errcheck
		db.Close()    // nolint:errcheck
	})
	return store, db
}

func seedCompactionRuntimeTurns(t *testing.T, store *usermemory.Store, sessionID string, count int) (usermemory.SessionProfile, error) {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", sessionID, time.Hour)
	if err != nil {
		return usermemory.SessionProfile{}, err
	}
	for i := 1; i <= count; i++ {
		if err := appendDeliveredPressureTurn(store, sessionID, "user-1", profile.Generation, fmt.Sprintf("I am working on Atlas item %d.", i), "Progress recorded.", 100001, 100000); err != nil {
			return usermemory.SessionProfile{}, err
		}
	}
	return profile, nil
}

func appendDeliveredPressureTurn(store *usermemory.Store, sessionID, userID string, generation int, userText, assistantText string, tokens, limit int) error {
	turn, err := store.AppendSessionTurnForGenerationResultWithPressure(context.Background(), sessionID, userID, generation, userText, assistantText, nil, time.Hour, usermemory.SessionPromptPressure{Tokens: tokens, Limit: limit, Version: promptPressureVersion("model", limit)})
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
