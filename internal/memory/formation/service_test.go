package formation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/extraction"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type fakeExtractor struct {
	candidates    []memory.MemorySaveItem
	submitted     int
	malformed     int
	err           error
	preempt       func()
	calls         int
	lastErrorCode string
}

type fakePatternExtractor struct {
	fakeExtractor
	patterns      memory.MemoryPatternBatch
	patternErr    error
	patternCalls  int
	turnIDs       []int64
	lastErrorCode string
	preempt       func()
}

func (f *fakePatternExtractor) ExtractPatterns(_ context.Context, turns []memory.StoredSessionTurn, lastErrorCode string) (memory.MemoryPatternBatch, error) {
	f.patternCalls++
	f.lastErrorCode = lastErrorCode
	f.turnIDs = f.turnIDs[:0]
	for _, turn := range turns {
		f.turnIDs = append(f.turnIDs, turn.ID)
	}
	if f.preempt != nil {
		f.preempt()
	}
	return f.patterns, f.patternErr
}

type unavailableLowPriorityGate struct{}

func (unavailableLowPriorityGate) TryAcquireLowPriority(context.Context) (context.Context, func(), bool) {
	return nil, nil, false
}

type canceledLowPriorityGate struct{}

func (canceledLowPriorityGate) TryAcquireLowPriority(parent context.Context) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	cancel()
	return ctx, func() {}, true
}

type preemptibleLowPriorityGate struct {
	cancel context.CancelFunc
}

func (g *preemptibleLowPriorityGate) TryAcquireLowPriority(parent context.Context) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	return ctx, func() {}, true
}

func TestFormationJobLeaseCoversProviderTimeout(t *testing.T) {
	if formationJobLease < 5*time.Minute {
		t.Fatalf("formation lease=%s want at least 5m", formationJobLease)
	}
}

func TestFormationJobLeaseUsesRenewableDefault(t *testing.T) {
	service := NewService(nil, nil, "model", config.NewLogger(config.LevelError))
	if service.jobLease != formationJobLease {
		t.Fatalf("job lease=%s", service.jobLease)
	}
}

func TestFormationWarningRetryAndDeadStatuses(t *testing.T) {
	for _, test := range []struct {
		name   string
		event  string
		status string
	}{
		{name: "retry", event: "user_memory.formation.job.retry", status: "retry"},
		{name: "dead", event: "user_memory.formation.job.dead", status: "degraded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := config.NewLogger(config.LevelDebug)
			logger.SetOutput(&output)
			service := NewService(nil, nil, "model", logger)
			service.warn(test.event, "fixed warning", errors.New("provider failed with token=secret"), config.F("status", test.status))

			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
				t.Fatalf("decode warning log: %v; output=%q", err, output.String())
			}
			if record["event"] != test.event || record["status"] != test.status {
				t.Fatalf("warning record=%#v", record)
			}
			if record["log_type"] != "server" || record["component"] != "user_memory.formation" {
				t.Fatalf("unstable warning scope: %#v", record)
			}
			if strings.Contains(output.String(), "secret") {
				t.Fatalf("warning leaked sanitized error: %s", output.String())
			}
		})
	}
}

func (f *fakeExtractor) Extract(_ context.Context, _ memory.StoredSessionTurn, lastErrorCode string) (memory.MemorySaveBatch, error) {
	f.calls++
	f.lastErrorCode = lastErrorCode
	if f.preempt != nil {
		f.preempt()
	}
	submitted := f.submitted
	if submitted == 0 && len(f.candidates) > 0 {
		submitted = len(f.candidates)
	}
	return memory.MemorySaveBatch{Memories: f.candidates, SubmittedCount: submitted, MalformedCount: f.malformed}, f.err
}

func TestServiceProcessesAndReplaysTurnIdempotently(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go for project Atlas", "req")
	extractor := &fakeExtractor{candidates: []memory.MemorySaveItem{{
		Statement: "The user uses Go for project Atlas.", Evidence: "I use Go for project Atlas",
		Scope: "long_term", Category: "projects", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "low", Confidence: 0.95, Importance: 4,
		ClaimSlot: "project.atlas_language", ClaimValue: "go",
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	source := memory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}
	jobID, err := store.EnqueueFormationJob(context.Background(), source, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.ID != jobID || job.Purpose != memory.FormationPurposeBackgroundPattern {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteFormationJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}

	if err := service.process(context.Background(), &job); err == nil {
		t.Fatal("completed formation lease replay succeeded")
	}
	memories, err = store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("replay memories=%+v err=%v", memories, err)
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls=%d want=1 with persisted artifact", extractor.calls)
	}
}

func TestServicePatternArtifactIsStableAndSpoofedSourceRejectsWholePattern(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	var output bytes.Buffer
	logger := config.NewLogger(config.LevelDebug)
	logger.SetOutput(&output)
	first := formationTestTurn(t, store, "I keep making review notes concise.", "pattern-1")
	second := formationTestTurn(t, store, "I repeatedly make review summaries concise.", "pattern-2")
	extractor := &fakePatternExtractor{patterns: memory.MemoryPatternBatch{Patterns: []memory.MemoryPattern{{
		Statement: "The user may favor concise review communication.", Category: "communication_preferences",
		ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: 0.65,
		Observations: []memory.PatternObservation{
			{SourceTurnID: first, Evidence: "I keep making review notes concise."},
			{SourceTurnID: second, Evidence: "I repeatedly make review summaries concise."},
			{SourceTurnID: second + 1000, Evidence: "spoofed"},
		},
	}}}}
	service := NewService(store, extractor, "model", logger)
	jobID, created, err := store.EnqueuePatternFormationJob(context.Background(), memory.FormationSource{RequestID: "pattern-2", SessionID: "session", SessionGeneration: 1, TurnID: second, Model: "model"}, "user-1")
	if err != nil || !created {
		t.Fatalf("enqueue id=%d created=%v err=%v", jobID, created, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.FormationJobArtifact(context.Background(), job)
	if err != nil || artifact == "" {
		t.Fatalf("artifact=%q err=%v", artifact, err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if extractor.patternCalls != 1 || len(extractor.turnIDs) != 2 || extractor.turnIDs[0] != first || extractor.turnIDs[1] != second {
		t.Fatalf("calls=%d turn_ids=%v", extractor.patternCalls, extractor.turnIDs)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
	var candidates int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user-1'`).Scan(&candidates); err != nil || candidates != 0 {
		t.Fatalf("spoofed pattern left candidate fragments: count=%d err=%v", candidates, err)
	}
	if !strings.Contains(output.String(), `"event":"user_memory.formation.pattern.rejected"`) || !strings.Contains(output.String(), `"reason_code":"invalid_source_turn"`) || strings.Contains(output.String(), "spoofed") {
		t.Fatalf("pattern rejection observability=%q", output.String())
	}
}

func TestServicePatternAggregatesNormalizedClaimIdentity(t *testing.T) {
	store := formationTestStore(t)
	first := formationTestTurn(t, store, "I keep making review notes concise.", "normalized-pattern-1")
	second := formationTestTurn(t, store, "I repeatedly make review summaries concise.", "normalized-pattern-2")
	extractor := &fakePatternExtractor{patterns: memory.MemoryPatternBatch{Patterns: []memory.MemoryPattern{{
		Statement: "The user likely favors concise review communication.", Category: "communication_preferences",
		ClaimSlot: " Communication.Review Style ", ClaimValue: " Concise Answers ", Sensitivity: "low", Confidence: 0.8,
		Observations: []memory.PatternObservation{
			{SourceTurnID: first, Evidence: "I keep making review notes concise."},
			{SourceTurnID: second, Evidence: "I repeatedly make review summaries concise."},
		},
	}}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	if _, created, err := store.EnqueuePatternFormationJob(context.Background(), memory.FormationSource{RequestID: "normalized-pattern-2", SessionID: "session", SessionGeneration: 1, TurnID: second, Model: "model"}, "user-1"); err != nil || !created {
		t.Fatalf("enqueue created=%v err=%v", created, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
	if memories[0].ClaimSlot != "communication.review_style" || memories[0].ClaimValue != "concise_answers" || memories[0].EvidenceCount != 2 {
		t.Fatalf("normalized memory=%+v", memories[0])
	}
}

func TestServicePatternSuccessCannotResurrectResetSession(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			ctx := context.Background()
			store, db := formationTestStoreWithDB(t)
			first := formationTestTurn(t, store, "I keep reviews concise.", "first")
			second := formationTestTurn(t, store, "I still keep reviews concise.", "second")
			extractor := &fakePatternExtractor{patterns: memory.MemoryPatternBatch{Patterns: []memory.MemoryPattern{{
				Statement: "The user likely favors concise reviews.", Category: "communication_preferences",
				ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: 0.8,
				Observations: []memory.PatternObservation{{SourceTurnID: first, Evidence: "I keep reviews concise."}, {SourceTurnID: second, Evidence: "I still keep reviews concise."}},
			}}}}
			service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
			jobID, created, err := store.EnqueuePatternFormationJob(ctx, memory.FormationSource{RequestID: "second", SessionID: "session", SessionGeneration: 1, TurnID: second, Model: "model"}, "user-1")
			if err != nil || !created {
				t.Fatalf("enqueue created=%t err=%v", created, err)
			}
			var callbackErr error
			var state string
			var submissions int
			var profile memory.SessionProfile
			extractor.preempt = func() {
				callbackErr = db.SQL().QueryRow(`SELECT state, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &submissions)
				if callbackErr == nil && reset {
					profile, callbackErr = store.ResetSession(ctx, "user-1", "session", time.Hour)
				}
			}
			// Reset inside the accepted call, then return the same valid successful result.
			service.drain(ctx)
			service.drain(ctx)
			if callbackErr != nil || state != "running" || submissions != 1 || extractor.patternCalls != 1 || extractor.calls != 0 {
				t.Fatalf("accepted pattern call: state=%q submissions=%d pattern_calls=%d legacy_calls=%d err=%v", state, submissions, extractor.patternCalls, extractor.calls, callbackErr)
			}
			if reset && profile.Generation != 2 {
				t.Fatalf("reset generation=%d", profile.Generation)
			}
			want := 1
			if reset {
				want = 0
			}
			for _, query := range []string{
				`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'memory_formation'`,
				`SELECT COUNT(*) FROM memory_entries`,
			} {
				var count int
				if err := db.SQL().QueryRow(query).Scan(&count); err != nil || count != want {
					t.Fatalf("%s: count=%d want=%d err=%v", query, count, want, err)
				}
			}
			if reset {
				var count int
				if err := db.SQL().QueryRow(`SELECT (SELECT COUNT(*) FROM memory_candidates) + (SELECT COUNT(*) FROM session_turns) + (SELECT COUNT(*) FROM session_summaries)`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("reset artifacts=%d err=%v", count, err)
				}
			} else if state, err := store.FormationJobState(ctx, "user-1", jobID); err != nil || state != "succeeded" {
				t.Fatalf("control state=%q err=%v", state, err)
			}
		})
	}
}

func TestServicePatternRequiresNewAnchorEvidence(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	first := formationTestTurn(t, store, "Pancakes sound good.", "pattern-1")
	second := formationTestTurn(t, store, "I could eat pancakes today.", "pattern-2")
	third := formationTestTurn(t, store, "What time is it?", "pattern-3")
	extractor := &fakePatternExtractor{patterns: memory.MemoryPatternBatch{Patterns: []memory.MemoryPattern{{
		Statement: "The user likely likes pancakes.", Category: "durable_preferences",
		ClaimSlot: "preference.food.pancakes", ClaimValue: "likes", Sensitivity: "low", Confidence: 0.8,
		Observations: []memory.PatternObservation{{SourceTurnID: first, Evidence: "Pancakes sound good."}, {SourceTurnID: second, Evidence: "I could eat pancakes today."}},
	}}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	_, created, err := store.EnqueuePatternFormationJob(context.Background(), memory.FormationSource{RequestID: "pattern-3", SessionID: "session", SessionGeneration: 1, TurnID: third, Model: "model"}, "user-1")
	if err != nil || !created {
		t.Fatalf("enqueue created=%v err=%v", created, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("stale pattern reassessment created memory: %+v err=%v", memories, err)
	}
	var candidates int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user-1'`).Scan(&candidates); err != nil || candidates != 0 {
		t.Fatalf("stale pattern reassessment left evidence: count=%d err=%v", candidates, err)
	}
}

func TestServiceEnqueueCreatesIndependentAgentSaveAndBackgroundJobs(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestForegroundTurn(t, store, "I prefer concise replies.", "foreground")
	extractor := &fakeExtractor{}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	source := memory.FormationSource{RequestID: "foreground", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}
	if err := service.Enqueue(context.Background(), "user-1", source); err != nil {
		t.Fatal(err)
	}

	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.Purpose != memory.FormationPurposeAgentSave || job.ExtractorVersion != memory.AgentSaveExtractorVersion {
		t.Fatalf("agent-save job=%+v err=%v", job, err)
	}
	artifact, err := store.SessionTurnForegroundMemory(context.Background(), "user-1", turnID)
	if err != nil || len(artifact.Candidates) != 1 {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
	if output, evaluateErr := artifact.Candidates[0].Evaluate("I prefer concise replies."); evaluateErr != nil || output.Approval != policy.ApprovalApproved {
		t.Fatalf("reevaluated output=%+v err=%v", output, evaluateErr)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 0 {
		t.Fatalf("agent-save processing made %d extractor calls", extractor.calls)
	}
	var submissions int
	if err := db.SQL().QueryRow(`SELECT model_submission_count FROM durable_jobs WHERE id = ?`, job.ID).Scan(&submissions); err != nil || submissions != 0 {
		t.Fatalf("agent-save submissions=%d err=%v", submissions, err)
	}
	if err := store.CompleteFormationJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 || memories[0].Statement != "The user prefers concise replies." {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}

	background, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || background.Purpose != memory.FormationPurposeBackgroundPattern || background.ExtractorVersion != memory.FormationExtractorVersion {
		t.Fatalf("background job=%+v err=%v", background, err)
	}
}

func TestServiceAgentSavePassesValidatedReinforcementTarget(t *testing.T) {
	store := formationTestStore(t)
	baseOutput, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: "I prefer concise replies.", Statement: "The user likes concise replies.", Evidence: "I prefer concise replies.",
		Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect,
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave,
		Scope: policy.ScopeLongTerm, Category: policy.CategoryCommunicationPreferences,
		Context: policy.ContextDirectAssertion, Confidence: 0.6, Importance: 3,
		ClaimSlot: "communication.reply_style", ClaimValue: "concise",
	})
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := store.ProposeCandidate(context.Background(), "user-1", memory.CandidateProposal{Output: baseOutput, IdempotencyKey: "target-base"})
	if err != nil {
		t.Fatal(err)
	}
	turnID := formationTestForegroundTurn(t, store, "I prefer concise replies.", "target-save", base.PublishedMemoryID)
	service := NewService(store, &fakeExtractor{}, "model", config.NewLogger(config.LevelError))
	if err := service.Enqueue(context.Background(), "user-1", memory.FormationSource{RequestID: "target-save", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.Purpose != memory.FormationPurposeAgentSave {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	memory, err := store.EntryByID(base.PublishedMemoryID)
	if err != nil || memory.Statement != "The user prefers concise replies." || memory.Confidence != 1 || memory.EvidenceCount != 2 {
		t.Fatalf("reinforced memory=%+v err=%v", memory, err)
	}
}

func TestServiceEnqueueOmitsAgentSaveJobForEmptyArtifact(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Nothing durable here", "empty-foreground")
	service := NewService(store, &fakeExtractor{}, "model", config.NewLogger(config.LevelError))
	if err := service.Enqueue(context.Background(), "user-1", memory.FormationSource{RequestID: "empty-foreground", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.Purpose != memory.FormationPurposeBackgroundPattern {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if _, err := store.ClaimFormationJob(context.Background(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected second job: %v", err)
	}
}

func TestAgentSaveReconciliationIsIdempotentAcrossRestart(t *testing.T) {
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
	turnID := formationTestForegroundTurn(t, store, "I prefer concise replies.", "restart-agent-save")
	if err := store.MarkFormationEligible(context.Background(), "user-1", turnID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := memorytest.NewStore(t, path, log)
	t.Cleanup(func() { reopened.Close() })
	count, err := reopened.ReconcileAgentSaveFormationJobs(context.Background(), "model")
	if err != nil || count != 1 {
		t.Fatalf("first reconciliation count=%d err=%v", count, err)
	}
	count, err = reopened.ReconcileAgentSaveFormationJobs(context.Background(), "model")
	if err != nil || count != 0 {
		t.Fatalf("second reconciliation count=%d err=%v", count, err)
	}
	job, err := reopened.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.TurnID != turnID || job.Purpose != memory.FormationPurposeAgentSave {
		t.Fatalf("reconciled job=%+v err=%v", job, err)
	}
}

func TestServicePublishesPartialDirectNameIntoNewSessionProfile(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Before we continue, my name is Ada. What should we build?", "name")
	extractor := &fakeExtractor{candidates: []memory.MemorySaveItem{{
		Statement: "The user's name is Ada.", Evidence: "my name is Ada.",
		Scope: "long_term", Category: "identity", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "identity_or_contact", Confidence: 0.95, Importance: 1,
		ClaimSlot: "identity.name", ClaimValue: "ada",
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	_, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "name", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "identity", 10)
	if err != nil || len(memories) != 1 || memories[0].Importance != 3 {
		t.Fatalf("identity memories=%+v err=%v", memories, err)
	}
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "new-session", time.Hour)
	if err != nil || !strings.Contains(profile.Content, "The user's name is Ada.") {
		t.Fatalf("profile=%q err=%v", profile.Content, err)
	}
}

func TestServicePublishesIndependentFactsFromLongTurn(t *testing.T) {
	store := formationTestStore(t)
	text := "My name is Ada. I use Fedora on my workstation. I prefer concise replies."
	turnID := formationTestTurn(t, store, text, "long")
	extractor := &fakeExtractor{candidates: []memory.MemorySaveItem{
		{Statement: "The user's name is Ada.", Evidence: "My name is Ada.", Scope: "long_term", Category: "identity", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "identity_or_contact", Confidence: 0.95, Importance: 3, ClaimSlot: "identity.name", ClaimValue: "Ada"},
		{Statement: "The user uses Fedora on their workstation.", Evidence: "I use Fedora on my workstation.", Scope: "long_term", Category: "environment", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.95, Importance: 4, ClaimSlot: "environment.workstation_os", ClaimValue: "Fedora"},
		{Statement: "The user prefers concise replies.", Evidence: "I prefer concise replies.", Scope: "long_term", Category: "communication_preferences", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "communication.reply_style", ClaimValue: "concise"},
	}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	if _, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "long", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 3 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServicePublishesPacmanInferenceAsUncertainMemory(t *testing.T) {
	store := formationTestStore(t)
	text := "Considering pacman packages for file management."
	turnID := formationTestTurn(t, store, text, "pacman")
	extractor := &fakeExtractor{candidates: []memory.MemorySaveItem{{
		Statement: "The user may use or be evaluating a pacman-based Arch-family Linux environment.", Evidence: text,
		Scope: "long_term", Category: "environment", Context: "direct_assertion",
		Provenance: "model_inference", Sensitivity: "low", Confidence: 0.55, Importance: 3,
		ClaimSlot: "environment.linux_distribution", ClaimValue: "arch_family",
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	source := memory.FormationSource{RequestID: "pacman", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}
	if _, err := store.EnqueueFormationJob(context.Background(), source, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
	memory := memories[0]
	if memory.Confidence != 0.55 || memory.ProvenanceType != "model_inference" || memory.SourceAuthority != "model" || memory.ClaimSlot != "environment.linux_distribution" || memory.ClaimValue != "arch_family" || memory.EvidenceCount != 1 {
		t.Fatalf("uncertain memory=%+v", memory)
	}
}

func TestServiceCompletesBlockedConflictWithoutPublicationRetry(t *testing.T) {
	store := formationTestStore(t)
	activeOutput, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: "I prefer tea.", Statement: "The user prefers tea.", Evidence: "I prefer tea.",
		Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect,
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAutomaticExtraction,
		Scope: policy.ScopeLongTerm, Category: policy.CategoryDurablePreferences,
		Context: policy.ContextDirectAssertion, Confidence: 0.95, Importance: 4,
		ClaimSlot: "preference.drink", ClaimValue: "tea",
	})
	if err != nil {
		t.Fatal(err)
	}
	activeCandidate, _, err := store.ProposeCandidate(context.Background(), "user-1", memory.CandidateProposal{Output: activeOutput, IdempotencyKey: "active-tea"})
	if err != nil {
		t.Fatal(err)
	}
	if activeCandidate.PublishedMemoryID == 0 {
		t.Fatalf("active candidate was not atomically published: %+v", activeCandidate)
	}

	turnID := formationTestTurn(t, store, "I prefer coffee.", "blocked")
	extractor := &fakeExtractor{candidates: []memory.MemorySaveItem{{
		Statement: "The user prefers coffee.", Evidence: "I prefer coffee.", Scope: "long_term",
		Category: "durable_preferences", Context: "direct_assertion", Provenance: "user_statement",
		Sensitivity: "low", Confidence: 0.9, Importance: 4,
		ClaimSlot: "preference.drink", ClaimValue: "coffee",
	}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "blocked", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "succeeded" || extractor.calls != 1 {
		t.Fatalf("state=%q extractor_calls=%d err=%v", state, extractor.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 || memories[0].ClaimValue != "tea" {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServiceLeavesFailedJobRetryable(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go", "req")
	extractor := &fakeExtractor{err: errors.New("extractor offline")}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	_, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err == nil {
		t.Fatal("expected extraction failure")
	}
	if _, err := store.RetryFormationJob(context.Background(), job, "extractor", formationMaxAttempts); err != nil {
		t.Fatal(err)
	}
	state, err := store.FormationJobState(context.Background(), "user-1", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != "retry" {
		t.Fatalf("job state=%s", state)
	}
}

func TestServiceDefersExtractionWhileForegroundWorkIsBusy(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "busy")
	extractor := &fakeExtractor{}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(unavailableLowPriorityGate{})
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "busy", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || extractor.calls != 0 {
		t.Fatalf("state=%q calls=%d err=%v", state, extractor.calls, err)
	}
	var submissions int
	if err := db.SQL().QueryRow(`SELECT model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&submissions); err != nil || submissions != 0 {
		t.Fatalf("deferred submissions=%d err=%v", submissions, err)
	}
}

func TestServiceDiscardsSuccessfulExtractionAfterForegroundPreemption(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go", "preempt")
	extractor := &fakeExtractor{candidates: []memory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "Go"}}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(canceledLowPriorityGate{})
	if _, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "preempt", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); !errors.Is(err, errBackgroundPreempted) {
		t.Fatalf("process error=%v", err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("preempted memories=%+v err=%v", memories, err)
	}
}

func TestServiceRefundsFormationSubmissionBeforeProviderAcceptance(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "preaccept-preempt")
	extractor := &fakeExtractor{err: context.Canceled}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(canceledLowPriorityGate{})
	if _, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "preaccept-preempt", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); !errors.Is(err, errBackgroundPreempted) {
		t.Fatalf("process error=%v", err)
	}
	var submissions int
	if err := db.SQL().QueryRow(`SELECT model_submission_count FROM durable_jobs WHERE id = ?`, job.ID).Scan(&submissions); err != nil || submissions != 0 {
		t.Fatalf("pre-acceptance submissions=%d err=%v", submissions, err)
	}
}

func TestServiceForegroundPreemptionNeverConsumesFormationBudget(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "started-preempt")
	gate := &preemptibleLowPriorityGate{}
	extractor := &fakeExtractor{preempt: func() { gate.cancel() }, err: fmt.Errorf("provider stream canceled: %w", context.Canceled)}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(gate)
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "started-preempt", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
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
		makeFormationJobReady(t, db, jobID)
	}
}

func TestServiceForegroundPreemptionRefundsPatternSubmission(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	_ = formationTestTurn(t, store, "I keep reviews concise.", "pattern-preempt-1")
	anchorID := formationTestTurn(t, store, "I still keep reviews concise.", "pattern-preempt-2")
	gate := &preemptibleLowPriorityGate{}
	extractor := &fakePatternExtractor{patternErr: fmt.Errorf("provider stream canceled: %w", context.Canceled)}
	extractor.preempt = func() { gate.cancel() }
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	service.SetLowPriorityGate(gate)
	jobID, created, err := store.EnqueuePatternFormationJob(context.Background(), memory.FormationSource{RequestID: "pattern-preempt-2", SessionID: "session", SessionGeneration: 1, TurnID: anchorID, Model: "model"}, "user-1")
	if err != nil || !created {
		t.Fatalf("enqueue id=%d created=%v err=%v", jobID, created, err)
	}

	service.drain(context.Background())
	var state, code string
	var attempts, submissions int
	if err := db.SQL().QueryRow(`SELECT state, attempt_count, last_error_code, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&state, &attempts, &code, &submissions); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attempts != 0 || submissions != 0 || code != "foreground_preempted" || extractor.patternCalls != 1 {
		t.Fatalf("state=%q attempts=%d submissions=%d code=%q calls=%d", state, attempts, submissions, code, extractor.patternCalls)
	}
}

func TestServiceRetriesAllMalformedCandidatesOnceThenSkips(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "You actually are on v3.2.0 not 1.0", "req")
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"The AI is running version 3.2.0.","evidence":"You actually are on v3.2.0 not 1.0","scope":"short_term","category":"software_version","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":0.4,"ttl_days":7,"supersedes_statement":null}]}`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
	makeFormationJobReady(t, db, jobID)
	service.drain(context.Background())
	state, err = store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "skipped" || client.calls != 2 {
		t.Fatalf("terminal state=%q calls=%d err=%v", state, client.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServicePublishesWhenInvalidOutputRetryRecovers(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "retry-recovers")
	extractor := &fakeExtractor{err: &extraction.InvalidOutputError{Code: "missing_tool_call"}}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "retry-recovers", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || extractor.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, extractor.calls, err)
	}
	extractor.err = nil
	extractor.candidates = []memory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "go"}}
	makeFormationJobReady(t, db, jobID)
	service.drain(context.Background())
	state, err = store.FormationJobState(context.Background(), "user-1", jobID)
	memories, listErr := store.ListMemories("user-1", "", "", 10)
	if err != nil || listErr != nil || state != "succeeded" || extractor.calls != 2 || extractor.lastErrorCode != "missing_tool_call" || len(memories) != 1 {
		t.Fatalf("state=%q calls=%d memories=%+v state_err=%v list_err=%v", state, extractor.calls, memories, err, listErr)
	}
}

func TestServiceStructuredRetrySharesSubmissionBudgetAndKeepsReason(t *testing.T) {
	valid := []memory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "go"}}
	for _, test := range []struct {
		name   string
		errors []error
	}{
		{name: "transient then invalid", errors: []error{errors.New("provider unavailable"), &extraction.InvalidOutputError{Code: "missing_tool_call"}, nil}},
		{name: "invalid then transient", errors: []error{&extraction.InvalidOutputError{Code: "missing_tool_call"}, errors.New("provider unavailable"), nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, db := formationTestStoreWithDB(t)
			turnID := formationTestTurn(t, store, "I use Go", test.name)
			extractor := &fakeExtractor{}
			service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
			jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: test.name, SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
			if err != nil {
				t.Fatal(err)
			}
			for index, extractErr := range test.errors {
				extractor.err = extractErr
				extractor.candidates = nil
				if extractErr == nil {
					extractor.candidates = valid
				}
				service.drain(context.Background())
				if index < len(test.errors)-1 {
					state, err := store.FormationJobState(context.Background(), "user-1", jobID)
					if err != nil || state != "retry" {
						t.Fatalf("step=%d state=%q err=%v", index, state, err)
					}
					makeFormationJobReady(t, db, jobID)
				}
			}
			state, err := store.FormationJobState(context.Background(), "user-1", jobID)
			memories, listErr := store.ListMemories("user-1", "", "", 10)
			var attemptCount, invalidRetryCount, submissionCount int
			counterErr := db.SQL().QueryRow(`SELECT attempt_count, invalid_output_retry_count, model_submission_count FROM durable_jobs WHERE id = ?`, jobID).Scan(&attemptCount, &invalidRetryCount, &submissionCount)
			if err != nil || listErr != nil || counterErr != nil || state != "succeeded" || extractor.calls != 3 || extractor.lastErrorCode != "missing_tool_call" || len(memories) != 1 || attemptCount != 3 || invalidRetryCount != 1 || submissionCount != 3 {
				t.Fatalf("state=%q calls=%d memories=%+v attempts=%d invalid_retries=%d submissions=%d state_err=%v list_err=%v counter_err=%v", state, extractor.calls, memories, attemptCount, invalidRetryCount, submissionCount, err, listErr, counterErr)
			}
		})
	}
}

func TestServiceFormationProviderCallsHaveAbsoluteBound(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "submission-bound")
	extractor := &fakeExtractor{err: errors.New("provider unavailable")}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "submission-bound", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model"}, "user-1")
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
		if state == "dead" {
			if submissions != memory.DurableModelSubmissionLimit || extractor.calls != memory.DurableModelSubmissionLimit {
				t.Fatalf("submissions=%d calls=%d", submissions, extractor.calls)
			}
			return
		}
		if state != "retry" {
			t.Fatalf("unexpected state=%q", state)
		}
		makeFormationJobReady(t, db, jobID)
	}
}

func TestServiceDoesNotRetryValidEmptyBatch(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Nothing durable here", "empty")
	extractor := &fakeExtractor{}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "empty", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "succeeded" || extractor.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, extractor.calls, err)
	}
}

func TestServiceRetriesCandidateMissingCategory(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "My name is Jonah", "missing-category")
	client := &fakeExtractionChatter{content: `{"memories":[{"statement":"The user's name is Jonah.","evidence":"My name is Jonah","scope":"long_term","context":"direct_assertion","provenance":"user_statement","sensitivity":"identity_or_contact","confidence":1,"importance":5,"ttl_days":0,"supersedes":"","claim_slot":"identity.name","claim_value":"Jonah"}]}`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "missing-category", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServiceDoesNotRetryPolicyRejectedCandidate(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "My name is Jonah", "rejected")
	client := &fakeExtractionChatter{content: `{"candidates":[{"statement":"The user's name is Jonah.","evidence":"My name is Jonah","scope":"long_term","category":"identity","context":"direct_assertion","provenance":"user_statement","sensitivity":"identity_or_contact","confidence":1,"importance":3,"ttl_days":0,"supersedes_statement":"","claim_slot":"identity_name","claim_value":"jonah"}]}`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "rejected", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "succeeded" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServiceDropsMalformedPersistedArtifactBesideValidCandidate(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go.", "mixed-artifact")
	source := memory.FormationSource{RequestID: "mixed-artifact", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}
	if _, err := store.EnqueueFormationJob(context.Background(), source, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	artifact := `{"memories":[{"statement":"Malformed sibling.","evidence":"I use Go.","scope":"long_term","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes":"","claim_slot":"project.language","claim_value":"go"},{"statement":"The user uses Go.","evidence":"I use Go.","scope":"long_term","category":"projects","context":"direct_assertion","provenance":"user_statement","sensitivity":"low","confidence":0.9,"importance":4,"ttl_days":0,"supersedes":"","claim_slot":"project.language","claim_value":"go"}]}`
	if err := store.SaveFormationJobArtifact(context.Background(), job, artifact); err != nil {
		t.Fatal(err)
	}
	if err := NewService(store, nil, "model", config.NewLogger(config.LevelError)).process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 || memories[0].ClaimSlot != "project.language" || memories[0].ClaimValue != "go" || memories[0].EvidenceCount != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestServicePersistsAndReplaysExtractionCounts(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go.", "artifact-counts")
	extractor := &fakeExtractor{candidates: []memory.MemorySaveItem{{Statement: "The user uses Go.", Evidence: "I use Go.", Scope: "long_term", Category: "projects", Context: "direct_assertion", Provenance: "user_statement", Sensitivity: "low", Confidence: 0.9, Importance: 4, ClaimSlot: "project.language", ClaimValue: "go"}}, submitted: 2, malformed: 1}
	service := NewService(store, extractor, "model", config.NewLogger(config.LevelError))
	if _, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "artifact-counts", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.FormationJobArtifact(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	batch, itemErrors, err := memory.DecodeMemorySaveBatchJSON([]byte(artifact))
	if err != nil || len(itemErrors) != 0 || batch.SubmittedCount != 2 || batch.MalformedCount != 1 || len(batch.Memories) != 1 {
		t.Fatalf("artifact=%s batch=%+v item_errors=%+v err=%v", artifact, batch, itemErrors, err)
	}
	if err := service.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls=%d want 1", extractor.calls)
	}
}

func TestServiceRetriesMalformedOuterArguments(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Nothing to retain", "req")
	client := &fakeExtractionChatter{content: `[{"statement":"wrong top-level shape"}]`}
	service := NewService(store, newTestLLMExtractor(t, client), "model", config.NewLogger(config.LevelError))
	jobID, err := store.EnqueueFormationJob(context.Background(), memory.FormationSource{RequestID: "req", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	service.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" || client.calls != 1 {
		t.Fatalf("state=%q calls=%d err=%v", state, client.calls, err)
	}
}

type fakeExtractionChatter struct {
	content string
	err     error
	request llm.ChatRequest
	calls   int
}

func (f *fakeExtractionChatter) Chat(_ context.Context, request llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return nil, f.err
	}
	arguments := map[string]interface{}{}
	var artifact map[string]interface{}
	if err := json.Unmarshal([]byte(f.content), &artifact); err != nil {
		arguments["_raw"] = f.content
	} else if candidates, ok := artifact["candidates"].([]interface{}); ok {
		for _, raw := range candidates {
			if candidate, ok := raw.(map[string]interface{}); ok {
				if value, exists := candidate["supersedes_statement"]; exists {
					candidate["supersedes"] = value
					delete(candidate, "supersedes_statement")
				}
			}
		}
		arguments["memories"] = candidates
	} else {
		arguments = artifact
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: "user_memory_save", Arguments: arguments}}}}}, nil
}

func newTestLLMExtractor(t *testing.T, client llm.Chatter) *extraction.LLMExtractor {
	t.Helper()
	extractor, err := extraction.NewLLMExtractor(client, "model", 8192)
	if err != nil {
		t.Fatal(err)
	}
	return extractor
}

func formationTestStore(t *testing.T) *memory.Store {
	t.Helper()
	store, _ := formationTestStoreWithDB(t)
	return store
}

func formationTestStoreWithDB(t *testing.T) (*memory.Store, *database.DB) {
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

func makeFormationJobReady(t *testing.T, db *database.DB, jobID int64) {
	t.Helper()
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
}

func formationTestTurn(t *testing.T, store *memory.Store, text, requestID string) int64 {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: requestID})
	turn, err := memorytest.AppendPendingTurn(ctx, store, "session", "user-1", profile.Generation, text, "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(context.Background(), "user-1", turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}

func formationTestForegroundTurn(t *testing.T, store *memory.Store, text, requestID string, targetMemoryID ...int64) int64 {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	output, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: text, Statement: "The user prefers concise replies.", Evidence: text,
		Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect,
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave,
		Scope: policy.ScopeLongTerm, Category: policy.CategoryCommunicationPreferences,
		Context: policy.ContextDirectAssertion, Confidence: 1, Importance: 3,
		ClaimSlot: "communication.reply_style", ClaimValue: "concise",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: requestID})
	var target int64
	if len(targetMemoryID) > 0 {
		target = targetMemoryID[0]
	}
	turn, err := store.AppendPendingSessionTurn(
		ctx, memory.SessionTurnWrite{SessionID: "session", UserID: "user-1", Generation: profile.Generation, UserText: text, AssistantText: "answer", ToolNames: nil, History: memory.EmptyToolHistory(), Staged: []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user-1", Candidate: output, TargetMemoryID: target}}, TTL: time.Hour, Pressure: memory.SessionPromptPressure{Tokens: 10, Limit: 100, Version: "test"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return turn.ID
}
