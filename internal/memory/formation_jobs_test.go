package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

func TestFormationLeaseFenceRollsBackCandidateAndCanonicalWrites(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	jobID, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user")
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.ID != jobID {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	job.LeaseUntil = job.LeaseUntil.Add(time.Second)
	output := evaluatedFormationCandidate(t, "I use Go", "I use Go", "The user uses Go.", policy.CategoryProjects)
	if _, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, IdempotencyKey: "stale", Source: FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, FormationJob: &job}); err == nil {
		t.Fatal("expected stale lease")
	}
	var candidates, memories int
	_ = store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates`).Scan(&candidates)
	_ = store.sql.QueryRow(`SELECT COUNT(*) FROM memory_entries`).Scan(&memories)
	if candidates != 0 || memories != 0 {
		t.Fatalf("candidate=%d memory=%d", candidates, memories)
	}
}

func TestFormationLeaseRenewalAdvancesExactFence(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old := job
	leaseUntil, err := store.RenewFormationJobLease(context.Background(), job, 2*time.Minute)
	if err != nil || !leaseUntil.After(job.LeaseUntil) {
		t.Fatalf("renewed until=%s err=%v", leaseUntil, err)
	}
	if err := store.ValidateFormationJobLease(context.Background(), old); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("old lease validation=%v", err)
	}
	job.LeaseUntil = leaseUntil
	if err := store.ValidateFormationJobLease(context.Background(), job); err != nil {
		t.Fatalf("renewed lease validation=%v", err)
	}
}

func TestFormationJobsRequireExactSuccessfullyDeliveredSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Store, int64)
	}{
		{name: "undelivered", mutate: func(store *Store, turnID int64) {
			if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?`, turnID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "failed", mutate: func(store *Store, turnID int64) {
			if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), turnID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "deleted", mutate: func(store *Store, turnID int64) {
			if _, err := store.sql.Exec(`DELETE FROM session_turns WHERE id = ?`, turnID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFormationTestStore(t)
			turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
			jobID, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user")
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(store, turnID)
			if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID, ExtractorVersion: "other"}, "user"); err == nil {
				t.Fatal("enqueued job from invalid source")
			}
			if _, err := store.ClaimFormationJob(context.Background(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("claim invalid source job %d: %v", jobID, err)
			}
		})
	}

	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "wrong", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err == nil {
		t.Fatal("enqueued source with mismatched request identity")
	}
}

func TestReconcileFormationJobsOnlyUsesSuccessfulExactSources(t *testing.T) {
	store := newFormationTestStore(t)
	successful := seedFormationTurn(t, store, "user", "successful", "I use Go", "successful-request")
	undelivered := seedFormationTurn(t, store, "user", "undelivered", "I use Rust", "undelivered-request")
	failed := seedFormationTurn(t, store, "user", "failed", "I use Java", "failed-request")
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?; UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, undelivered, formatTime(time.Now().UTC()), failed); err != nil {
		t.Fatal(err)
	}
	count, err := store.ReconcileFormationJobs(context.Background(), "model", FormationExtractorVersion)
	if err != nil || count != 1 {
		t.Fatalf("reconciled=%d err=%v", count, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.TurnID != successful || job.RequestID != "successful-request" || job.SessionID != "successful" || job.SessionGeneration != 1 {
		t.Fatalf("reconciled job=%+v", job)
	}
	if _, err := store.ClaimFormationJob(context.Background(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claimed ineligible reconciled source: %v", err)
	}
}

func TestFormationLeaseMutationsFailAfterSourceBecomesUnsuccessful(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.LeaseOwner == "" {
		t.Fatal("claim returned empty lease owner")
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), turnID); err != nil {
		t.Fatal(err)
	}
	assertStale := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrStaleFormationJobLease) {
			t.Fatalf("%s error=%v", name, err)
		}
	}
	_, err = store.FormationJobArtifact(context.Background(), job)
	assertStale("artifact", err)
	assertStale("validate", store.ValidateFormationJobLease(context.Background(), job))
	_, err = store.ReserveFormationModelSubmission(context.Background(), job)
	assertStale("reserve model submission", err)
	assertStale("save", store.SaveFormationJobArtifact(context.Background(), job, `{"memories":[]}`))
	output := evaluatedFormationCandidate(t, "I use Go", "I use Go", "The user uses Go.", policy.CategoryProjects)
	_, _, err = store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, Source: FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, FormationJob: &job})
	assertStale("propose", err)
	assertStale("complete", store.CompleteFormationJob(context.Background(), job, false))
	assertStale("retry", store.RetryFormationJob(context.Background(), job, "transient", 5))
	assertStale("invalid retry", store.RetryInvalidFormationJob(context.Background(), job, "invalid_batch_shape"))
	assertStale("defer", store.DeferFormationJob(context.Background(), job, time.Second))
	assertStale("skip", store.SkipFormationJob(context.Background(), job, "invalid"))
}

func TestRetryInvalidFormationJobUsesDedicatedBudget(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.AttemptCount != 1 || job.InvalidOutputRetryCount != 0 {
		t.Fatalf("initial job=%+v", job)
	}
	if count, err := store.ReserveFormationModelSubmission(context.Background(), job); err != nil || count != 1 {
		t.Fatalf("reserve count=%d err=%v", count, err)
	} else {
		job.ModelSubmissionCount = count
	}
	if err := store.RetryInvalidFormationJob(context.Background(), job, "invalid_batch_shape"); err != nil {
		t.Fatal(err)
	}
	var state, leaseOwner, code string
	var attemptCount, invalidRetryCount, submissionCount int
	var leaseUntil sql.NullString
	if err := store.sql.QueryRow(`SELECT state, attempt_count, invalid_output_retry_count, model_submission_count, lease_owner, lease_until, corrective_error_code FROM durable_jobs WHERE id = ?`, job.ID).Scan(&state, &attemptCount, &invalidRetryCount, &submissionCount, &leaseOwner, &leaseUntil, &code); err != nil {
		t.Fatal(err)
	}
	if state != "retry" || attemptCount != 1 || invalidRetryCount != 1 || submissionCount != 1 || leaseOwner != "" || leaseUntil.Valid || code != "invalid_batch_shape" {
		t.Fatalf("retried state=%q attempts=%d invalid_retries=%d submissions=%d lease_owner=%q lease_until=%v code=%q", state, attemptCount, invalidRetryCount, submissionCount, leaseOwner, leaseUntil, code)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.AttemptCount != 2 || reclaimed.InvalidOutputRetryCount != 1 || reclaimed.ModelSubmissionCount != 1 || reclaimed.CorrectiveErrorCode != "invalid_batch_shape" {
		t.Fatalf("reclaimed job=%+v", reclaimed)
	}
	if err := store.RetryInvalidFormationJob(context.Background(), reclaimed, "all_items_malformed"); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("second invalid retry error=%v", err)
	}
	if err := store.DeferFormationJob(context.Background(), reclaimed, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT attempt_count, invalid_output_retry_count, model_submission_count, corrective_error_code FROM durable_jobs WHERE id = ?`, job.ID).Scan(&attemptCount, &invalidRetryCount, &submissionCount, &code); err != nil {
		t.Fatal(err)
	}
	if code != "invalid_batch_shape" {
		t.Fatalf("deferred structured retry lost corrective reason: %q", code)
	}
	if attemptCount != 1 || invalidRetryCount != 1 || submissionCount != 1 {
		t.Fatalf("preempted attempts=%d invalid_retries=%d submissions=%d", attemptCount, invalidRetryCount, submissionCount)
	}
}

func TestFormationModelSubmissionBudgetCannotBeResetByClaims(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for submission := 1; submission <= DurableModelSubmissionLimit; submission++ {
		count, reserveErr := store.ReserveFormationModelSubmission(context.Background(), job)
		if reserveErr != nil || count != submission {
			t.Fatalf("submission=%d count=%d err=%v", submission, count, reserveErr)
		}
		job.ModelSubmissionCount = count
		if submission == DurableModelSubmissionLimit {
			break
		}
		if err := store.RetryFormationJob(context.Background(), job, "transient_provider", DurableModelSubmissionLimit); err != nil {
			t.Fatal(err)
		}
		if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
			t.Fatal(err)
		}
		job, err = store.ClaimFormationJob(context.Background(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ReserveFormationModelSubmission(context.Background(), job); !errors.Is(err, ErrModelSubmissionBudgetExhausted) {
		t.Fatalf("fourth reservation error=%v", err)
	}
}

func TestInvalidOutputRetrySurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newTestStore(path, config.NewLogger(config.LevelError))
	seedAccountUsers(t, store, "user")
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	count, err := store.ReserveFormationModelSubmission(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	job.ModelSubmissionCount = count
	if err := store.RetryInvalidFormationJob(context.Background(), job, "missing_tool_call"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newTestStore(path, config.NewLogger(config.LevelError))
	t.Cleanup(func() { reopened.Close() })
	if _, err := reopened.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := reopened.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.InvalidOutputRetryCount != 1 || reclaimed.AttemptCount != 2 || reclaimed.ModelSubmissionCount != 1 || reclaimed.CorrectiveErrorCode != "missing_tool_call" {
		t.Fatalf("reopened job=%+v", reclaimed)
	}
}

func TestReclaimedFormationLeaseRejectsStaleOwnerAtExactLeaseUntil(t *testing.T) {
	store := newFormationTestStore(t)
	turnID := seedFormationTurn(t, store, "user", "session", "I use Go", "request")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turnID}, "user"); err != nil {
		t.Fatal(err)
	}
	stale, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), stale.ID); err != nil {
		t.Fatal(err)
	}
	current, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if current.LeaseOwner == stale.LeaseOwner || current.LeaseOwner == "" {
		t.Fatalf("stale owner=%q current owner=%q", stale.LeaseOwner, current.LeaseOwner)
	}
	stale.LeaseUntil = current.LeaseUntil
	if err := store.SaveFormationJobArtifact(context.Background(), stale, `{"memories":[]}`); !errors.Is(err, ErrStaleFormationJobLease) {
		t.Fatalf("stale reclaimed lease saved artifact: %v", err)
	}
	if err := store.SaveFormationJobArtifact(context.Background(), current, `{"memories":[]}`); err != nil {
		t.Fatalf("current lease failed: %v", err)
	}
}
