package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrStaleDerivedIndexChangeLease indicates that a claimed outbox lease was
// released or reclaimed by another worker.
var ErrStaleDerivedIndexChangeLease = errors.New("stale derived index change lease")

// DerivedIndexChange is one leased canonical mutation from the durable outbox.
type DerivedIndexChange struct {
	Sequence, EntityID            int64
	UserID, EntityKind, Operation string
	AttemptCount                  int
	LeaseOwner                    string
	LeaseUntil                    time.Time
}

// ClaimDerivedIndexChange leases the oldest durable change.
func (s *Store) ClaimDerivedIndexChange(ctx context.Context, lease time.Duration) (DerivedIndexChange, error) {
	if lease <= 0 {
		lease = time.Minute
	}
	owner, err := newLeaseOwner()
	if err != nil {
		return DerivedIndexChange{}, fmt.Errorf("create derived index lease owner: %w", err)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return DerivedIndexChange{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	var change DerivedIndexChange
	var userID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, canonical_user_id, entity_kind, entity_id, operation, attempt_count FROM durable_jobs WHERE job_kind = 'derived_index' AND ((state IN ('queued', 'retry') AND available_at <= ?) OR (state = 'running' AND lease_until <= ?)) ORDER BY id LIMIT 1`, formatTime(now), formatTime(now)).Scan(&change.Sequence, &userID, &change.EntityKind, &change.EntityID, &change.Operation, &change.AttemptCount)
	if err != nil {
		return DerivedIndexChange{}, err
	}
	change.UserID = userID.String
	result, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET state = 'running', attempt_count = attempt_count + 1, lease_owner = ?, lease_until = ?, updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND ((state IN ('queued', 'retry') AND available_at <= ?) OR (state = 'running' AND lease_until <= ?))`, owner, formatTime(leaseUntil), formatTime(now), change.Sequence, formatTime(now), formatTime(now))
	if err != nil {
		return DerivedIndexChange{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return DerivedIndexChange{}, sql.ErrNoRows
	}
	change.AttemptCount++
	change.LeaseOwner = owner
	change.LeaseUntil = leaseUntil
	if err := tx.Commit(); err != nil {
		return DerivedIndexChange{}, err
	}
	return change, nil
}

// RenewDerivedIndexChangeLease extends an exactly owned, still-live outbox lease.
func (s *Store) RenewDerivedIndexChangeLease(ctx context.Context, change DerivedIndexChange, lease time.Duration) error {
	if lease <= 0 {
		return fmt.Errorf("renew derived index lease: duration must be positive")
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET lease_until = ?, updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND state = 'running' AND lease_owner = ? AND julianday(lease_until) > julianday(?)`, formatTime(now.Add(lease)), formatTime(now), change.Sequence, change.LeaseOwner, formatTime(now))
	return requireDerivedIndexLeaseMutation(result, err)
}

// CompleteDerivedIndexChange acknowledges a leased change.
func (s *Store) CompleteDerivedIndexChange(ctx context.Context, change DerivedIndexChange) error {
	now := formatTime(time.Now().UTC())
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'succeeded', completed_at = ?, lease_owner = '', lease_until = NULL, last_error_code = '', updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND state = 'running' AND lease_owner = ? AND julianday(lease_until) > julianday(?)`, now, now, change.Sequence, change.LeaseOwner, now)
	return requireDerivedIndexLeaseMutation(result, err)
}

// RetryDerivedIndexChange durably releases a failed change with backoff.
func (s *Store) RetryDerivedIndexChange(ctx context.Context, change DerivedIndexChange, code string) error {
	now := time.Now().UTC()
	delay := time.Duration(1<<min(change.AttemptCount, 6)) * time.Second
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'retry', available_at = ?, lease_owner = '', lease_until = NULL, last_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND state = 'running' AND lease_owner = ?`, formatTime(now.Add(delay)), safeErrorCode(code), formatTime(now), change.Sequence, change.LeaseOwner)
	return requireDerivedIndexLeaseMutation(result, err)
}

func requireDerivedIndexLeaseMutation(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStaleDerivedIndexChangeLease
	}
	return nil
}

// ReconcileDerivedIndexChanges restores abandoned leases and inserts missing
// idempotent changes for every currently indexable canonical record.
func (s *Store) ReconcileDerivedIndexChanges(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.sql.ExecContext(ctx, `
UPDATE durable_jobs SET state = 'retry', available_at = ?, lease_owner = '', lease_until = NULL, updated_at = ? WHERE job_kind = 'derived_index' AND state = 'running' AND lease_until <= ?;
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'reconcile:memory:' || entity.id || ':' || entity.updated_at, entity.canonical_user_id, 'memory', entity.id, 'upsert', ?, ? FROM memory_entries entity WHERE entity.status = 'active' AND (entity.expires_at IS NULL OR entity.expires_at > ?) AND NOT EXISTS (SELECT 1 FROM durable_jobs receipt WHERE receipt.job_kind = 'derived_index' AND receipt.state = 'succeeded' AND receipt.operation = 'upsert' AND receipt.entity_kind = 'memory' AND receipt.entity_id = entity.id AND receipt.canonical_user_id = entity.canonical_user_id)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING;
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'reconcile:turn:' || turns.id || ':' || turns.delivered_at, turns.canonical_user_id, 'session_turn', turns.id, 'upsert', ?, ? FROM session_turns turns JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ? AND NOT EXISTS (SELECT 1 FROM durable_jobs receipt WHERE receipt.job_kind = 'derived_index' AND receipt.state = 'succeeded' AND receipt.operation = 'upsert' AND receipt.entity_kind = 'session_turn' AND receipt.entity_id = turns.id AND receipt.canonical_user_id = turns.canonical_user_id)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING;
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'reconcile:global_memory:' || entity.id || ':' || entity.created_at, NULL, 'global_memory', entity.id, 'upsert', ?, ? FROM global_memories entity WHERE NOT EXISTS (SELECT 1 FROM durable_jobs receipt WHERE receipt.job_kind = 'derived_index' AND receipt.state = 'succeeded' AND receipt.operation = 'upsert' AND receipt.entity_kind = 'global_memory' AND receipt.entity_id = entity.id AND receipt.canonical_user_id IS NULL)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING;`, now, now, now, now, now, now, now, now, now, now, now)
	return err
}

func enqueueDerivedChangeTx(ctx context.Context, tx *sql.Tx, userID, entityKind string, entityID int64, operation, token string) error {
	if entityID <= 0 || (entityKind != "memory" && entityKind != "session_turn") || (operation != "upsert" && operation != "delete") {
		return fmt.Errorf("invalid derived index change")
	}
	now := formatTime(time.Now().UTC())
	key := formationKey("derived-index", entityKind, entityID, operation, token)
	_, err := tx.ExecContext(ctx, `INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at) VALUES ('derived_index', ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(job_kind, idempotency_key) DO NOTHING`, key, userID, entityKind, entityID, operation, now, now)
	if err != nil {
		return fmt.Errorf("enqueue derived index change: %w", err)
	}
	return nil
}
