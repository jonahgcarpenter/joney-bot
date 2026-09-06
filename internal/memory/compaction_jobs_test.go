package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestSessionCompactionRangeAndEnqueueIdempotency(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user-a", "user-b")
	generation := activateCompactionSession(t, store, "user-a", "shared")
	activateCompactionSession(t, store, "user-b", "shared")
	first := appendDeliveredCompactionTurn(t, store, "user-a", "shared", generation, "one")
	second := appendDeliveredCompactionTurn(t, store, "user-a", "shared", generation, "two")
	_ = appendDeliveredCompactionTurn(t, store, "user-b", "shared", generation, "private")

	planned, err := store.CompactionWindowAfter(context.Background(), "user-a", "shared", generation, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if planned.TotalCount != 2 || planned.NewestTurnID != second || len(planned.Turns) != 1 || planned.Turns[0].ID != first {
		t.Fatalf("planned turns = %+v", planned)
	}
	ranged, err := store.DeliveredSessionTurnsRange(context.Background(), "user-a", "shared", generation, first, second)
	if err != nil || len(ranged) != 2 || ranged[0].ID != first || ranged[1].ID != second {
		t.Fatalf("range = %+v, err = %v", ranged, err)
	}
	firstJob, err := store.EnqueueSessionCompactionJob(context.Background(), "user-a", "shared", generation, first, second, second, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := store.EnqueueSessionCompactionJob(context.Background(), "user-a", "shared", generation, first, second, second, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || secondJob != firstJob {
		t.Fatalf("idempotent job = %d, want %d, err = %v", secondJob, firstJob, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user-b", "shared", generation, first, second, second, compactionTestModel, compactionTestGeneratorVersion); err == nil {
		t.Fatal("expected cross-tenant range rejection")
	}
}

func TestSessionCompactionSuppressesOverlappingContractJobs(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	second := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")
	third := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "three")
	firstJob, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, second, second, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	overlap, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, third, third, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || overlap != firstJob {
		t.Fatalf("overlap job=%d want=%d err=%v", overlap, firstJob, err)
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction'`, 1)
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SkipSessionCompactionJob(context.Background(), job, "invalid_output"); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, third, third, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || blocked != firstJob {
		t.Fatalf("terminal contract blocker=%d want=%d err=%v", blocked, firstJob, err)
	}
	replacement, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, third, third, compactionTestModel, "session-summary-v2")
	if err != nil || replacement == 0 || replacement == firstJob {
		t.Fatalf("replacement=%d first=%d err=%v", replacement, firstJob, err)
	}
}

func TestSessionCompactionInvalidOutputRetryIsDedicatedAndDurable(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	count, err := store.ReserveSessionCompactionModelSubmission(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	job.ModelSubmissionCount = count
	if err := store.RetryInvalidSessionCompactionJob(context.Background(), job, "missing_tool_call"); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts, invalidRetries int
	var submissions int
	if err := store.sql.QueryRow(`SELECT state, attempt_count, compaction_invalid_output_retry_count, model_submission_count FROM durable_jobs WHERE id = ?`, job.ID).Scan(&state, &attempts, &invalidRetries, &submissions); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attempts != 1 || invalidRetries != 1 || submissions != 1 {
		t.Fatalf("state=%q attempts=%d invalid_retries=%d submissions=%d", state, attempts, invalidRetries, submissions)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimSessionCompactionJob(context.Background(), "worker-2", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || reclaimed.InvalidOutputRetryCount != 1 || reclaimed.AttemptCount != 2 || reclaimed.ModelSubmissionCount != 1 || reclaimed.CorrectiveErrorCode != "missing_tool_call" {
		t.Fatalf("reclaimed=%+v err=%v", reclaimed, err)
	}
	if err := store.DeferSessionCompactionJob(context.Background(), reclaimed, time.Second); err != nil {
		t.Fatal(err)
	}
	var code string
	if err := store.sql.QueryRow(`SELECT corrective_error_code FROM durable_jobs WHERE id = ?`, job.ID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "missing_tool_call" {
		t.Fatalf("deferred structured retry lost corrective reason: %q", code)
	}
}

func TestSessionCompactionReconcilePreservesStructuredRetryReason(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	count, err := store.ReserveSessionCompactionModelSubmission(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	job.ModelSubmissionCount = count
	if err := store.RetryInvalidSessionCompactionJob(context.Background(), job, "missing_tool_call"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'running', attempt_count = 1, lease_owner = 'expired-worker', lease_until = ?, available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileSessionCompactionJobs(context.Background(), compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	var state, code string
	if err := store.sql.QueryRow(`SELECT state, corrective_error_code FROM durable_jobs WHERE id = ?`, job.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || code != "missing_tool_call" {
		t.Fatalf("state=%q code=%q", state, code)
	}
}

func TestSessionCompactionReconcileSupersedesActiveOldContract(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	oldID, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, "old-model", "old-version")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReconcileSessionCompactionJobs(context.Background(), compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || changed != 1 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	var state, code string
	if err := store.sql.QueryRow(`SELECT state, last_error_code FROM durable_jobs WHERE id = ?`, oldID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || code != "superseded_compaction_contract" {
		t.Fatalf("state=%q code=%q", state, code)
	}
	currentID, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || currentID == oldID {
		t.Fatalf("current=%d old=%d err=%v", currentID, oldID, err)
	}
}

func TestSessionCompactionArtifactRetriesSaturateLegacyAttemptCount(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	jobID, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker-0", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: "Stable artifact", GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 4; attempt++ {
		if err := store.RetrySessionCompactionJob(context.Background(), job, "transient_storage"); err != nil {
			t.Fatalf("retry %d: %v", attempt, err)
		}
		if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().Add(-time.Second)), jobID); err != nil {
			t.Fatal(err)
		}
		job, err = store.ClaimSessionCompactionJob(context.Background(), fmt.Sprintf("worker-%d", attempt), time.Minute, compactionTestModel, compactionTestGeneratorVersion)
		if err != nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
	}
	if job.AttemptCount != SessionCompactionModelSubmissionLimit {
		t.Fatalf("saturated attempt count=%d", job.AttemptCount)
	}
}

func TestExpiredSessionGenerationCannotCompact(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turn := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), "user", turn); err == nil {
		t.Fatal("expired generation accepted delivery mark")
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turn, turn, turn, compactionTestModel, compactionTestGeneratorVersion); err == nil {
		t.Fatal("expired generation accepted compaction job")
	}
}

func TestPreCompactionCandidateRequiresLiveJobLease(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turn := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "I work on Atlas.")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turn, turn, turn, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(-time.Minute)), job.ID); err != nil {
		t.Fatal(err)
	}
	output, err := policy.Evaluate(policy.CandidateInput{SourceUserText: "I work on Atlas.", Statement: "The user works on Atlas.", Evidence: "I work on Atlas.", Scope: policy.ScopeLongTerm, Category: policy.CategoryProjects, Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityModel, Sensitivity: policy.SensitivityLow, Mode: policy.ModePreCompactionExtraction, Context: policy.ContextDirectAssertion, Confidence: 0.9, Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, Source: FormationSource{RequestID: "session-compaction:1", SessionID: "session", SessionGeneration: generation, TurnID: turn, Model: "model", ExtractorVersion: "v1"}, IdempotencyKey: "stale-lease", CompactionJob: &job}); err == nil {
		t.Fatal("stale lease staged pre-compaction candidate")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM memory_candidates WHERE idempotency_key = 'stale-lease'`, 0)
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(time.Minute)), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, Source: FormationSource{RequestID: "session-compaction:1", SessionID: "other-session", SessionGeneration: generation, TurnID: turn, Model: "model", ExtractorVersion: "v1"}, IdempotencyKey: "wrong-scope", CompactionJob: &job}); err == nil {
		t.Fatal("mismatched session staged pre-compaction candidate")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM memory_candidates WHERE idempotency_key = 'wrong-scope'`, 0)
}

func TestPreCompactionCandidateLifecyclePublicationAndStaleLeaseFence(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	belowTurn := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "I work on Atlas.")
	approvedTurn := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "I prefer tea.")
	rejectedTurn := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "My coworker prefers coffee.")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, belowTurn, rejectedTurn, rejectedTurn, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	evaluate := func(source, statement, evidence, category, slot, value string, confidence float64) policy.CandidateOutput {
		t.Helper()
		output, err := policy.Evaluate(policy.CandidateInput{
			SourceUserText: source, Statement: statement, Evidence: evidence,
			Scope: policy.ScopeLongTerm, Category: policy.Category(category),
			Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityModel,
			Sensitivity: policy.SensitivityLow, Mode: policy.ModePreCompactionExtraction,
			Context: policy.ContextDirectAssertion, Confidence: confidence, Importance: 4,
			ClaimSlot: slot, ClaimValue: value,
		})
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	propose := func(key string, turnID int64, output policy.CandidateOutput) FormationCandidate {
		t.Helper()
		candidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{
			Output: output, IdempotencyKey: key, CompactionJob: &job,
			Source: FormationSource{RequestID: "session-compaction:" + fmt.Sprint(job.ID), SessionID: job.SessionID, SessionGeneration: generation, TurnID: turnID, Model: "model", ExtractorVersion: "v2"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return candidate
	}
	below := propose("compact-below", belowTurn, evaluate("I work on Atlas.", "The user works on Atlas.", "I work on Atlas.", "projects", "project.name", "Atlas", 0.349))
	if below.State != "proposed" || below.PublishedMemoryID != 0 {
		t.Fatalf("below-threshold candidate=%+v", below)
	}
	approved := propose("compact-approved", approvedTurn, evaluate("I prefer tea.", "The user prefers tea.", "I prefer tea.", "durable_preferences", "preference.drink", "tea", 0.35))
	if approved.State != "approved" || approved.PublishedMemoryID == 0 {
		t.Fatalf("approved candidate=%+v", approved)
	}
	memory, err := store.EntryByID(approved.PublishedMemoryID)
	if err != nil || memory.Status != "active" || memory.ClaimSlot != "preference.drink" {
		t.Fatalf("published memory=%+v err=%v", memory, err)
	}
	published, err := store.loadFixtureCandidate(context.Background(), "user", approved.ID)
	if err != nil || published.PublishedMemoryID != memory.ID {
		t.Fatalf("published candidate=%+v err=%v", published, err)
	}
	rejected := propose("compact-rejected", rejectedTurn, evaluate("My coworker prefers coffee.", "The user prefers coffee.", "My coworker prefers coffee.", "durable_preferences", "preference.drink", "coffee", 0.99))
	if rejected.State != "rejected" || rejected.PublishedMemoryID != 0 {
		t.Fatalf("unsound candidate=%+v", rejected)
	}
}

func TestStaleCompactionProposalCannotReconcilePublishedCandidateAfterSameOwnerReclaim(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "I prefer tea.")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	staleJob, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	evaluate := func(confidence float64, importance int) policy.CandidateOutput {
		t.Helper()
		output, err := policy.Evaluate(policy.CandidateInput{
			SourceUserText: "I prefer tea.", Statement: "The user prefers tea.", Evidence: "I prefer tea.",
			Scope: policy.ScopeLongTerm, Category: policy.CategoryDurablePreferences,
			Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityModel,
			Sensitivity: policy.SensitivityLow, Mode: policy.ModePreCompactionExtraction,
			Context: policy.ContextDirectAssertion, Confidence: confidence, Importance: importance,
			ClaimSlot: "preference.drink", ClaimValue: "tea",
		})
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	source := FormationSource{RequestID: fmt.Sprintf("session-compaction:%d", staleJob.ID), SessionID: staleJob.SessionID, SessionGeneration: generation, TurnID: turnID, Model: "model", ExtractorVersion: "v2"}
	candidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: evaluate(0.7, 3), IdempotencyKey: "published-before-reclaim", Source: source, CompactionJob: &staleJob})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.EntryByID(candidate.PublishedMemoryID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(-time.Minute)), staleJob.ID); err != nil {
		t.Fatal(err)
	}
	currentJob, err := store.ClaimSessionCompactionJob(context.Background(), staleJob.LeaseOwner, 2*time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || currentJob.LeaseUntil.Equal(staleJob.LeaseUntil) {
		t.Fatalf("reclaimed job=%+v err=%v", currentJob, err)
	}
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: evaluate(0.95, 5), IdempotencyKey: "stale-reconcile", Source: source, CompactionJob: &staleJob}); err == nil {
		t.Fatal("stale proposal reconciled under the same owner's newer lease")
	}
	loaded, err := store.EntryByID(memory.ID)
	if err != nil || loaded.Confidence != 0.7 || loaded.Importance != 3 || loaded.EvidenceCount != 1 {
		t.Fatalf("published memory mutated by stale reconciliation: %+v err=%v", loaded, err)
	}
	loadedCandidate, err := store.loadFixtureCandidate(context.Background(), "user", candidate.ID)
	if err != nil || loadedCandidate.PublishedMemoryID == 0 || loadedCandidate.Confidence != 0.7 {
		t.Fatalf("published candidate mutated by stale reconciliation: %+v err=%v", loadedCandidate, err)
	}
}

func TestSessionCompactionLeaseRenewalAdvancesExactFence(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "I prefer tea.")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	leaseUntil, err := store.RenewSessionCompactionJobLease(context.Background(), job, 2*time.Minute)
	if err != nil || !leaseUntil.After(job.LeaseUntil) {
		t.Fatalf("renewed until=%s err=%v", leaseUntil, err)
	}
	if _, err := store.RenewSessionCompactionJobLease(context.Background(), job, 2*time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale lease renewed: %v", err)
	}
	if _, err := store.ReserveSessionCompactionModelSubmission(context.Background(), job); !errors.Is(err, ErrStaleSessionCompactionJobLease) {
		t.Fatalf("stale lease reserved model submission: %v", err)
	}
	job.LeaseUntil = leaseUntil
	if _, err := store.RenewSessionCompactionJobLease(context.Background(), job, 2*time.Minute); err != nil {
		t.Fatalf("renew current lease: %v", err)
	}
}

func TestSessionCompactionLeaseRetryStopsAtSubmissionLimit(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= SessionCompactionModelSubmissionLimit; attempt++ {
		job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
		if err != nil || job.AttemptCount != attempt {
			t.Fatalf("attempt %d job = %+v, err = %v", attempt, job, err)
		}
		count, err := store.ReserveSessionCompactionModelSubmission(context.Background(), job)
		if err != nil || count != attempt {
			t.Fatalf("attempt %d reserve count=%d err=%v", attempt, count, err)
		}
		job.ModelSubmissionCount = count
		if err := store.RetrySessionCompactionJob(context.Background(), job, "transient_provider"); err != nil {
			t.Fatal(err)
		}
		if attempt < SessionCompactionModelSubmissionLimit {
			if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(-time.Second)), job.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim dead job error = %v", err)
	}
	var submissions int
	if err := store.sql.QueryRow(`SELECT model_submission_count FROM durable_jobs WHERE job_kind = 'session_compaction'`).Scan(&submissions); err != nil || submissions != SessionCompactionModelSubmissionLimit {
		t.Fatalf("submissions=%d err=%v", submissions, err)
	}
}
