package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestForegroundMemoryRoundTripOnSessionTurn(t *testing.T) {
	store := newTestStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(context.Background(), "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	output, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: "I prefer dark mode.", Statement: "The user prefers dark mode.", Evidence: "I prefer dark mode.",
		Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect,
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave, Scope: policy.ScopeLongTerm,
		Category: policy.CategoryDurablePreferences, Context: policy.ContextDirectAssertion,
		Confidence: 1, Importance: 3, ClaimSlot: "durable_preferences.fact", ClaimValue: "dark mode",
	})
	if err != nil || output.Approval != policy.ApprovalApproved {
		t.Fatalf("candidate=%+v err=%v", output, err)
	}
	turn, err := store.AppendPendingSessionTurn(
		context.Background(), SessionTurnWrite{SessionID: "session", UserID: "user", Generation: profile.Generation, UserText: "I prefer dark mode.", AssistantText: "Noted.", ToolNames: nil, History: EmptyToolHistory(), Staged: []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: output}}, TTL: time.Hour, Pressure: SessionPromptPressure{Tokens: 10, Limit: 100, Version: "test"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.SessionTurnForegroundMemory(context.Background(), "user", turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Candidates) != 1 || artifact.Candidates[0].Statement != output.Statement || artifact.Candidates[0].Provenance != policy.ProvenanceUserStatement || artifact.Candidates[0].EvidenceType != "direct_statement" || artifact.Candidates[0].Confidence != 1 {
		t.Fatalf("artifact=%+v", artifact)
	}
	if err := store.MarkFormationEligible(context.Background(), "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	source := FormationSource{SessionID: turn.SessionID, SessionGeneration: turn.Generation, TurnID: turn.ID, Model: "model"}
	firstJobID, created, err := store.EnqueueAgentSaveFormationJob(context.Background(), source, "user")
	if err != nil || !created {
		t.Fatalf("first enqueue id=%d created=%v err=%v", firstJobID, created, err)
	}
	secondJobID, created, err := store.EnqueueAgentSaveFormationJob(context.Background(), source, "user")
	if err != nil || created || secondJobID != firstJobID {
		t.Fatalf("duplicate enqueue id=%d want=%d created=%v err=%v", secondJobID, firstJobID, created, err)
	}
	rechecked, err := artifact.Candidates[0].Evaluate("I prefer dark mode.")
	if err != nil || rechecked.Approval != policy.ApprovalApproved {
		t.Fatalf("rechecked=%+v err=%v", rechecked, err)
	}
	var raw string
	if err := store.sql.QueryRow(`SELECT foreground_memory FROM session_turns WHERE id = ?`, turn.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"approval", "decision", "sensitivity", "source_authority"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("persisted policy output %q in %s", forbidden, raw)
		}
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET foreground_memory = '{"version":2,"candidates":[]}' WHERE id = ?`, turn.ID); err == nil {
		t.Fatal("mutable foreground memory artifact was accepted")
	}
}

func TestForegroundMemoryEncodingBoundsAndOwnership(t *testing.T) {
	approved := policy.CandidateOutput{
		Statement: strings.Repeat("x", MaxForegroundMemoryBytes), Evidence: "I use x.", Category: policy.CategoryNotes,
		ClaimSlot: "notes.fact", ClaimValue: "x", Provenance: policy.ProvenanceUserStatement, Mode: policy.ModeAgentSave, Approval: policy.ApprovalApproved, Confidence: 1,
	}
	if _, err := EncodeForegroundMemory("user", []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: approved}}); err == nil {
		t.Fatal("oversized foreground memory was accepted")
	}
	approved.Statement = "The user uses x."
	if _, err := EncodeForegroundMemory("other", []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: approved}}); err == nil {
		t.Fatal("cross-tenant foreground memory was accepted")
	}
	if _, err := DecodeForegroundMemory(`{"version":2,"candidates":[],"unexpected":true}`); err == nil {
		t.Fatal("unknown artifact field was accepted")
	}
	if _, err := DecodeForegroundMemory(`{"version":2,"candidates":[{"statement":"The user likes tea.","evidence":"I like tea.","category":"identity","claim_slot":"preference.drink","claim_value":"tea","evidence_type":"direct_statement","provenance":"user_statement","confidence":1}]}`); err == nil {
		t.Fatal("policy-rejected artifact candidate was accepted")
	}
}

func TestForegroundMemoryPersistsExactModelAssessment(t *testing.T) {
	proposed := policy.CandidateOutput{
		Statement: "The user might like pancakes.", Evidence: "I want pancakes.", Category: policy.CategoryDurablePreferences,
		ClaimSlot: "preference.food", ClaimValue: "pancakes", Provenance: policy.ProvenanceModelInference,
		Mode: policy.ModeAgentSave, Approval: policy.ApprovalProposed, Confidence: 0.2,
	}
	encoded, err := EncodeForegroundMemory("user", []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: proposed, TargetMemoryID: 17, SupersedesStatement: "The user dislikes pancakes."}})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := DecodeForegroundMemory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	candidate := artifact.Candidates[0]
	if candidate.EvidenceType != "model_inference" || candidate.Provenance != policy.ProvenanceModelInference || candidate.Confidence != 0.2 || candidate.TargetMemoryID != 17 || candidate.SupersedesStatement != "The user dislikes pancakes." {
		t.Fatalf("assessment was not retained: %+v", candidate)
	}
	rechecked, err := candidate.Evaluate("I want pancakes.")
	if err != nil || rechecked.Mode != policy.ModeAgentSave || rechecked.Approval != policy.ApprovalProposed || rechecked.Confidence != 0.2 || rechecked.SourceAuthority != policy.AuthorityModel {
		t.Fatalf("assessment was not rerun exactly: %+v err=%v", rechecked, err)
	}
}
