package memory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestSessionCleanupCountsPreserveLogJSON(t *testing.T) {
	counts := MaintenanceCounts{SessionCleanup: SessionCleanupCounts{
		SessionTurnsDeleted: 1, SessionsDeactivated: 2, MemoryEntriesExpired: 3,
		CandidatesDeleted: 4, FormationJobsDeleted: 5, SessionSummariesDeleted: 6, CompactionJobsRetired: 7,
	}}
	encoded, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	const want = `{"SessionTurnsDeleted":1,"TenantSessionsDeleted":2,"MemoryEntriesExpired":3,"CandidatesErased":4,"FormationJobsDeleted":5,"SessionSummariesDeleted":6,"CompactionJobsDeleted":7}`
	if got := string(envelope["session_cleanup"]); got != want {
		t.Fatalf("session cleanup JSON = %s, want %s", got, want)
	}
}

func TestCleanupExpiredSessionsIndependentSweep(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	ctx := context.Background()
	expiredProfile, err := store.ResolveSessionProfile(ctx, "user", "expired-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.appendFixtureDeliveredTurn(ctx, "expired-session", "user", expiredProfile.Generation, "bound", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.appendFixtureOrphanTurn(ctx, "independently-expired", "user", "expired", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.appendFixtureOrphanTurn(ctx, "subsecond-future", "user", "future", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishFixtureMemory(ctx, "user", memoryFixture{
		Scope: ScopeLongTerm, Category: "identity", Statement: "The user is Ada.", Confidence: 1, Importance: 5,
	}); err != nil {
		t.Fatal(err)
	}
	latestProfile, err := store.ResolveSessionProfile(ctx, "user", "active-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if latestProfile.VersionID == expiredProfile.VersionID {
		t.Fatal("profile mutation did not create a new version")
	}

	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'expired-session'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'independently-expired'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'subsecond-future'`, formatTime(now.Add(500*time.Millisecond))); err != nil {
		t.Fatal(err)
	}

	counts, err := store.cleanupExpiredSessions(ctx, now, normalizedMaintenancePolicy(config.RetentionPolicy{}))
	if err != nil {
		t.Fatal(err)
	}
	if counts != (SessionCleanupCounts{SessionTurnsDeleted: 2, SessionsDeactivated: 1}) {
		t.Fatalf("unexpected cleanup counts: %+v", counts)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE session_id = 'subsecond-future'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'user' AND session_id = 'expired-session' AND is_active = 0`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'user' AND profile_version = ?`, 1, latestProfile.VersionID)

	next, err := store.ResolveSessionProfile(ctx, "user", "expired-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != expiredProfile.Generation+1 {
		t.Fatalf("next generation = %d, want %d", next.Generation, expiredProfile.Generation+1)
	}
}

func TestCleanupExpiredSessionsDeletesPatternJobsBeforeSourceTurns(t *testing.T) {
	store := newFormationTestStore(t)
	ctx := context.Background()
	first := seedFormationTurn(t, store, "user", "expired-pattern", "I keep review notes concise.", "request-1")
	second := seedFormationTurn(t, store, "user", "expired-pattern", "I repeatedly shorten review notes.", "request-2")
	jobID, created, err := store.EnqueuePatternFormationJob(ctx, FormationSource{RequestID: "request-2", SessionID: "expired-pattern", SessionGeneration: 1, TurnID: second, Model: "model"}, "user")
	if err != nil || !created {
		t.Fatalf("enqueue pattern job id=%d created=%v err=%v", jobID, created, err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET extraction_payload = ? WHERE id = ?`, `{"patterns":[{"evidence":"private"}]}`, jobID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'expired-pattern'; UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'expired-pattern'`, formatTime(now.Add(-time.Second)), formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	counts, err := store.cleanupExpiredSessions(ctx, now, normalizedMaintenancePolicy(config.RetentionPolicy{}))
	if err != nil {
		t.Fatal(err)
	}
	if counts.FormationJobsDeleted != 1 || counts.SessionTurnsDeleted != 2 {
		t.Fatalf("cleanup counts=%+v", counts)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE id = ?`, 0, jobID)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE id IN (?, ?)`, 0, first, second)
}

func TestCleanupExpiresDurableMemoryAndErasesFormationRetention(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	ctx := context.Background()
	memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeShortTerm, Category: "notes", Statement: "Temporary secret code.", Evidence: "temporary", TTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	pendingInput := policy.CandidateInput{SourceUserText: "My phone is 555-0100", Statement: "The user's phone is 555-0100.", Evidence: "My phone is 555-0100", Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect, Sensitivity: policy.SensitivityIdentityOrContact, Mode: policy.ModeAutomaticExtraction, Scope: policy.ScopeLongTerm, Category: policy.CategoryIdentity, Context: policy.ContextDirectAssertion, Confidence: 0.2, Importance: 4}
	output, err := policy.Evaluate(pendingInput)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := store.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, IdempotencyKey: "old-pending"})
	if err != nil {
		t.Fatal(err)
	}
	turnID := seedFormationTurn(t, store, "user", "session", "source")
	jobID, err := store.EnqueueFormationJob(ctx, FormationSource{TurnID: turnID, SessionID: "session", ExtractorVersion: FormationExtractorVersion}, "user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE memory_candidates SET created_at = ? WHERE id = ?; UPDATE durable_jobs SET state = 'succeeded', completed_at = ? WHERE id = ? AND job_kind = 'memory_formation'`, formatTime(now.Add(-31*24*time.Hour)), candidate.ID, formatTime(now.Add(-8*24*time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	counts, err := store.cleanupExpiredSessions(ctx, now, normalizedMaintenancePolicy(config.RetentionPolicy{}))
	if err != nil {
		t.Fatal(err)
	}
	if counts.MemoryEntriesExpired != 1 || counts.CandidatesDeleted != 1 || counts.FormationJobsDeleted != 1 {
		t.Fatalf("cleanup counts=%+v", counts)
	}
	var status string
	if err := store.sql.QueryRow(`SELECT status FROM memory_entries WHERE id = ?`, memory.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	var candidateCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE id = ?`, candidate.ID).Scan(&candidateCount); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || candidateCount != 0 {
		t.Fatalf("status=%s candidate_count=%d", status, candidateCount)
	}
	var deleteChanges int
	if err := store.sql.QueryRow(`SELECT count(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind = 'memory' AND entity_id = ? AND operation = 'delete'`, memory.ID).Scan(&deleteChanges); err != nil {
		t.Fatal(err)
	}
	if deleteChanges != 1 {
		t.Fatalf("expired memory delete changes=%d", deleteChanges)
	}
}

func TestCleanupExpiredSessionsRollsBackOnFailure(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	profile, err := store.ResolveSessionProfile(context.Background(), "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.appendFixtureDeliveredTurn(context.Background(), "session", "user", profile.Generation, "user", "assistant", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`CREATE TRIGGER fail_session_cleanup BEFORE UPDATE OF is_active ON sessions BEGIN SELECT RAISE(ABORT, 'cleanup blocked'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.cleanupExpiredSessions(context.Background(), now, normalizedMaintenancePolicy(config.RetentionPolicy{})); err == nil {
		t.Fatal("cleanup unexpectedly succeeded")
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'user' AND is_active = 1`, 1)
}

func TestCleanupRetainsTranscriptAndSummaryForActiveSessionLifetime(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	ctx := context.Background()
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		if err := store.appendFixtureDeliveredTurn(ctx, "session", "user", profile.Generation, text, "answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := store.CompactionWindowAfter(ctx, "user", "session", profile.Generation, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueSessionCompactionJob(ctx, "user", "session", profile.Generation, turns.Turns[0].ID, turns.Turns[1].ID, turns.Turns[1].ID, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(ctx, "test", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(ctx, job, SummaryArtifact{Narrative: "summary", GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	counts, err := store.cleanupExpiredSessions(ctx, now, normalizedMaintenancePolicy(config.RetentionPolicy{}))
	if err != nil {
		t.Fatal(err)
	}
	if counts.SessionTurnsDeleted != 0 || counts.SessionSummariesDeleted != 0 || counts.CompactionJobsRetired != 0 {
		t.Fatalf("active session history was removed: %+v", counts)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user' AND session_id = 'session'`, 2)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'user' AND session_id = 'session'`, 1)

	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	counts, err = store.cleanupExpiredSessions(ctx, now, normalizedMaintenancePolicy(config.RetentionPolicy{}))
	if err != nil {
		t.Fatal(err)
	}
	if counts.SessionTurnsDeleted != 2 || counts.SessionSummariesDeleted != 1 || counts.CompactionJobsRetired != 1 || counts.SessionsDeactivated != 1 {
		t.Fatalf("inactive session history cleanup counts: %+v", counts)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE id = ? AND state = 'succeeded' AND artifact_summary_id IS NULL`, 1, job.ID)
	if _, err := store.MaintenanceSweep(ctx, now.Add(8*24*time.Hour), config.RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE id = ?`, 0, job.ID)
}

func assertCleanupRowCount(t *testing.T, store *Store, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := store.sql.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("row count = %d, want %d for %q", got, want, query)
	}
}
