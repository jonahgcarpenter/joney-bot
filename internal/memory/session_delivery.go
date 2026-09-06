package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MarkSessionTurnDelivered records successful response delivery exactly once.
func (s *Store) MarkSessionTurnDelivered(ctx context.Context, userID string, turnID int64) error {
	if strings.TrimSpace(userID) == "" || turnID <= 0 {
		return fmt.Errorf("mark session turn delivered: tenant and turn are required")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delivered session turn update: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	now := time.Now().UTC()
	lateDelivery, err := sessionTurnHadDeliveryFailureTx(ctx, tx, strings.TrimSpace(userID), turnID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE session_turns
SET delivered_at = COALESCE(delivered_at, ?), delivery_failed_at = NULL
WHERE id = ? AND canonical_user_id = ?
	AND EXISTS (
		SELECT 1 FROM sessions active
		WHERE active.canonical_user_id = session_turns.canonical_user_id
			AND active.session_id = session_turns.session_id
			AND active.generation = session_turns.session_generation
			AND active.is_active = 1
			AND julianday(active.expires_at) > julianday(?)
	)`, formatTime(now), turnID, strings.TrimSpace(userID), formatTime(now))
	if err != nil {
		return fmt.Errorf("mark session turn delivered: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count delivered session turn: %w", err)
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	if lateDelivery {
		if err := invalidateCompactionAfterLateDeliveryTx(ctx, tx, strings.TrimSpace(userID), turnID); err != nil {
			return err
		}
	}
	if err := enqueueDerivedChangeTx(ctx, tx, strings.TrimSpace(userID), "session_turn", turnID, "upsert", "delivered:"+formatTime(now)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delivered session turn update: %w", err)
	}
	s.signalDerivedIndex()
	return nil
}

func sessionTurnHadDeliveryFailureTx(ctx context.Context, tx *sql.Tx, userID string, turnID int64) (bool, error) {
	var failedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT delivery_failed_at FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turnID, userID).Scan(&failedAt); err != nil {
		return false, err
	}
	return failedAt.Valid, nil
}

func invalidateCompactionAfterLateDeliveryTx(ctx context.Context, tx *sql.Tx, userID string, turnID int64) error {
	var sessionID string
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT session_id, session_generation FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turnID, userID).Scan(&sessionID, &generation); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND covered_through_turn_id >= ?`, userID, sessionID, generation, turnID); err != nil {
		return fmt.Errorf("invalidate compaction jobs after late delivery: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_summaries WHERE canonical_user_id = ? AND session_id = ? AND session_generation = ? AND covered_through_turn_id >= ?`, userID, sessionID, generation, turnID); err != nil {
		return fmt.Errorf("invalidate session summaries after late delivery: %w", err)
	}
	return nil
}

// MarkSessionTurnDeliveryFailed records a terminal failed send without making
// the persisted response eligible for context, search, or compaction.
func (s *Store) MarkSessionTurnDeliveryFailed(ctx context.Context, userID string, turnID int64) error {
	if strings.TrimSpace(userID) == "" || turnID <= 0 {
		return fmt.Errorf("mark session turn delivery failed: tenant and turn are required")
	}
	result, err := s.sql.ExecContext(ctx, `UPDATE session_turns SET delivery_failed_at = COALESCE(delivery_failed_at, ?) WHERE id = ? AND canonical_user_id = ? AND delivered_at IS NULL`, formatTime(time.Now().UTC()), turnID, strings.TrimSpace(userID))
	if err != nil {
		return fmt.Errorf("mark session turn delivery failed: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
