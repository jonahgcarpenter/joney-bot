package formation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/extraction"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

type fakeAssessmentExtractor struct {
	fakePatternExtractor
	batch           memory.AssessmentBatch
	err             error
	assessmentCalls int
	inputs          []memory.AssessmentInput
	codes           []string
	onExtract       func(context.Context)
}

type projectionChatter struct {
	calls int
	items []memory.ForegroundMemoryCandidate
}

func (c *projectionChatter) Chat(_ context.Context, _ llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	c.calls++
	items := c.items
	if items == nil {
		items = []memory.ForegroundMemoryCandidate{}
	}
	return &llm.ChatResponse{Message: llm.ChatMessage{ToolCalls: []llm.ToolCall{{Function: llm.ToolFunction{Name: "user_memory_assess", Arguments: map[string]interface{}{"items": items}}}}}}, nil
}

func TestAssessmentOmittedEvidenceIsRetriedWithoutSavingArtifact(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, strings.Repeat("x", 5000)+" I use Go", "assessment")
	id := enqueueTestAssessment(t, store, turnID)
	client := &projectionChatter{items: []memory.ForegroundMemoryCandidate{assessmentTestCandidate()}}
	extractor, err := extraction.NewLLMExtractor(client, "model", 8192)
	if err != nil {
		t.Fatal(err)
	}
	NewService(store, extractor, "model", config.NewLogger(config.LevelError)).drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", id)
	if err != nil || state != "retry" || client.calls != 1 {
		t.Fatalf("state=%s calls=%d err=%v", state, client.calls, err)
	}
	var payload string
	if err := db.SQL().QueryRow(`SELECT extraction_payload FROM durable_jobs WHERE id=?`, id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != "" {
		t.Fatalf("saved output from omitted evidence: %s", payload)
	}
}

func TestAssessmentProjectionMetricsReachCompletionWithoutPayload(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		store := formationTestStore(t)
		turnID := formationTestTurn(t, store, strings.Repeat("x", 5000)+"private-projection-canary", "assessment")
		enqueueTestAssessment(t, store, turnID)
		var output bytes.Buffer
		log := config.NewLogger(level)
		log.SetOutput(&output)
		client := &projectionChatter{}
		extractor, err := extraction.NewLLMExtractor(client, "model", 8192)
		if err != nil {
			t.Fatal(err)
		}
		NewService(store, extractor, "model", log).drain(context.Background())
		projected, completed := 0, 0
		for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			switch record["event"] {
			case "user_memory.formation.input.projected":
				projected++
			case "user_memory.formation.job.complete":
				completed++
			default:
				continue
			}
			if record["level"] != "info" || record["request_id"] != "assessment" || record["operation_id"] == nil {
				t.Fatalf("missing projection scope: %s", line)
			}
			if tokens, ok := record["input_tokens"].(float64); !ok || tokens <= 0 || tokens > 6000 {
				t.Fatalf("invalid token estimate: %s", line)
			}
			if omitted, ok := record["omitted_text_chars"].(float64); !ok || omitted <= 0 {
				t.Fatalf("missing omissions: %s", line)
			}
		}
		if projected != 1 || completed != 1 || client.calls != 1 || strings.Contains(output.String(), "private-projection-canary") {
			t.Fatalf("projection=%d completion=%d calls=%d logs=%s", projected, completed, client.calls, output.String())
		}
	}
}

func (f *fakeAssessmentExtractor) ExtractAssessment(ctx context.Context, input memory.AssessmentInput, code string) (memory.AssessmentBatch, error) {
	f.assessmentCalls++
	f.inputs = append(f.inputs, input)
	f.codes = append(f.codes, code)
	if f.onExtract != nil {
		f.onExtract(ctx)
	}
	return f.batch, f.err
}

func assessmentTestCandidate() memory.ForegroundMemoryCandidate {
	return memory.ForegroundMemoryCandidate{Statement: "The user uses Go.", Evidence: "I use Go", Category: "projects", ClaimSlot: "project.language", ClaimValue: "go", EvidenceType: "direct_statement", Provenance: "user_statement", Confidence: .95, Retention: "durable", Intent: "automatic", Context: "direct_assertion", Cardinality: "multiple"}
}

func enqueueTestAssessment(t *testing.T, store *memory.Store, turnID int64) int64 {
	t.Helper()
	id, created, err := store.EnqueueAssessmentFormationJob(context.Background(), memory.FormationSource{RequestID: "assessment", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model"}, "user-1")
	if err != nil || !created {
		t.Fatalf("enqueue id=%d created=%v err=%v", id, created, err)
	}
	return id
}

func TestAssessmentDispatchFirstTurnRecoveryAndReplay(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "Remember: I use Go for work.", "assessment")
	f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{assessmentTestCandidate()}}}
	s := NewService(store, f, "model", config.NewLogger(config.LevelError))
	if err := s.Enqueue(context.Background(), "user-1", memory.FormationSource{RequestID: "assessment", SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model"}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.ExtractorVersion != memory.AssessmentExtractorVersion {
		t.Fatalf("wrong dispatch: %+v", job)
	}
	if err := s.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if err := s.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if f.assessmentCalls != 1 || f.patternCalls != 0 || f.calls != 0 {
		t.Fatalf("assessment=%d pattern=%d legacy=%d", f.assessmentCalls, f.patternCalls, f.calls)
	}
	if len(f.inputs[0].Context) != 0 || f.inputs[0].Anchor.ID != turnID {
		t.Fatalf("first-turn input=%+v", f.inputs)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
	if job.ModelSubmissionCount != 1 {
		t.Fatalf("submissions=%d", job.ModelSubmissionCount)
	}
}

func TestAssessmentTemporaryObservationWithoutCanonicalPublication(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go for today's task.", "assessment")
	enqueueTestAssessment(t, store, turnID)
	c := assessmentTestCandidate()
	c.Retention = "observation"
	c.TTLDays = 7
	f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{c}}}
	s := NewService(store, f, "model", config.NewLogger(config.LevelError))
	s.drain(context.Background())
	observations, err := store.ListMemoryObservations(context.Background(), "user-1", time.Now())
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations=%+v err=%v", observations, err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("temporary observation published: %+v err=%v", memories, err)
	}
}

func TestAssessmentWorkerUsesCrossSessionObservation(t *testing.T) {
	store := formationTestStore(t)
	first := formationTestTurn(t, store, "I use Go for today's task.", "assessment")
	enqueueTestAssessment(t, store, first)
	c := assessmentTestCandidate()
	c.Retention, c.TTLDays = "observation", 7
	f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{c}}}
	s := NewService(store, f, "model", config.NewLogger(config.LevelError))
	s.drain(context.Background())
	observations, err := store.ListMemoryObservations(context.Background(), "user-1", time.Now())
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations=%+v err=%v", observations, err)
	}
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "other-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := memorytest.AppendPendingTurn(context.Background(), store, "other-session", "user-1", profile.Generation, "I use Go again today.", "context-only answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(context.Background(), "user-1", turn.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.EnqueueAssessmentFormationJob(context.Background(), memory.FormationSource{SessionID: "other-session", SessionGeneration: profile.Generation, TurnID: turn.ID, Model: "model"}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	c = assessmentTestCandidate()
	c.EvidenceType, c.Provenance, c.Confidence = "model_inference", "model_inference", .8
	c.SourceObservationIDs = []int64{observations[0].ID}
	f.batch.Items = []memory.ForegroundMemoryCandidate{c}
	s.drain(context.Background())
	if f.assessmentCalls != 2 || len(f.inputs[1].Context) != 0 || len(f.inputs[1].Observations) != 1 || f.inputs[1].Observations[0].SourceTurnID != first {
		t.Fatalf("missing cross-session frozen evidence: %+v", f.inputs)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestAssessmentInvalidOutputRetryUsesFrozenInput(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "assessment")
	id := enqueueTestAssessment(t, store, turnID)
	c := assessmentTestCandidate()
	c.SourceObservationIDs = []int64{999}
	f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{c}}}
	s := NewService(store, f, "model", config.NewLogger(config.LevelError))
	s.drain(context.Background())
	state, err := store.FormationJobState(context.Background(), "user-1", id)
	if err != nil || state != "retry" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	var payload string
	if err := db.SQL().QueryRow(`SELECT extraction_payload FROM durable_jobs WHERE id=?`, id).Scan(&payload); err != nil || payload != "" {
		t.Fatalf("invalid output persisted=%q err=%v", payload, err)
	}
	formationTestTurn(t, store, "A later unrelated turn", "later")
	f.batch = memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{}}
	makeFormationJobReady(t, db, id)
	s.drain(context.Background())
	if f.assessmentCalls != 2 || !reflect.DeepEqual(f.inputs[0], f.inputs[1]) || f.codes[1] != "invalid_assessment" {
		t.Fatalf("calls=%d codes=%v input changed=%v", f.assessmentCalls, f.codes, !reflect.DeepEqual(f.inputs[0], f.inputs[1]))
	}
	state, err = store.FormationJobState(context.Background(), "user-1", id)
	if err != nil || state != "succeeded" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestAssessmentSubmissionBudgetAndInvalidRetryLimit(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider budget", true: "invalid retry"}[invalid], func(t *testing.T) {
			store, db := formationTestStoreWithDB(t)
			turnID := formationTestTurn(t, store, "I use Go", "assessment")
			id := enqueueTestAssessment(t, store, turnID)
			f := &fakeAssessmentExtractor{err: errors.New("synthetic provider failure")}
			limit, wantState := memory.DurableModelSubmissionLimit, "dead"
			if invalid {
				f.err = &extraction.InvalidOutputError{Code: "invalid_assessment"}
				limit = 2
				wantState = "skipped"
			}
			s := NewService(store, f, "model", config.NewLogger(config.LevelError))
			for i := 0; i < limit; i++ {
				makeFormationJobReady(t, db, id)
				s.drain(context.Background())
			}
			state, err := store.FormationJobState(context.Background(), "user-1", id)
			if err != nil || state != wantState || f.assessmentCalls != limit {
				t.Fatalf("state=%s calls=%d err=%v", state, f.assessmentCalls, err)
			}
		})
	}
}

func TestAssessmentPreemptionRefundsRenewedLease(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	turnID := formationTestTurn(t, store, "I use Go", "assessment")
	id := enqueueTestAssessment(t, store, turnID)
	gate := &preemptibleLowPriorityGate{}
	f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{}}}
	s := NewService(store, f, "model", config.NewLogger(config.LevelError))
	s.jobLease = 90 * time.Millisecond
	s.SetLowPriorityGate(gate)
	job, err := store.ClaimFormationJob(context.Background(), s.jobLease)
	if err != nil {
		t.Fatal(err)
	}
	initial := job.LeaseUntil
	f.onExtract = func(ctx context.Context) {
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-deadline.C:
				t.Error("heartbeat did not renew")
				gate.cancel()
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				var raw string
				if err := db.SQL().QueryRow(`SELECT lease_until FROM durable_jobs WHERE id=?`, id).Scan(&raw); err != nil {
					t.Error(err)
					gate.cancel()
					return
				}
				until, err := time.Parse(time.RFC3339Nano, raw)
				if err == nil && until.After(initial) {
					gate.cancel()
					return
				}
			}
		}
	}
	if err := s.process(context.Background(), &job); !errors.Is(err, errBackgroundPreempted) {
		t.Fatalf("process err=%v", err)
	}
	if !job.LeaseUntil.After(initial) || job.ModelSubmissionCount != 0 {
		t.Fatalf("lease/count not renewed/refunded: %+v", job)
	}
	if err := store.DeferFormationJob(context.Background(), job, time.Second); err != nil {
		t.Fatal(err)
	}
	var count int
	var artifact string
	if err := db.SQL().QueryRow(`SELECT model_submission_count,extraction_payload FROM durable_jobs WHERE id=?`, id).Scan(&count, &artifact); err != nil {
		t.Fatal(err)
	}
	if count != 0 || artifact != "" {
		t.Fatalf("preempted count=%d artifact=%s", count, artifact)
	}
}

func TestAssessmentArtifactReplayNeedsNoExtractor(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestTurn(t, store, "I use Go", "assessment")
	enqueueTestAssessment(t, store, turnID)
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FormationAssessmentInput(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{assessmentTestCandidate()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFormationJobArtifact(context.Background(), job, string(data)); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, nil, "model", config.NewLogger(config.LevelError))
	if err := s.process(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.ModelSubmissionCount != 0 {
		t.Fatalf("replay submitted model: %d", job.ModelSubmissionCount)
	}
}

func TestAssessmentRevokedObservationRejectsItemAndCompletesReplay(t *testing.T) {
	for _, revoke := range []string{"delete", "expire"} {
		t.Run(revoke, func(t *testing.T) {
			store, db := formationTestStoreWithDB(t)
			first := formationTestTurn(t, store, "I use Go today", "assessment")
			enqueueTestAssessment(t, store, first)
			item := assessmentTestCandidate()
			item.Retention, item.TTLDays = "observation", 7
			f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{item}}}
			s := NewService(store, f, "model", config.NewLogger(config.LevelError))
			s.drain(context.Background())
			observations, err := store.ListMemoryObservations(context.Background(), "user-1", time.Now())
			if err != nil || len(observations) != 1 {
				t.Fatalf("observations=%+v err=%v", observations, err)
			}
			second := formationTestTurn(t, store, "I use Go again today", "assessment")
			enqueueTestAssessment(t, store, second)
			job, err := store.ClaimFormationJob(context.Background(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			item = assessmentTestCandidate()
			item.SourceObservationIDs = []int64{observations[0].ID}
			f.batch.Items = []memory.ForegroundMemoryCandidate{item}
			f.onExtract = func(context.Context) {
				if len(f.inputs[len(f.inputs)-1].Observations) != 1 {
					t.Fatal("observation was not frozen before revocation")
				}
				query := `DELETE FROM memory_observations WHERE id=?`
				if revoke == "expire" {
					query = `UPDATE memory_observations SET observed_at='1999-12-31T00:00:00Z', expires_at='2000-01-01T00:00:00Z' WHERE id=?`
				}
				if _, err := db.SQL().Exec(query, observations[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.process(context.Background(), &job); err != nil {
				t.Fatalf("revocation became retry error: %v", err)
			}
			rejected := false
			for _, field := range s.resultFields {
				if field.Key == "rejected_count" && field.Value == 1 {
					rejected = true
				}
			}
			if !rejected {
				t.Fatalf("missing rejected item result: %+v", s.resultFields)
			}
			if err := s.process(context.Background(), &job); err != nil {
				t.Fatalf("immutable replay failed: %v", err)
			}
			if err := store.CompleteFormationJob(context.Background(), job, false); err != nil {
				t.Fatal(err)
			}
			if f.assessmentCalls != 2 {
				t.Fatalf("replay invoked model: %d", f.assessmentCalls)
			}
			memories, err := store.ListMemories("user-1", "", "", 10)
			if err != nil || len(memories) != 0 {
				t.Fatalf("revoked evidence published: %+v err=%v", memories, err)
			}
		})
	}
}

func TestRetainedPatternReplayRejectsOverlongWholeTurnWithoutRetry(t *testing.T) {
	// Retained pattern-v1 artifacts require whole-turn evidence, but legacy policy
	// bounds evidence to 1000 runes. Replay keeps this rejection; new assessment-v1
	// jobs instead cite bounded exact spans and do not inherit that limitation.
	store := formationTestStore(t)
	text := "I repeatedly use Go. " + strings.Repeat("x", 1001)
	first := formationTestTurn(t, store, text, "first")
	second := formationTestTurn(t, store, text, "assessment")
	_, _, err := store.EnqueuePatternFormationJob(context.Background(), memory.FormationSource{RequestID: "assessment", SessionID: "session", SessionGeneration: 1, TurnID: second, Model: "model"}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	batch := memory.MemoryPatternBatch{Patterns: []memory.MemoryPattern{{Statement: "The user may regularly use Go.", Category: "projects", ClaimSlot: "project.language", ClaimValue: "go", Sensitivity: "low", Confidence: .8, Observations: []memory.PatternObservation{{SourceTurnID: first, Evidence: text}, {SourceTurnID: second, Evidence: text}}}}}
	artifact, err := memory.MarshalMemoryPatternBatchArtifact(batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFormationJobArtifact(context.Background(), job, string(artifact)); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, nil, "model", config.NewLogger(config.LevelError))
	if err := s.process(context.Background(), &job); err != nil {
		t.Fatalf("legacy rejection became retry error: %v", err)
	}
	if err := store.CompleteFormationJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 0 || job.ModelSubmissionCount != 0 {
		t.Fatalf("legacy replay memories=%+v submissions=%d err=%v", memories, job.ModelSubmissionCount, err)
	}
}

func TestAssessmentReconcileBackfillsEveryAnchorIncludingFirst(t *testing.T) {
	store, db := formationTestStoreWithDB(t)
	formationTestTurn(t, store, "First ordinary turn", "first")
	formationTestTurn(t, store, "Second ordinary turn", "second")
	f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{}}}
	s := NewService(store, f, "model", config.NewLogger(config.LevelError))
	s.reconcile(context.Background())
	s.reconcile(context.Background())
	s.drain(context.Background())
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE extractor_version = ? AND state = 'succeeded'`, memory.AssessmentExtractorVersion).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 || f.assessmentCalls != 2 || f.patternCalls != 0 {
		t.Fatalf("jobs=%d assessments=%d patterns=%d", count, f.assessmentCalls, f.patternCalls)
	}
}

func TestAssessmentForegroundV3UsesNoModelAndReplays(t *testing.T) {
	store := formationTestStore(t)
	turnID := formationTestForegroundTurn(t, store, "I prefer concise replies.", "foreground")
	if err := store.MarkFormationEligible(context.Background(), "user-1", turnID); err != nil {
		t.Fatal(err)
	}
	_, _, err := store.EnqueueAgentSaveFormationJob(context.Background(), memory.FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: turnID, Model: "model"}, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAssessmentExtractor{err: errors.New("model must not run")}
	s := NewService(store, f, "model", config.NewLogger(config.LevelError))
	for i := 0; i < 2; i++ {
		if err := s.process(context.Background(), &job); err != nil {
			t.Fatal(err)
		}
	}
	if f.assessmentCalls != 0 || f.calls != 0 || job.ModelSubmissionCount != 0 {
		t.Fatal("foreground invoked model")
	}
	memories, err := store.ListMemories("user-1", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}

func TestAssessmentSummaryRequiresCompletionAndContainsNoPayload(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, fail := range []bool{false, true} {
			store, db := formationTestStoreWithDB(t)
			turnID := formationTestTurn(t, store, "I use Go private-content-canary", "assessment")
			enqueueTestAssessment(t, store, turnID)
			if fail {
				if _, err := db.SQL().Exec(`CREATE TRIGGER fail_assessment_complete BEFORE UPDATE OF state ON durable_jobs WHEN NEW.state = 'succeeded' AND NEW.job_kind = 'memory_formation' BEGIN SELECT RAISE(ABORT, 'private-failure-canary'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			log := config.NewLogger(level)
			log.SetOutput(&output)
			f := &fakeAssessmentExtractor{batch: memory.AssessmentBatch{Version: 1, Items: []memory.ForegroundMemoryCandidate{assessmentTestCandidate()}}}
			NewService(store, f, "model", log).drain(context.Background())
			completed := 0
			for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] == "user_memory.formation.job.complete" {
					completed++
					if record["level"] != "info" || record["published_outcome_count"] != float64(1) || record["request_id"] != "assessment" || record["operation_id"] == nil {
						t.Fatalf("invalid summary %s", line)
					}
				}
			}
			want := 1
			if fail {
				want = 0
			}
			if completed != want || strings.Contains(output.String(), "private-content-canary") || strings.Contains(output.String(), "private-failure-canary") {
				t.Fatalf("completion=%d logs=%s", completed, output.String())
			}
		}
	}
}
