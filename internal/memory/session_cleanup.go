package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// SessionCleanupCounts reports rows removed by one session cleanup transaction.
type SessionCleanupCounts struct {
	SessionTurnsDeleted     int64
	SessionsDeactivated     int64 `json:"TenantSessionsDeleted"`
	MemoryEntriesExpired    int64
	CandidatesDeleted       int64 `json:"CandidatesErased"`
	FormationJobsDeleted    int64
	SessionSummariesDeleted int64
	CompactionJobsRetired   int64 `json:"CompactionJobsDeleted"`
}

func (s *Store) cleanupExpiredSessions(ctx context.Context, now time.Time, policy config.RetentionPolicy) (counts SessionCleanupCounts, err error) {
	defer func() {
		if err != nil {
			counts = SessionCleanupCounts{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return counts, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	batch := policy.BatchSize
	if batch <= 0 {
		batch = 100
	}
	nowText := formatTime(now)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("begin expired session cleanup: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	expiringMemories, err := memoryIDsTx(tx, `SELECT id FROM memory_entries WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at <= ? ORDER BY expires_at, id LIMIT ?`, nowText, batch)
	if err != nil {
		return counts, fmt.Errorf("enumerate expiring memories: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
UPDATE memory_entries
SET status = 'expired', statement = '', claim_slot = '', claim_value = '',
	updated_at = ?
WHERE id IN (SELECT id FROM memory_entries WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at <= ? ORDER BY expires_at, id LIMIT ?)
`, nowText, nowText, batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("expire durable memories: %w", err)
	}
	if counts.MemoryEntriesExpired, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count expired durable memories: %w", err)
	}
	for _, id := range expiringMemories {
		var userID string
		if err := tx.QueryRowContext(ctx, `SELECT canonical_user_id FROM memory_entries WHERE id = ?`, id).Scan(&userID); err != nil {
			return counts, err
		}
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", id, "delete", "expire:"+nowText); err != nil {
			return counts, err
		}
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM memory_candidates WHERE id IN (SELECT id FROM memory_candidates WHERE published_memory_id IS NULL AND ((expires_at IS NOT NULL AND expires_at <= ?) OR created_at <= ?) ORDER BY id LIMIT ?)`, nowText, formatTime(now.Add(-policy.DeadJobRetention)), batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete expired memory candidates: %w", err)
	}
	if counts.CandidatesDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted memory candidates: %w", err)
	}
	result, err = tx.ExecContext(ctx, `
DELETE FROM durable_jobs
WHERE job_kind = 'memory_formation' AND id IN (SELECT id FROM durable_jobs WHERE job_kind = 'memory_formation' AND ((state IN ('succeeded', 'skipped') AND completed_at <= ?)
	OR (state = 'dead' AND completed_at <= ?))
	ORDER BY id LIMIT ?)
`, formatTime(now.Add(-policy.SuccessfulJobRetention)), formatTime(now.Add(-policy.DeadJobRetention)), batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete retained memory formation jobs: %w", err)
	}
	if counts.FormationJobsDeleted, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted memory formation jobs: %w", err)
	}

	result, err = tx.ExecContext(ctx, `
UPDATE durable_jobs
SET state = CASE WHEN state IN ('queued','running','retry') THEN 'skipped' ELSE state END,
	artifact_summary_id = NULL, lease_owner = '', lease_until = NULL,
	completed_at = CASE WHEN state IN ('queued','running','retry') THEN COALESCE(completed_at, ?) ELSE completed_at END,
	updated_at = CASE WHEN state IN ('queued','running','retry') THEN ? ELSE updated_at END
WHERE job_kind = 'session_compaction' AND id IN (SELECT id FROM durable_jobs jobs WHERE jobs.job_kind = 'session_compaction' AND (jobs.state IN ('queued','running','retry') OR jobs.artifact_summary_id IS NOT NULL) AND NOT EXISTS (
	SELECT 1 FROM sessions active
	WHERE active.canonical_user_id = jobs.canonical_user_id
		AND active.session_id = jobs.session_id
		AND active.generation = jobs.session_generation
		AND active.is_active = 1
		AND active.expires_at > ?
) ORDER BY id LIMIT ?)
`, nowText, nowText, nowText, batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("retire inactive session compaction jobs: %w", err)
	}
	counts.CompactionJobsRetired, _ = result.RowsAffected()
	result, err = tx.ExecContext(ctx, `
DELETE FROM session_summaries WHERE id IN (SELECT id FROM session_summaries summaries
WHERE NOT EXISTS (
	SELECT 1 FROM sessions active
	WHERE active.canonical_user_id = summaries.canonical_user_id
		AND active.session_id = summaries.session_id
		AND active.generation = summaries.session_generation
		AND active.is_active = 1
		AND active.expires_at > ?
) ORDER BY id LIMIT ?)
`, nowText, batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("delete inactive session summaries: %w", err)
	}
	counts.SessionSummariesDeleted, _ = result.RowsAffected()

	turnRows, err := tx.QueryContext(ctx, `SELECT id, canonical_user_id FROM session_turns WHERE ((expires_at IS NOT NULL AND expires_at <= ?) AND NOT EXISTS (SELECT 1 FROM sessions active WHERE active.canonical_user_id = session_turns.canonical_user_id AND active.session_id = session_turns.session_id AND active.generation = session_turns.session_generation AND active.is_active = 1 AND active.expires_at > ?)) OR EXISTS (SELECT 1 FROM sessions WHERE sessions.canonical_user_id = session_turns.canonical_user_id AND sessions.session_id = session_turns.session_id AND sessions.generation = session_turns.session_generation AND (sessions.is_active = 0 OR sessions.expires_at <= ?)) ORDER BY id LIMIT ?`, nowText, nowText, nowText, batch)
	if err != nil {
		return counts, err
	}
	type turnOwner struct {
		id     int64
		userID string
	}
	var deletedTurns []turnOwner
	for turnRows.Next() {
		var turn turnOwner
		if err := turnRows.Scan(&turn.id, &turn.userID); err != nil {
			turnRows.Close()
			return counts, err
		}
		deletedTurns = append(deletedTurns, turn)
	}
	if err := turnRows.Close(); err != nil {
		return counts, err
	}
	for _, turn := range deletedTurns {
		result, err = tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = ? AND (source_turn_id = ? OR (extractor_version = ? AND EXISTS (SELECT 1 FROM json_each(durable_jobs.artifact_payload, '$.turn_ids') source WHERE source.type = 'integer' AND source.value = ?)))`, turn.userID, turn.id, PatternExtractorVersion, turn.id)
		if err != nil {
			return SessionCleanupCounts{}, fmt.Errorf("delete expired session formation jobs: %w", err)
		}
		changed, _ := result.RowsAffected()
		counts.FormationJobsDeleted += changed
		result, err = tx.ExecContext(ctx, `DELETE FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turn.id, turn.userID)
		if err != nil {
			return SessionCleanupCounts{}, fmt.Errorf("delete expired session turn: %w", err)
		}
		changed, _ = result.RowsAffected()
		counts.SessionTurnsDeleted += changed
	}
	for _, turn := range deletedTurns {
		if err := enqueueDerivedChangeTx(ctx, tx, turn.userID, "session_turn", turn.id, "delete", "cleanup:"+nowText); err != nil {
			return counts, err
		}
	}

	result, err = tx.ExecContext(ctx, `UPDATE sessions SET is_active = 0 WHERE rowid IN (SELECT rowid FROM sessions WHERE is_active = 1 AND expires_at <= ? ORDER BY expires_at, rowid LIMIT ?)`, nowText, batch)
	if err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("deactivate expired tenant sessions: %w", err)
	}
	if counts.SessionsDeactivated, err = result.RowsAffected(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("count deleted tenant sessions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return SessionCleanupCounts{}, fmt.Errorf("commit expired session cleanup: %w", err)
	}
	s.signalDerivedIndex()
	return counts, nil
}
