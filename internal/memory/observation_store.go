package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MemoryObservation is admitted evidence whose retention is independent of its source session.
type MemoryObservation struct {
	ID           int64
	Statement    string
	Evidence     string
	Context      string
	Provenance   string
	Intent       string
	ObservedAt   time.Time
	ExpiresAt    time.Time
	SourceTurnID int64
	ClaimSlot    string
	ClaimValue   string
}

// ListMemoryObservations lists live observations without extending their expiry.
func (s *Store) ListMemoryObservations(ctx context.Context, userID string, now time.Time) ([]MemoryObservation, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return listMemoryObservationsTx(ctx, tx, userID, now)
}

func listMemoryObservationsTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) ([]MemoryObservation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, statement, evidence, context, provenance_type, observed_at, expires_at, source_turn_id, claim_slot, claim_value, intent FROM memory_observations WHERE canonical_user_id = ? AND julianday(expires_at) > julianday(?) ORDER BY id`, userID, formatTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []MemoryObservation{}
	for rows.Next() {
		var o MemoryObservation
		var observed, expires string
		if err := rows.Scan(&o.ID, &o.Statement, &o.Evidence, &o.Context, &o.Provenance, &observed, &expires, &o.SourceTurnID, &o.ClaimSlot, &o.ClaimValue, &o.Intent); err != nil {
			return nil, err
		}
		o.ObservedAt, o.ExpiresAt = parseTime(observed), parseTime(expires)
		result = append(result, o)
	}
	return result, rows.Err()
}

func insertObservationTx(ctx context.Context, tx *sql.Tx, userID string, turnID int64, c ForegroundMemoryCandidate, now time.Time) (bool, error) {
	days := c.TTLDays
	if days == 0 {
		days = 7
	}
	if days < 1 || days > 30 {
		return false, fmt.Errorf("invalid observation lifetime")
	}
	var sourceTime string
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM session_turns WHERE id=? AND canonical_user_id=? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL`, turnID, userID).Scan(&sourceTime); err != nil {
		return false, err
	}
	observed := parseTime(sourceTime)
	if observed.IsZero() {
		return false, fmt.Errorf("invalid observation source timestamp")
	}
	expires := observed.Add(time.Duration(days) * 24 * time.Hour)
	if !expires.After(now) {
		return false, nil
	}
	digest := formationKey(c.ClaimSlot, c.ClaimValue, c.Evidence)
	var admitted bool
	var turnCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(digest=?),0) FROM memory_observation_receipts WHERE canonical_user_id=? AND source_turn_id=?`, digest, userID, turnID).Scan(&turnCount, &admitted); err != nil {
		return false, err
	}
	if admitted || turnCount >= 5 {
		return false, nil
	}
	intent := c.Intent
	if intent == "" {
		intent = "automatic"
	}
	if intent != "automatic" && intent != "remember" {
		return false, fmt.Errorf("invalid observation intent")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_observations WHERE canonical_user_id = ? AND julianday(expires_at) <= julianday(?)`, userID, formatTime(now)); err != nil {
		return false, err
	}
	var count, bytes int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(statement||evidence||context||provenance_type||claim_slot||claim_value AS BLOB))),0) FROM memory_observations WHERE canonical_user_id = ?`, userID).Scan(&count, &bytes); err != nil {
		return false, err
	}
	newBytes := len(c.Statement) + len(c.Evidence) + len(c.Context) + len(c.Provenance) + len(c.ClaimSlot) + len(c.ClaimValue)
	if newBytes > 128*1024 {
		return false, fmt.Errorf("observation exceeds tenant byte budget")
	}
	if count >= 100 || bytes+newBytes > 128*1024 {
		var removableCount, removableBytes int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(statement||evidence||context||provenance_type||claim_slot||claim_value AS BLOB))),0) FROM memory_observations WHERE canonical_user_id=? AND (intent='automatic' OR ?='remember')`, userID, intent).Scan(&removableCount, &removableBytes); err != nil {
			return false, err
		}
		if (count >= 100 && removableCount == 0) || bytes-removableBytes+newBytes > 128*1024 {
			return false, nil
		}
	}
	for count >= 100 || bytes+newBytes > 128*1024 {
		var id int64
		var size int
		err := tx.QueryRowContext(ctx, `SELECT id,length(CAST(statement||evidence||context||provenance_type||claim_slot||claim_value AS BLOB)) FROM memory_observations WHERE canonical_user_id=? AND (intent='automatic' OR ?='remember') ORDER BY CASE intent WHEN 'automatic' THEN 0 ELSE 1 END, julianday(observed_at),id LIMIT 1`, userID, intent).Scan(&id, &size)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_observations WHERE id=? AND canonical_user_id=?`, id, userID); err != nil {
			return false, err
		}
		count--
		bytes -= size
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_observation_receipts(canonical_user_id,source_turn_id,digest) VALUES(?,?,?)`, userID, turnID, digest); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO memory_observations(canonical_user_id,source_turn_id,statement,evidence,context,provenance_type,claim_slot,claim_value,observed_at,expires_at,intent) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, userID, turnID, c.Statement, c.Evidence, c.Context, c.Provenance, c.ClaimSlot, c.ClaimValue, formatTime(observed), formatTime(expires), intent)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
