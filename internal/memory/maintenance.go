package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// MaintenanceCounts contains aggregate results from one sweep.
type MaintenanceCounts struct {
	SessionCleanup          SessionCleanupCounts `json:"session_cleanup"`
	PendingDeliveriesFailed int64                `json:"pending_deliveries_failed"`
	CandidatesDeleted       int64                `json:"candidates_deleted"`
	FormationJobsDeleted    int64                `json:"formation_jobs_deleted"`
	CompactionJobsDeleted   int64                `json:"compaction_jobs_deleted"`
	DerivedIndexJobsDeleted int64                `json:"derived_index_jobs_deleted"`
	ChallengesDeleted       int64                `json:"account_challenges_deleted"`
	IndexRowsDeleted        int64                `json:"index_rows_deleted"`
	IndexRevisionsDegraded  int64                `json:"index_revisions_degraded"`
	IndexTablesDropped      int64                `json:"index_tables_dropped"`
	OptimizeRun             bool                 `json:"optimize_run"`
}

// Changed returns the number of rows changed, excluding database hygiene.
func (c MaintenanceCounts) Changed() int64 {
	s := c.SessionCleanup
	return s.SessionTurnsDeleted + s.SessionsDeactivated + s.MemoryEntriesExpired + s.CandidatesDeleted + s.FormationJobsDeleted + s.SessionSummariesDeleted + s.CompactionJobsRetired +
		c.PendingDeliveriesFailed + c.CandidatesDeleted + c.FormationJobsDeleted + c.CompactionJobsDeleted + c.DerivedIndexJobsDeleted + c.ChallengesDeleted + c.IndexRowsDeleted + c.IndexRevisionsDegraded + c.IndexTablesDropped
}

// MaintenanceSweep performs one bounded, serialized retention and consistency pass.
func (s *Store) MaintenanceSweep(ctx context.Context, now time.Time, policy config.RetentionPolicy) (MaintenanceCounts, error) {
	var counts MaintenanceCounts
	var err error
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	policy = normalizedMaintenancePolicy(policy)
	if err := maintenanceForeignKeyCheckDB(ctx, s.sql); err != nil {
		return counts, err
	}
	counts.SessionCleanup, err = s.cleanupExpiredSessions(ctx, now, policy)
	if err != nil {
		return counts, err
	}

	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("begin maintenance sweep: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	if err := maintenanceForeignKeyCheck(ctx, tx); err != nil {
		return counts, err
	}
	batch := policy.BatchSize
	nowText := formatTime(now)
	deadCutoff := formatTime(now.Add(-policy.DeadJobRetention))
	successCutoff := formatTime(now.Add(-policy.SuccessfulJobRetention))
	pendingCutoff := formatTime(now.Add(-policy.PendingDeliveryTimeout))

	if counts.PendingDeliveriesFailed, err = execAffected(ctx, tx, `WITH due AS (SELECT id FROM session_turns WHERE delivered_at IS NULL AND delivery_failed_at IS NULL AND julianday(created_at) <= julianday(?) ORDER BY julianday(created_at), id LIMIT ?) UPDATE session_turns SET delivery_failed_at = ? WHERE id IN (SELECT id FROM due)`, pendingCutoff, batch, nowText); err != nil {
		return counts, err
	}
	if counts.CandidatesDeleted, err = execAffected(ctx, tx, `DELETE FROM memory_candidates WHERE id IN (SELECT candidate.id FROM memory_candidates candidate LEFT JOIN memory_entries published ON published.id = candidate.published_memory_id WHERE (candidate.published_memory_id IS NULL AND julianday(candidate.created_at) <= julianday(?)) OR published.status IN ('expired','superseded') ORDER BY candidate.id LIMIT ?)`, deadCutoff, batch); err != nil {
		return counts, err
	}
	if counts.FormationJobsDeleted, err = execAffected(ctx, tx, `DELETE FROM durable_jobs WHERE id IN (SELECT id FROM durable_jobs WHERE job_kind = 'memory_formation' AND ((state IN ('succeeded','skipped') AND julianday(completed_at) <= julianday(?)) OR (state = 'dead' AND julianday(completed_at) <= julianday(?))) ORDER BY id LIMIT ?)`, successCutoff, deadCutoff, batch); err != nil {
		return counts, err
	}
	if counts.CompactionJobsDeleted, err = execAffected(ctx, tx, `DELETE FROM durable_jobs WHERE id IN (SELECT job.id FROM durable_jobs job WHERE job.job_kind = 'session_compaction' AND ((job.state IN ('succeeded','skipped') AND julianday(job.completed_at) <= julianday(?)) OR (job.state = 'dead' AND julianday(job.completed_at) <= julianday(?))) AND NOT (job.artifact_summary_id IS NULL AND job.state IN ('skipped','dead') AND EXISTS (SELECT 1 FROM sessions active WHERE active.canonical_user_id = job.canonical_user_id AND active.session_id = job.session_id AND active.generation = job.session_generation AND active.is_active = 1 AND julianday(active.expires_at) > julianday(?))) ORDER BY job.id LIMIT ?)`, successCutoff, deadCutoff, nowText, batch); err != nil {
		return counts, err
	}
	if counts.DerivedIndexJobsDeleted, err = execAffected(ctx, tx, `DELETE FROM durable_jobs WHERE id IN (SELECT job.id FROM durable_jobs job WHERE job.job_kind = 'derived_index' AND job.state = 'succeeded' AND julianday(job.completed_at) <= julianday(?) AND NOT (job.operation = 'upsert' AND ((job.entity_kind = 'memory' AND EXISTS (SELECT 1 FROM memory_entries entity WHERE entity.id = job.entity_id AND entity.canonical_user_id = job.canonical_user_id AND entity.status = 'active' AND (entity.expires_at IS NULL OR julianday(entity.expires_at) > julianday(?)))) OR (job.entity_kind = 'session_turn' AND EXISTS (SELECT 1 FROM session_turns entity JOIN sessions active ON active.canonical_user_id = entity.canonical_user_id AND active.session_id = entity.session_id AND active.generation = entity.session_generation WHERE entity.id = job.entity_id AND entity.canonical_user_id = job.canonical_user_id AND entity.delivered_at IS NOT NULL AND entity.delivery_failed_at IS NULL AND active.is_active = 1 AND julianday(active.expires_at) > julianday(?))) OR (job.entity_kind = 'global_memory' AND EXISTS (SELECT 1 FROM global_memories entity WHERE entity.id = job.entity_id))) AND job.id = (SELECT MAX(receipt.id) FROM durable_jobs receipt WHERE receipt.job_kind = 'derived_index' AND receipt.state = 'succeeded' AND receipt.operation = 'upsert' AND receipt.entity_kind = job.entity_kind AND receipt.entity_id = job.entity_id AND receipt.canonical_user_id IS job.canonical_user_id)) ORDER BY job.id LIMIT ?)`, successCutoff, nowText, nowText, batch); err != nil {
		return counts, err
	}
	if counts.ChallengesDeleted, err = execAffected(ctx, tx, `DELETE FROM account_link_challenges WHERE id IN (SELECT id FROM account_link_challenges WHERE julianday(expires_at) <= julianday(?) ORDER BY julianday(expires_at), id LIMIT ?)`, formatTime(now.Add(-policy.AccountChallengeGrace)), batch); err != nil {
		return counts, err
	}
	if err := tx.Commit(); err != nil {
		return counts, fmt.Errorf("commit maintenance retention: %w", err)
	}
	s.signalDerivedIndex()

	indexCounts, indexErr := s.MaintainDerivedIndexes(ctx, now, policy.RetiredIndexRetention, policy.BatchSize)
	counts.IndexRowsDeleted = indexCounts.RowsDeleted
	counts.IndexRevisionsDegraded = indexCounts.RevisionsDegraded
	counts.IndexTablesDropped = indexCounts.TablesDropped
	if indexErr != nil {
		return counts, indexErr
	}
	if err := s.ReconcileDerivedIndexChanges(ctx); err != nil {
		return counts, fmt.Errorf("reconcile derived index outbox: %w", err)
	}
	if err := s.databaseHygiene(ctx, now, policy, &counts); err != nil {
		return counts, err
	}
	s.signalDerivedIndex()
	return counts, nil
}

func normalizedMaintenancePolicy(policy config.RetentionPolicy) config.RetentionPolicy {
	defaults := config.DefaultRetentionPolicy()
	values := []*time.Duration{&policy.RetiredIndexRetention, &policy.SessionInactivity, &policy.PendingDeliveryTimeout, &policy.SuccessfulJobRetention, &policy.DeadJobRetention, &policy.AccountChallengeGrace, &policy.MaintenanceInterval, &policy.DatabaseOptimizeInterval}
	defaultValues := []time.Duration{defaults.RetiredIndexRetention, defaults.SessionInactivity, defaults.PendingDeliveryTimeout, defaults.SuccessfulJobRetention, defaults.DeadJobRetention, defaults.AccountChallengeGrace, defaults.MaintenanceInterval, defaults.DatabaseOptimizeInterval}
	for i := range values {
		if *values[i] <= 0 {
			*values[i] = defaultValues[i]
		}
	}
	if policy.BatchSize <= 0 {
		policy.BatchSize = defaults.BatchSize
	}
	return policy
}

func execAffected(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func maintenanceForeignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("maintenance foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("maintenance foreign key check failed")
	}
	return rows.Err()
}

func maintenanceForeignKeyCheckDB(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("maintenance foreign key precheck: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("maintenance foreign key precheck failed")
	}
	return rows.Err()
}

func (s *Store) databaseHygiene(ctx context.Context, now time.Time, policy config.RetentionPolicy, counts *MaintenanceCounts) error {
	if _, err := s.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return fmt.Errorf("passive WAL checkpoint: %w", err)
	}
	var autoVacuum int
	if err := s.sql.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		return fmt.Errorf("read auto vacuum mode: %w", err)
	}
	if autoVacuum == 2 {
		if _, err := s.sql.ExecContext(ctx, `PRAGMA incremental_vacuum(100)`); err != nil {
			return fmt.Errorf("incremental vacuum: %w", err)
		}
	}
	s.mutationMu.Lock()
	lastOptimize := s.lastOptimizeAt
	s.mutationMu.Unlock()
	if lastOptimize.After(now.Add(-policy.DatabaseOptimizeInterval)) {
		return nil
	}
	if _, err := s.sql.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return fmt.Errorf("optimize database: %w", err)
	}
	s.mutationMu.Lock()
	s.lastOptimizeAt = now
	s.mutationMu.Unlock()
	counts.OptimizeRun = true
	return nil
}
