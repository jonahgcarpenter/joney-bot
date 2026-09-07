package memory

import (
	"context"
	"database/sql"
	"time"
)

// MemorySuppression blocks one exact canonical claim, not semantically similar text.
type MemorySuppression struct {
	ID         int64
	Statement  string
	ClaimSlot  string
	ClaimValue string
}

func suppressedClaimTx(ctx context.Context, tx *sql.Tx, userID, slot, value string, turnID int64) (bool, error) {
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_suppressions WHERE canonical_user_id = ? AND claim_slot = ? AND replace(claim_value,'_',' ')=replace(?,'_',' ') AND (is_active = 1 OR through_turn_id >= ?))`, userID, slot, value, turnID).Scan(&blocked)
	return blocked, err
}

// Only separators are compatible across versions: punctuation such as C++ and C# remains significant.
func publicationBlockedTx(ctx context.Context, tx *sql.Tx, userID, scope, slot, value string, turnID int64) (bool, error) {
	blocked, err := suppressedClaimTx(ctx, tx, userID, slot, value, turnID)
	if err != nil || blocked {
		return blocked, err
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_entries WHERE canonical_user_id=? AND scope=? AND claim_slot=? AND (claim_slot NOT LIKE '%.fact' OR replace(claim_value,'_',' ')=replace(?,'_',' ')) AND assessed_source_turn_id>?)`, userID, scope, slot, value, turnID).Scan(&blocked)
	return blocked, err
}

// ListMemorySuppressions returns only currently enabled rules for the owner.
func (s *Store) ListMemorySuppressions(ctx context.Context, userID string) ([]MemorySuppression, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, statement, claim_slot, claim_value FROM memory_suppressions WHERE canonical_user_id = ? AND is_active = 1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []MemorySuppression{}
	for rows.Next() {
		var item MemorySuppression
		if err := rows.Scan(&item.ID, &item.Statement, &item.ClaimSlot, &item.ClaimValue); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// SuppressMemory physically deletes matching memories and blocks their exact claim atomically.
func (s *Store) SuppressMemory(ctx context.Context, userID string, id int64, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := requireActiveUser(ctx, tx, userID); err != nil {
		return 0, err
	}
	var ruleID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO memory_suppressions(canonical_user_id, statement, claim_slot, claim_value, through_turn_id, created_at)
 SELECT canonical_user_id, statement, claim_slot, claim_value, COALESCE((SELECT MAX(id) FROM session_turns),0), ? FROM memory_entries WHERE canonical_user_id = ? AND id = ?
 ON CONFLICT(canonical_user_id,claim_slot,claim_value) DO UPDATE SET is_active = 1, statement = excluded.statement, through_turn_id = MAX(through_turn_id,excluded.through_turn_id),retained_source_turn_id=0
 RETURNING id`, formatTime(now), userID, id).Scan(&ruleID)
	if err != nil {
		return 0, err
	}
	ids, err := memoryIDsTx(tx, `SELECT id FROM memory_entries m WHERE canonical_user_id=? AND EXISTS(SELECT 1 FROM memory_suppressions r WHERE id=? AND r.claim_slot=m.claim_slot AND replace(r.claim_value,'_',' ')=replace(m.claim_value,'_',' '))`, userID, ruleID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_observations WHERE canonical_user_id = ? AND EXISTS(SELECT 1 FROM memory_suppressions r WHERE r.id=? AND r.claim_slot=memory_observations.claim_slot AND replace(r.claim_value,'_',' ')=replace(memory_observations.claim_value,'_',' '))`, userID, ruleID); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := hardDeleteMemoryTx(ctx, tx, userID, id, now, true); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.signalDerivedIndex()
	return ruleID, nil
}

// UnsuppressMemory permits new evidence, retaining a cutoff against old replay.
func (s *Store) UnsuppressMemory(ctx context.Context, userID string, ruleID int64) (bool, error) {
	result, err := s.sql.ExecContext(ctx, `UPDATE memory_suppressions SET is_active = 0, retained_source_turn_id=0, through_turn_id = MAX(through_turn_id, COALESCE((SELECT MAX(id) FROM session_turns),0)) WHERE canonical_user_id = ? AND id = ? AND is_active = 1`, userID, ruleID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
