package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestMaintenanceDirectlyDeletesStaleCandidateRows(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	seedAccountUsers(t, store, "user")
	old := formatTime(time.Now().UTC().Add(-48 * time.Hour))
	result, err := store.sql.Exec(`INSERT INTO memory_candidates(canonical_user_id,idempotency_key,state,scope,category,statement,evidence,confidence,importance,provenance_type,extractor_version,formation_mode,sensitivity,created_at,updated_at,decision_reason,claim_slot,claim_value) VALUES ('user','stale','rejected','long_term','notes','The user has a stale candidate.','evidence',0.2,2,'user_statement','v1','automatic','low',?,?,'rejected','notes.stale','stale')`, old, old)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ := result.LastInsertId()
	policy := config.RetentionPolicy{RetiredIndexRetention: time.Hour, SessionInactivity: time.Hour, PendingDeliveryTimeout: time.Minute, SuccessfulJobRetention: time.Hour, DeadJobRetention: 24 * time.Hour, AccountChallengeGrace: time.Hour, MaintenanceInterval: time.Hour, DatabaseOptimizeInterval: time.Hour, BatchSize: 100}
	counts, err := store.MaintenanceSweep(context.Background(), time.Now().UTC(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if counts.CandidatesDeleted+counts.SessionCleanup.CandidatesDeleted != 1 {
		t.Fatalf("maintenance counts=%+v", counts)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE id = ?`, 0, candidateID)
}

func TestMaintenanceRetainsRecentSubthresholdCandidateForReinforcement(t *testing.T) {
	store := newFormationTestStore(t)
	lowTurn := seedFormationTurn(t, store, "user", "session", "I want pancakes.", "low-pancakes")
	low, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: "I want pancakes.", Statement: "The user might like pancakes.", Evidence: "I want pancakes.",
		Provenance: policy.ProvenanceModelInference, ClaimedAuthority: policy.AuthorityModel,
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave, Scope: policy.ScopeLongTerm,
		Category: policy.CategoryDurablePreferences, Context: policy.ContextDirectAssertion, Confidence: 0.2, Importance: 3,
		ClaimSlot: "preference.food.pancakes", ClaimValue: "likes",
	})
	if err != nil {
		t.Fatal(err)
	}
	lowCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: low, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: lowTurn}, IdempotencyKey: "low-pancakes"})
	if err != nil {
		t.Fatal(err)
	}
	retention := config.RetentionPolicy{RetiredIndexRetention: time.Hour, SessionInactivity: time.Hour, PendingDeliveryTimeout: time.Minute, SuccessfulJobRetention: time.Hour, DeadJobRetention: 24 * time.Hour, AccountChallengeGrace: time.Hour, MaintenanceInterval: time.Hour, DatabaseOptimizeInterval: time.Hour, BatchSize: 100}
	if _, err := store.MaintenanceSweep(context.Background(), time.Now().UTC(), retention); err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE id = ? AND state = 'proposed'`, 1, lowCandidate.ID)

	highTurn := seedFormationTurn(t, store, "user", "session", "I like pancakes.", "high-pancakes")
	high := low
	high.Statement = "The user likes pancakes."
	high.Evidence = "I like pancakes."
	high.Provenance = policy.ProvenanceUserStatement
	high.SourceAuthority = policy.AuthorityUserDirect
	high.Confidence = 0.95
	high.Approval = policy.ApprovalApproved
	high.Decision = policy.DecisionAutomatic
	high.Reason = "direct user fact meets the active memory threshold"
	created, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: high, Source: FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: highTurn}, IdempotencyKey: "high-pancakes"})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.EntryByID(created.PublishedMemoryID)
	if err != nil || memory.Confidence != 0.95 || memory.EvidenceCount != 2 {
		t.Fatalf("reinforced memory=%+v err=%v", memory, err)
	}
}

func TestMaintenanceRetainsFailedCompactionContractForActiveSession(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	jobID, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SkipSessionCompactionJob(context.Background(), job, "missing_tool_call"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET completed_at = ?, updated_at = ? WHERE id = ?`, formatTime(now.Add(-48*time.Hour)), formatTime(now.Add(-48*time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
	policy := config.RetentionPolicy{RetiredIndexRetention: time.Hour, SessionInactivity: 24 * time.Hour, PendingDeliveryTimeout: time.Minute, SuccessfulJobRetention: time.Hour, DeadJobRetention: 24 * time.Hour, AccountChallengeGrace: time.Hour, MaintenanceInterval: time.Hour, DatabaseOptimizeInterval: time.Hour, BatchSize: 100}
	if _, err := store.MaintenanceSweep(context.Background(), now, policy); err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE id = ?`, 1, jobID)
	if _, err := store.ResetSession(context.Background(), "user", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE id = ?`, 0, jobID)
}
