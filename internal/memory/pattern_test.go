package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestPatternJobFreezesNewestEightTurnsAndRequiresTwo(t *testing.T) {
	store := newFormationTestStore(t)
	first := seedFormationTurn(t, store, "user", "session", "I keep making review notes concise.", "request-0")
	if id, created, err := store.EnqueuePatternFormationJob(context.Background(), FormationSource{RequestID: "request-0", SessionID: "session", SessionGeneration: 1, TurnID: first, Model: "model"}, "user"); err != nil || created || id != 0 {
		t.Fatalf("single-turn pattern job id=%d created=%v err=%v", id, created, err)
	}
	ids := []int64{first}
	for i := 1; i < 10; i++ {
		ids = append(ids, seedFormationTurn(t, store, "user", "session", fmt.Sprintf("I keep making review note %d concise.", i), fmt.Sprintf("request-%d", i)))
	}
	jobID, created, err := store.EnqueuePatternFormationJob(context.Background(), FormationSource{RequestID: "request-9", SessionID: "session", SessionGeneration: 1, TurnID: ids[9], Model: "model"}, "user")
	if err != nil || !created || jobID == 0 {
		t.Fatalf("enqueue id=%d created=%v err=%v", jobID, created, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	window, err := store.FormationPatternContext(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	want := ids[2:]
	if len(window.TurnIDs) != 8 {
		t.Fatalf("window=%v", window.TurnIDs)
	}
	for i := range want {
		if window.TurnIDs[i] != want[i] {
			t.Fatalf("window=%v want=%v", window.TurnIDs, want)
		}
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET artifact_payload = '{"version":1,"turn_ids":[1,2]}' WHERE id = ?`, job.ID); err == nil {
		t.Fatal("mutable frozen pattern context was accepted")
	}
}

func TestPatternAggregationUsesStrongestServingMetadata(t *testing.T) {
	store := newFormationTestStore(t)
	first := seedFormationTurn(t, store, "user", "session", "Concise review notes help me focus.", "request-1")
	second := seedFormationTurn(t, store, "user", "session", "Concise review summaries reduce my stress.", "request-2")
	_, created, err := store.EnqueuePatternFormationJob(context.Background(), FormationSource{RequestID: "request-2", SessionID: "session", SessionGeneration: 1, TurnID: second, Model: "model"}, "user")
	if err != nil || !created {
		t.Fatalf("enqueue created=%v err=%v", created, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pattern := MemoryPattern{Statement: "The user may prefer concise review notes.", Category: "communication_preferences", ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: 0.65}
	for index, turn := range []StoredSessionTurn{{ID: first, UserID: "user", SessionID: "session", Generation: 1, UserText: "Concise review notes help me focus."}, {ID: second, UserID: "user", SessionID: "session", Generation: 1, UserText: "Concise review summaries reduce my stress."}} {
		output, err := EvaluatePatternObservation(turn, pattern, turn.UserText)
		if err != nil {
			t.Fatal(err)
		}
		candidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, RequireCorroboration: true, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: turn.ID, ExtractorVersion: PatternExtractorVersion}, IdempotencyKey: fmt.Sprintf("metadata-pattern-%d", index), FormationJob: &job})
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			if _, err := store.sql.Exec(`UPDATE memory_candidates SET importance = 5, sensitivity = 'high_impact_interaction' WHERE id = ?`, candidate.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	memoryID, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue)
	if err != nil || memoryID == 0 {
		t.Fatalf("aggregate memory=%d err=%v", memoryID, err)
	}
	var importance int
	var sensitivity string
	if err := store.sql.QueryRow(`SELECT importance, sensitivity FROM memory_entries WHERE id = ?`, memoryID).Scan(&importance, &sensitivity); err != nil {
		t.Fatal(err)
	}
	if importance != 5 || sensitivity != "high_impact_interaction" {
		t.Fatalf("importance=%d sensitivity=%s", importance, sensitivity)
	}
}

func TestPatternReconciliationReconstructsDeterministicEligibleWindows(t *testing.T) {
	store := newFormationTestStore(t)
	first := seedFormationTurn(t, store, "user", "session", "I keep making review notes concise.", "request-1")
	second := seedFormationTurn(t, store, "user", "session", "I repeatedly make review summaries concise.", "request-2")
	third := seedFormationTurn(t, store, "user", "session", "I often make review comments concise.", "request-3")
	count, err := store.ReconcilePatternFormationJobs(context.Background(), "model")
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	window, err := store.FormationPatternContext(context.Background(), job)
	if err != nil || job.TurnID != second || len(window.TurnIDs) != 2 || window.TurnIDs[0] != first || window.TurnIDs[1] != second {
		t.Fatalf("first job=%+v window=%v err=%v", job, window.TurnIDs, err)
	}
	if err := store.CompleteFormationJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	job, err = store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	window, err = store.FormationPatternContext(context.Background(), job)
	if err != nil || job.TurnID != third || len(window.TurnIDs) != 3 || window.TurnIDs[0] != first || window.TurnIDs[2] != third {
		t.Fatalf("second job=%+v window=%v err=%v", job, window.TurnIDs, err)
	}
	if count, err := store.ReconcilePatternFormationJobs(context.Background(), "model"); err != nil || count != 0 {
		t.Fatalf("replay count=%d err=%v", count, err)
	}
}

func TestPatternObservationRequiresExactEligibleFrozenEvidence(t *testing.T) {
	turn := StoredSessionTurn{ID: 7, UserText: "I keep making review notes concise."}
	pattern := MemoryPattern{Statement: "The user may favor concise review communication.", Category: "communication_preferences", ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: 0.65}
	output, err := EvaluatePatternObservation(turn, pattern, turn.UserText)
	if err != nil || output.Mode != policy.ModeBackgroundPattern || output.Provenance != policy.ProvenanceModelInference || output.Confidence != 0.65 || output.Importance != 3 || output.Approval != policy.ApprovalApproved {
		t.Fatalf("output=%+v err=%v", output, err)
	}
	for _, evidence := range []string{turn.UserText + " ", "I prefer concise replies."} {
		if _, err := EvaluatePatternObservation(turn, pattern, evidence); err == nil {
			t.Fatalf("accepted ineligible evidence %q", evidence)
		}
	}
	for _, text := range []string{"For context, I prefer concise replies.", "Remember that I prefer concise replies.", "If needed, I prefer concise replies."} {
		if _, err := EvaluatePatternObservation(StoredSessionTurn{ID: 8, UserText: text}, pattern, text); err != nil {
			t.Fatalf("wording was falsely rejected %q: %v", text, err)
		}
	}
}

func TestPatternAggregationPublishesTwoCorrelatedObservationsIdempotently(t *testing.T) {
	store := newFormationTestStore(t)
	texts := []string{"I keep making review notes concise.", "I repeatedly make review summaries concise.", "I usually make review comments concise."}
	ids := []int64{
		seedFormationTurn(t, store, "user", "session", texts[0], "request-1"),
		seedFormationTurn(t, store, "user", "session", texts[1], "request-2"),
		seedFormationTurn(t, store, "user", "session", texts[2], "request-3"),
	}
	_, created, err := store.EnqueuePatternFormationJob(context.Background(), FormationSource{RequestID: "request-3", SessionID: "session", SessionGeneration: 1, TurnID: ids[2], Model: "model"}, "user")
	if err != nil || !created {
		t.Fatalf("enqueue created=%v err=%v", created, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pattern := MemoryPattern{Statement: "The user may favor concise review communication.", Category: "communication_preferences", ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: 0.65}
	for i, id := range ids[:2] {
		output, err := EvaluatePatternObservation(StoredSessionTurn{ID: id, UserID: "user", SessionID: "session", Generation: 1, UserText: texts[i]}, pattern, texts[i])
		if err != nil {
			t.Fatal(err)
		}
		candidate, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, RequireCorroboration: true, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: id, Model: "model", ExtractorVersion: PatternExtractorVersion}, IdempotencyKey: fmt.Sprintf("pattern:%d:communication.review_style:concise", id), FormationJob: &job})
		if err != nil || !created || candidate.PublishedMemoryID != 0 {
			t.Fatalf("candidate=%+v created=%v err=%v", candidate, created, err)
		}
	}
	memoryID, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue)
	if err != nil || memoryID == 0 {
		t.Fatalf("memory=%d err=%v", memoryID, err)
	}
	memory, err := store.EntryByID(memoryID)
	if err != nil || memory.Confidence != 0.65 || memory.EvidenceCount != 2 || memory.ProvenanceType != "model_inference" {
		t.Fatalf("memory=%+v err=%v", memory, err)
	}
	if again, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue); err != nil || again != 0 {
		t.Fatalf("idempotent aggregate=%d err=%v", again, err)
	}
	var linked int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE published_memory_id = ?`, memoryID).Scan(&linked); err != nil || linked != 2 {
		t.Fatalf("linked=%d err=%v", linked, err)
	}
	lower := pattern
	lower.Confidence = 0.4
	lower.Statement = pattern.Statement
	output, err := EvaluatePatternObservation(StoredSessionTurn{ID: ids[2], UserText: texts[2]}, lower, texts[2])
	if err != nil {
		t.Fatal(err)
	}
	lowerCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, RequireCorroboration: true, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: ids[2], ExtractorVersion: PatternExtractorVersion}, FormationJob: &job})
	if err != nil {
		t.Fatal(err)
	}
	if lowerCandidate.State != "proposed" {
		t.Fatalf("lower candidate=%+v output=%+v", lowerCandidate, output)
	}
	if reinforcedID, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue); err != nil || reinforcedID != memoryID {
		t.Fatalf("lower reinforcement=%d err=%v", reinforcedID, err)
	}
	memory, err = store.EntryByID(memoryID)
	if err != nil || memory.Confidence != 0.65 || memory.Statement != pattern.Statement || memory.EvidenceCount != 3 {
		t.Fatalf("lower assessment changed memory=%+v err=%v", memory, err)
	}

	higher := lower
	higher.Confidence = 0.8
	higher.Statement = "The user likely favors concise review communication."
	output, err = EvaluatePatternObservation(StoredSessionTurn{ID: ids[2], UserText: texts[2]}, higher, texts[2])
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, RequireCorroboration: true, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: ids[2], ExtractorVersion: PatternExtractorVersion}, IdempotencyKey: "overlap-higher", FormationJob: &job}); err != nil || created {
		t.Fatalf("higher overlap created=%v err=%v", created, err)
	}
	memory, err = store.EntryByID(memoryID)
	if err != nil || memory.Confidence != 0.8 || memory.Statement != higher.Statement || memory.EvidenceCount != 3 {
		t.Fatalf("higher assessment memory=%+v err=%v", memory, err)
	}
}

func TestPatternAggregationKeepsWeakDistinctObservationsProposed(t *testing.T) {
	store := newFormationTestStore(t)
	texts := []string{"I keep review notes compact.", "I often shorten review summaries."}
	ids := []int64{
		seedFormationTurn(t, store, "user", "session", texts[0], "weak-1"),
		seedFormationTurn(t, store, "user", "session", texts[1], "weak-2"),
	}
	if _, _, err := store.EnqueuePatternFormationJob(context.Background(), FormationSource{RequestID: "weak-2", SessionID: "session", SessionGeneration: 1, TurnID: ids[1], Model: "model"}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pattern := MemoryPattern{Statement: "The user may favor compact reviews.", Category: "communication_preferences", ClaimSlot: "communication.review_style", ClaimValue: "compact", Sensitivity: "low", Confidence: 0.2}
	for i, id := range ids {
		output, err := EvaluatePatternObservation(StoredSessionTurn{ID: id, UserText: texts[i]}, pattern, texts[i])
		if err != nil {
			t.Fatal(err)
		}
		candidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, RequireCorroboration: true, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: id, ExtractorVersion: PatternExtractorVersion}, FormationJob: &job})
		if err != nil || candidate.State != "proposed" || candidate.PublishedMemoryID != 0 {
			t.Fatalf("candidate=%+v err=%v", candidate, err)
		}
	}
	if memoryID, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue); err != nil || memoryID != 0 {
		t.Fatalf("weak aggregate=%d err=%v", memoryID, err)
	}
	var proposed int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE state = 'proposed' AND published_memory_id IS NULL`).Scan(&proposed); err != nil || proposed != 2 {
		t.Fatalf("proposed=%d err=%v", proposed, err)
	}
}

func TestPatternLeaseMembershipAndDirectConflict(t *testing.T) {
	store := newFormationTestStore(t)
	first := seedFormationTurn(t, store, "user", "session", "I keep making review notes concise.", "request-1")
	anchor := seedFormationTurn(t, store, "user", "session", "I repeatedly make review summaries concise.", "request-2")
	outside := seedFormationTurn(t, store, "user", "other", "I keep making other notes concise.", "outside")
	_, _, err := store.EnqueuePatternFormationJob(context.Background(), FormationSource{RequestID: "request-2", SessionID: "session", SessionGeneration: 1, TurnID: anchor, Model: "model"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pattern := MemoryPattern{Statement: "The user may favor concise review communication.", Category: "communication_preferences", ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: 0.65}
	output, err := EvaluatePatternObservation(StoredSessionTurn{ID: outside, UserText: "I keep making other notes concise."}, pattern, "I keep making other notes concise.")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, RequireCorroboration: true, Source: FormationSource{SessionID: "other", SessionGeneration: 1, TurnID: outside, ExtractorVersion: PatternExtractorVersion}, FormationJob: &job}); err == nil {
		t.Fatal("accepted observation outside frozen job membership")
	}
	direct := evaluatedClaimCandidate(t, "I prefer detailed review notes", "The user prefers detailed review notes.", policy.CategoryCommunicationPreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.9, "communication.review_style", "detailed")
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: direct, IdempotencyKey: "direct"}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id   int64
		text string
	}{{first, "I keep making review notes concise."}, {anchor, "I repeatedly make review summaries concise."}} {
		out, err := EvaluatePatternObservation(StoredSessionTurn{ID: item.id, UserText: item.text}, pattern, item.text)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: out, RequireCorroboration: true, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: item.id, ExtractorVersion: PatternExtractorVersion}, IdempotencyKey: fmt.Sprintf("pattern-conflict:%d", item.id), FormationJob: &job}); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue); err != nil || id != 0 {
		t.Fatalf("weak pattern displaced direct memory id=%d err=%v", id, err)
	}
	memories, err := store.ListMemories("user", ScopeLongTerm, "communication_preferences", 10)
	if err != nil || len(memories) != 1 || memories[0].ClaimValue != "detailed" {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
	job.LeaseUntil = job.LeaseUntil.Add(time.Second)
	if _, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("stale aggregation error=%v", err)
	}
}

func TestPatternAggregationReusesMatchingDirectCanonicalMemory(t *testing.T) {
	store := newFormationTestStore(t)
	direct := evaluatedClaimCandidate(t, "I prefer concise reviews", "The user prefers concise reviews.", policy.CategoryCommunicationPreferences, policy.ProvenanceUserStatement, policy.SensitivityLow, 0.8, "communication.review_style", "concise")
	directCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: direct, IdempotencyKey: "matching-direct"})
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{"I keep making review notes concise.", "I repeatedly make review summaries concise."}
	ids := []int64{seedFormationTurn(t, store, "user", "session", texts[0], "request-1"), seedFormationTurn(t, store, "user", "session", texts[1], "request-2")}
	_, _, err = store.EnqueuePatternFormationJob(context.Background(), FormationSource{RequestID: "request-2", SessionID: "session", SessionGeneration: 1, TurnID: ids[1], Model: "model"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pattern := MemoryPattern{Statement: "The user may favor concise review communication.", Category: "communication_preferences", ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: 0.65}
	for i, id := range ids {
		output, err := EvaluatePatternObservation(StoredSessionTurn{ID: id, UserText: texts[i]}, pattern, texts[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, RequireCorroboration: true, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: id, ExtractorVersion: PatternExtractorVersion}, IdempotencyKey: fmt.Sprintf("matching-pattern:%d", id), FormationJob: &job}); err != nil {
			t.Fatal(err)
		}
	}
	memoryID, err := store.AggregatePatternCandidates(context.Background(), job, pattern.ClaimSlot, pattern.ClaimValue)
	if err != nil || memoryID != directCandidate.PublishedMemoryID {
		t.Fatalf("memory=%d direct=%d err=%v", memoryID, directCandidate.PublishedMemoryID, err)
	}
	memories, err := store.ListMemories("user", ScopeLongTerm, "communication_preferences", 10)
	if err != nil || len(memories) != 1 || memories[0].ProvenanceType != "user_statement" || memories[0].EvidenceCount != 3 {
		t.Fatalf("memories=%+v err=%v", memories, err)
	}
}
