package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestDerivedIndexOutboxIdempotencyRetryAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newTestStore(path, config.NewLogger(config.LevelError))
	seedAccountUsers(t, store, "user")
	tx, err := store.sql.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueDerivedChangeTx(context.Background(), tx, "user", "memory", 42, "upsert", "same-mutation"); err != nil {
		t.Fatal(err)
	}
	if err := enqueueDerivedChangeTx(context.Background(), tx, "user", "memory", 42, "upsert", "same-mutation"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("idempotent count=%d err=%v", count, err)
	}
	change, err := store.ClaimDerivedIndexChange(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RetryDerivedIndexChange(context.Background(), change, "offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ? AND job_kind = 'derived_index'`, formatTime(time.Now().Add(-time.Second)), change.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path, nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reclaimed, err := store.ClaimDerivedIndexChange(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Sequence != change.Sequence || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaimed=%+v initial=%+v", reclaimed, change)
	}
	if err := store.CompleteDerivedIndexChange(context.Background(), reclaimed); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedIndexLeaseRenewalKeepsOwnershipLive(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	seedAccountUsers(t, store, "user")
	tx, err := store.sql.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueDerivedChangeTx(context.Background(), tx, "user", "memory", 42, "upsert", "renew"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	change, err := store.ClaimDerivedIndexChange(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RenewDerivedIndexChangeLease(context.Background(), change, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDerivedIndexChange(context.Background(), change); err != nil {
		t.Fatalf("complete renewed lease: %v", err)
	}
}

func TestClaimGlobalDerivedIndexChangeAllowsNullCanonicalUser(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	ctx := context.Background()
	if _, err := store.sql.Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('shared fact', 'key', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileDerivedIndexChanges(ctx); err != nil {
		t.Fatal(err)
	}
	change, err := store.ClaimDerivedIndexChange(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if change.EntityKind != "global_memory" || change.UserID != "" || change.EntityID <= 0 {
		t.Fatalf("global change=%+v", change)
	}
}

func TestReclaimedDerivedIndexLeaseRejectsStaleWorker(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueDerivedChangeTx(ctx, tx, "user", "memory", 42, "delete", "stale-lease"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stale, err := store.ClaimDerivedIndexChange(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(-time.Second)), stale.Sequence); err != nil {
		t.Fatal(err)
	}
	current, err := store.ClaimDerivedIndexChange(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if stale.LeaseOwner == current.LeaseOwner || current.LeaseOwner == "" {
		t.Fatalf("stale owner=%q current owner=%q", stale.LeaseOwner, current.LeaseOwner)
	}
	if err := store.CompleteDerivedIndexChange(ctx, stale); !errors.Is(err, ErrStaleDerivedIndexChangeLease) {
		t.Fatalf("stale complete error=%v", err)
	}
	if err := store.RetryDerivedIndexChange(ctx, stale, "stale"); !errors.Is(err, ErrStaleDerivedIndexChangeLease) {
		t.Fatalf("stale retry error=%v", err)
	}
	if err := store.CompleteDerivedIndexChange(ctx, current); err != nil {
		t.Fatal(err)
	}
}
