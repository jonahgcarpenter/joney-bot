package memory

import (
	"context"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestJobHealthEmptyClaimedAndUnavailable(t *testing.T) {
	store := newFormationTestStore(t)
	ctx := context.Background()
	initial, err := store.JobHealth(ctx)
	if err != nil || len(initial) != 3 {
		t.Fatalf("empty snapshot=%v err=%v", initial, err)
	}
	for _, row := range initial {
		if row.Queued+row.Running+row.Retry+row.Dead != 0 {
			t.Fatalf("nonempty initial snapshot: %+v", row)
		}
	}
	turn := seedFormationTurn(t, store, "user", "session", "synthetic source", "request")
	if _, err := store.EnqueueFormationJob(ctx, FormationSource{RequestID: "request", SessionID: "session", SessionGeneration: 1, TurnID: turn}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.RetryFormationJob(ctx, job, "transient_runtime", 5)
	if err != nil || state != "retry" {
		t.Fatalf("transition=%q err=%v", state, err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ?`, formatTime(time.Now().Add(-time.Minute)), job.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := store.JobHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Kind == "memory_formation" && (row.Retry != 1 || row.Running != 0 || row.OldestReadyAgeMS < 59000) {
			t.Fatalf("retry snapshot=%+v", row)
		}
	}
	job, err = store.ClaimFormationJob(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, expired := range []bool{false, true} {
		if expired {
			if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ?`, formatTime(time.Now().Add(-time.Minute)), job.ID); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := store.JobHealth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			want := int64(0)
			if expired && row.Kind == "memory_formation" {
				want = 1
			}
			if row.ExpiredLeaseCount != want {
				t.Fatalf("expired=%v gauge=%+v", expired, row)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.JobHealth(ctx); err == nil || rows != nil {
		t.Fatalf("unavailable snapshot=%v err=%v", rows, err)
	}
}

func TestMaintenanceRollbackReportsOnlyPriorCommittedPhase(t *testing.T) {
	store := newFormationTestStore(t)
	ctx := context.Background()
	seedFormationTurn(t, store, "user", "expired", "synthetic expired turn", "old")
	pending := seedFormationTurn(t, store, "user", "active", "synthetic pending turn", "pending")
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE session_id = 'expired'; UPDATE session_turns SET delivered_at = NULL, created_at = ? WHERE id = ?`, formatTime(now.Add(-time.Hour)), formatTime(now.Add(-time.Hour)), pending); err != nil {
		t.Fatal(err)
	}
	// The retention transaction updates pending deliveries before this deletion.
	for _, key := range []string{"expiry", "retention"} {
		if _, err := store.sql.Exec(`INSERT INTO memory_candidates(canonical_user_id,idempotency_key,state,scope,category,statement,evidence,confidence,importance,provenance_type,extractor_version,formation_mode,sensitivity,created_at,updated_at,decision_reason,claim_slot,claim_value) VALUES ('user',?,'rejected','long_term','notes','synthetic','synthetic',0.2,2,'user_statement','v1','automatic','low',?,?,'rejected','notes.test','test')`, key, formatTime(now.Add(-800*time.Hour)), formatTime(now.Add(-800*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.sql.Exec(`CREATE TRIGGER fail_retention BEFORE DELETE ON memory_candidates WHEN OLD.idempotency_key = 'retention' BEGIN SELECT RAISE(ABORT, 'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultRetentionPolicy()
	policy.BatchSize = 1
	counts, err := store.MaintenanceSweep(ctx, now, policy)
	if err == nil || counts.Phase != "retention" {
		t.Fatalf("phase=%q err=%v", counts.Phase, err)
	}
	if counts.PendingDeliveriesFailed != 0 || counts.CandidatesDeleted != 0 || counts.SessionCleanup.SessionTurnsDeleted != 1 {
		t.Fatalf("uncommitted counts leaked: %+v", counts)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ? AND delivery_failed_at IS NULL`, 1, pending)
	if !store.LastMaintenance().IsZero() {
		t.Fatal("failed sweep recorded success timestamp")
	}
}
