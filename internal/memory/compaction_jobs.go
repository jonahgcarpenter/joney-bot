package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ActiveSessionScopes returns active generations for startup job reconciliation.
func (s *Store) ActiveSessionScopes(ctx context.Context, limit int) ([]ActiveSessionScope, error) {
	query := `SELECT canonical_user_id, session_id, generation FROM sessions WHERE is_active = 1 AND julianday(expires_at) > julianday(?) ORDER BY last_seen_at DESC, canonical_user_id, session_id`
	args := []any{formatTime(time.Now().UTC())}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scopes []ActiveSessionScope
	for rows.Next() {
		var scope ActiveSessionScope
		if err := rows.Scan(&scope.UserID, &scope.SessionID, &scope.Generation); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// SessionCompactionCampaignTarget returns an unfinished target pinned by the
// current model and generator contract.
func (s *Store) SessionCompactionCampaignTarget(ctx context.Context, userID, sessionID string, generation int, boundary int64, model, generatorVersion string) (int64, error) {
	if err := validateSessionScope(userID, sessionID, generation); err != nil {
		return 0, err
	}
	var target int64
	err := s.sql.QueryRowContext(ctx, `SELECT compaction_target_turn_id FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND compaction_model = ? AND compaction_generator_version = ? AND compaction_target_turn_id > ? ORDER BY id DESC LIMIT 1`, userID, sessionID, generation, strings.TrimSpace(model), strings.TrimSpace(generatorVersion), boundary).Scan(&target)
	return target, err
}

// EnqueueSessionCompactionJob creates one idempotent chunk belonging
// to a stable campaign target.
func (s *Store) EnqueueSessionCompactionJob(ctx context.Context, userID, sessionID string, generation int, fromTurnID, throughTurnID, targetTurnID int64, model, generatorVersion string) (int64, error) {
	if err := validateSessionScope(userID, sessionID, generation); err != nil {
		return 0, err
	}
	if fromTurnID <= 0 || throughTurnID < fromTurnID || targetTurnID < throughTurnID {
		return 0, fmt.Errorf("enqueue session compaction: invalid range")
	}
	model = strings.TrimSpace(model)
	generatorVersion = strings.TrimSpace(generatorVersion)
	if model == "" || generatorVersion == "" {
		return 0, fmt.Errorf("enqueue session compaction: model and generator version are required")
	}
	var blockedID int64
	err := s.sql.QueryRowContext(ctx, `SELECT id FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND compaction_model = ? AND compaction_generator_version = ? AND artifact_summary_id IS NULL AND state IN ('queued','running','retry','skipped','dead') ORDER BY id LIMIT 1`, userID, sessionID, generation, model, generatorVersion).Scan(&blockedID)
	if err == nil {
		return blockedID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	now := time.Now().UTC()
	var priorFrom, priorThrough int64
	err = s.sql.QueryRowContext(ctx, `SELECT covered_from_turn_id, covered_through_turn_id FROM session_summaries WHERE canonical_user_id = ? AND session_id = ? AND session_generation = ? ORDER BY covered_through_turn_id DESC LIMIT 1`, userID, sessionID, generation).Scan(&priorFrom, &priorThrough)
	if err == nil && (fromTurnID != priorFrom || throughTurnID <= priorThrough) {
		return 0, fmt.Errorf("enqueue session compaction: range does not extend latest checkpoint")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	result, err := s.sql.ExecContext(ctx, `
INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id,
	covered_through_turn_id, compaction_target_turn_id, compaction_model, compaction_generator_version, available_at, updated_at
)
SELECT 'session_compaction', ? || ':' || ? || ':' || ? || ':' || ? || ':' || ? || ':' || ? || ':' || ? || ':' || ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE EXISTS (
		SELECT 1 FROM sessions
		WHERE canonical_user_id = ? AND session_id = ? AND generation = ?
			AND is_active = 1
			AND julianday(expires_at) > julianday(?)
	)
	AND EXISTS (
		SELECT 1 FROM session_turns WHERE id = ? AND canonical_user_id = ?
			AND session_id = ? AND session_generation = ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL
	)
	AND NOT EXISTS (
		SELECT 1 FROM session_turns WHERE canonical_user_id = ? AND session_id = ?
			AND session_generation = ? AND id BETWEEN ? AND ? AND delivered_at IS NULL AND delivery_failed_at IS NULL
	)
	AND EXISTS (
		SELECT 1 FROM session_turns WHERE id = ? AND canonical_user_id = ?
			AND session_id = ? AND session_generation = ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL
	)
	AND NOT EXISTS (
		SELECT 1 FROM durable_jobs active_job WHERE active_job.job_kind = 'session_compaction'
			AND active_job.canonical_user_id = ? AND active_job.session_id = ?
			AND active_job.session_generation = ? AND active_job.state IN ('queued','running','retry')
	)
ON CONFLICT DO NOTHING`,
		userID, sessionID, generation, fromTurnID, throughTurnID, targetTurnID, model, generatorVersion,
		userID, sessionID, generation, fromTurnID, throughTurnID, targetTurnID, model, generatorVersion, formatTime(now), formatTime(now),
		userID, sessionID, generation, formatTime(now),
		fromTurnID, userID, sessionID, generation,
		userID, sessionID, generation, fromTurnID, throughTurnID,
		throughTurnID, userID, sessionID, generation,
		userID, sessionID, generation)
	if err != nil {
		return 0, fmt.Errorf("enqueue session compaction: %w", err)
	}
	if count, countErr := result.RowsAffected(); countErr != nil {
		return 0, countErr
	} else if count == 0 {
		var id int64
		err = s.sql.QueryRowContext(ctx, `SELECT id FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND state IN ('queued','running','retry') ORDER BY id LIMIT 1`, userID, sessionID, generation).Scan(&id)
		if err == nil {
			return id, nil
		}
		err = s.sql.QueryRowContext(ctx, `SELECT id FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND covered_from_turn_id = ? AND covered_through_turn_id = ? AND compaction_model = ? AND compaction_generator_version = ?`, userID, sessionID, generation, fromTurnID, throughTurnID, model, generatorVersion).Scan(&id)
		if err == nil {
			return id, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("enqueue session compaction: active delivered range not found")
		}
		return 0, err
	}
	var id int64
	err = s.sql.QueryRowContext(ctx, `SELECT id FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND covered_from_turn_id = ? AND covered_through_turn_id = ? AND compaction_model = ? AND compaction_generator_version = ?`, userID, sessionID, generation, fromTurnID, throughTurnID, model, generatorVersion).Scan(&id)
	return id, err
}

// RecordUncompactableSessionExchange persists one terminal receipt
// without submitting a model request that cannot fit its complete oldest turn.
func (s *Store) RecordUncompactableSessionExchange(ctx context.Context, userID, sessionID string, generation int, fromTurnID, throughTurnID, targetTurnID int64, model, generatorVersion string) (int64, error) {
	id, err := s.EnqueueSessionCompactionJob(ctx, userID, sessionID, generation, fromTurnID, throughTurnID, targetTurnID, model, generatorVersion)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'skipped', completed_at = ?, last_error_code = 'uncompactable_complete_exchange', updated_at = ? WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND state = 'queued' AND artifact_payload = ''`, formatTime(now), formatTime(now), id, userID)
	if err != nil {
		return 0, fmt.Errorf("record uncompactable session compaction campaign: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var state string
		if err := s.sql.QueryRowContext(ctx, `SELECT state FROM durable_jobs WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ?`, id, userID).Scan(&state); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// ReconcileSessionCompactionJobs skips stale generations and releases expired leases.
func (s *Store) ReconcileSessionCompactionJobs(ctx context.Context, model, generatorVersion string) (int64, error) {
	now := time.Now().UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // nolint:errcheck
	contract, err := tx.ExecContext(ctx, `
UPDATE durable_jobs
SET state = 'skipped', completed_at = COALESCE(completed_at, ?), lease_owner = '', lease_until = NULL,
	last_error_code = 'superseded_compaction_contract', updated_at = ?
WHERE job_kind = 'session_compaction' AND state IN ('queued','running','retry','dead')
	AND (compaction_model != ? OR compaction_generator_version != ?)`, formatTime(now), formatTime(now), strings.TrimSpace(model), strings.TrimSpace(generatorVersion))
	if err != nil {
		return 0, fmt.Errorf("skip superseded session compaction jobs: %w", err)
	}
	stale, err := tx.ExecContext(ctx, `
UPDATE durable_jobs
SET state = 'skipped', completed_at = ?, lease_owner = '', lease_until = NULL,
	last_error_code = 'stale_generation', updated_at = ?
	WHERE job_kind = 'session_compaction' AND state IN ('queued', 'running', 'retry') AND (NOT EXISTS (
	SELECT 1 FROM sessions active
	WHERE active.canonical_user_id = durable_jobs.canonical_user_id
		AND active.session_id = durable_jobs.session_id
		AND active.generation = durable_jobs.session_generation
		AND active.is_active = 1
		AND julianday(active.expires_at) > julianday(?)
	))`, formatTime(now), formatTime(now), formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("skip stale session compaction jobs: %w", err)
	}
	expired, err := tx.ExecContext(ctx, `
UPDATE durable_jobs
SET state = CASE WHEN model_submission_count >= ? AND artifact_payload = '' THEN 'dead' ELSE 'retry' END,
	available_at = ?, lease_owner = '', lease_until = NULL,
	completed_at = CASE WHEN model_submission_count >= ? AND artifact_payload = '' THEN ? ELSE NULL END,
	last_error_code = 'transient_lease_expired', updated_at = ?
WHERE job_kind = 'session_compaction' AND state = 'running' AND lease_until IS NOT NULL AND lease_until <= ?`,
		SessionCompactionModelSubmissionLimit, formatTime(now), SessionCompactionModelSubmissionLimit, formatTime(now), formatTime(now), formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("release expired session compaction leases: %w", err)
	}
	staleCount, _ := stale.RowsAffected()
	expiredCount, _ := expired.RowsAffected()
	contractCount, _ := contract.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return staleCount + expiredCount + contractCount, nil
}

// ClaimSessionCompactionJob leases the oldest ready job to one worker.
func (s *Store) ClaimSessionCompactionJob(ctx context.Context, owner string, lease time.Duration, model, generatorVersion string) (SessionCompactionJob, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return SessionCompactionJob{}, fmt.Errorf("claim session compaction job: lease owner is required")
	}
	if lease <= 0 {
		lease = time.Minute
	}
	now := time.Now().UTC()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return SessionCompactionJob{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	var id int64
	err = tx.QueryRowContext(ctx, `
SELECT id FROM durable_jobs jobs
WHERE job_kind = 'session_compaction' AND ((state IN ('queued', 'retry') AND available_at <= ?)
	OR (state = 'running' AND lease_until IS NOT NULL AND lease_until <= ?))
	AND compaction_model = ? AND compaction_generator_version = ?
	AND EXISTS (
		SELECT 1 FROM sessions active
		WHERE active.canonical_user_id = jobs.canonical_user_id
			AND active.session_id = jobs.session_id
			AND active.generation = jobs.session_generation
			AND active.is_active = 1
			AND julianday(active.expires_at) > julianday(?)
	)
ORDER BY available_at, id LIMIT 1`, formatTime(now), formatTime(now), strings.TrimSpace(model), strings.TrimSpace(generatorVersion), formatTime(now)).Scan(&id)
	if err != nil {
		return SessionCompactionJob{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE durable_jobs
SET state = 'running', attempt_count = MIN(attempt_count + 1, ?), lease_owner = ?, lease_until = ?,
	updated_at = ?
WHERE id = ? AND job_kind = 'session_compaction'`, SessionCompactionModelSubmissionLimit, owner, formatTime(now.Add(lease)), formatTime(now), id)
	if err != nil {
		return SessionCompactionJob{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return SessionCompactionJob{}, sql.ErrNoRows
	}
	job, err := loadSessionCompactionJobTx(ctx, tx, id)
	if err != nil {
		return SessionCompactionJob{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT source_request_id FROM session_turns WHERE id = ? AND canonical_user_id = ? AND session_id = ? AND session_generation = ?), '')`, job.CoveredThroughTurnID, job.UserID, job.SessionID, job.SessionGeneration).Scan(&job.RequestID); err != nil {
		return SessionCompactionJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionCompactionJob{}, err
	}
	return job, nil
}

// RenewSessionCompactionJobLease extends an exactly owned, still-live compaction lease.
func (s *Store) RenewSessionCompactionJobLease(ctx context.Context, job SessionCompactionJob, lease time.Duration) (time.Time, error) {
	if lease <= 0 {
		return time.Time{}, fmt.Errorf("renew session compaction lease: duration must be positive")
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET lease_until = ?, updated_at = ? WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?)`, formatTime(leaseUntil), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now))
	if err != nil {
		return time.Time{}, fmt.Errorf("renew session compaction lease: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return time.Time{}, sql.ErrNoRows
	}
	return leaseUntil, nil
}

// ReserveSessionCompactionModelSubmission transactionally consumes one
// provider submission immediately before invocation under the exact live lease.
func (s *Store) ReserveSessionCompactionModelSubmission(ctx context.Context, job SessionCompactionJob) (int, error) {
	now := time.Now().UTC()
	var count int
	err := s.sql.QueryRowContext(ctx, `UPDATE durable_jobs
SET model_submission_count = model_submission_count + 1, updated_at = ?
WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ?
	AND session_id = ? AND session_generation = ? AND state = 'running'
	AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?)
	AND model_submission_count < ?
RETURNING model_submission_count`, formatTime(now), job.ID, job.UserID, job.SessionID,
		job.SessionGeneration, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now), SessionCompactionModelSubmissionLimit).Scan(&count)
	if err == nil {
		return count, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("reserve session compaction model submission: %w", err)
	}
	var storedCount int
	if readErr := s.sql.QueryRowContext(ctx, `SELECT model_submission_count FROM durable_jobs
WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ?`, job.ID, job.UserID).Scan(&storedCount); readErr == nil && storedCount >= SessionCompactionModelSubmissionLimit {
		return storedCount, ErrModelSubmissionBudgetExhausted
	}
	return 0, ErrStaleSessionCompactionJobLease
}

// RefundSessionCompactionModelSubmission restores the current reservation
// owned by the exact live lease when foreground work intentionally preempts it.
func (s *Store) RefundSessionCompactionModelSubmission(ctx context.Context, job SessionCompactionJob) error {
	if job.ModelSubmissionCount <= 0 {
		return fmt.Errorf("refund session compaction model submission: invalid reservation")
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs
SET model_submission_count = model_submission_count - 1, updated_at = ?
WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ?
	AND session_id = ? AND session_generation = ? AND state = 'running'
	AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?)
	AND model_submission_count = ?`, formatTime(now), job.ID, job.UserID, job.SessionID,
		job.SessionGeneration, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now), job.ModelSubmissionCount)
	if err != nil {
		return fmt.Errorf("refund session compaction model submission: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStaleSessionCompactionJobLease
	}
	return nil
}

// SaveSessionCompactionArtifact persists canonical JSON for the first result only.
func (s *Store) SaveSessionCompactionArtifact(ctx context.Context, job SessionCompactionJob, artifact SummaryArtifact) error {
	if artifact.GenerationModel != job.Model || artifact.GeneratorVersion != job.GeneratorVersion {
		return fmt.Errorf("save session compaction artifact: contract mismatch")
	}
	payload, artifact, err := encodeSummaryArtifact(artifact)
	if err != nil {
		return err
	}
	result, err := s.sql.ExecContext(ctx, `
UPDATE durable_jobs
SET artifact_payload = CASE WHEN artifact_payload = '' THEN ? ELSE artifact_payload END,
	updated_at = ?
WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ?
	AND covered_from_turn_id = ? AND covered_through_turn_id = ?
	AND state = 'running' AND lease_owner = ? AND julianday(lease_until) > julianday(?)`,
		payload, formatTime(time.Now().UTC()),
		job.ID, job.UserID, job.SessionID, job.SessionGeneration, job.CoveredFromTurnID, job.CoveredThroughTurnID, job.LeaseOwner, formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("save session compaction artifact: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	var stored string
	if err := s.sql.QueryRowContext(ctx, `SELECT artifact_payload FROM durable_jobs WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ?`, job.ID, job.UserID).Scan(&stored); err != nil {
		return err
	}
	if stored != payload {
		return fmt.Errorf("session compaction artifact is immutable")
	}
	return nil
}

// SessionCompactionArtifact loads and strictly decodes a tenant-owned artifact.
func (s *Store) SessionCompactionArtifact(ctx context.Context, job SessionCompactionJob) (SummaryArtifact, error) {
	var payload string
	err := s.sql.QueryRowContext(ctx, `SELECT artifact_payload FROM durable_jobs WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND covered_from_turn_id = ? AND covered_through_turn_id = ?`, job.ID, job.UserID, job.SessionID, job.SessionGeneration, job.CoveredFromTurnID, job.CoveredThroughTurnID).Scan(&payload)
	if err != nil {
		return SummaryArtifact{}, err
	}
	if payload == "" {
		return SummaryArtifact{}, sql.ErrNoRows
	}
	return decodeSummaryArtifact(payload)
}

// CompleteSessionCompactionJob records successful publication or an intentional skip.
func (s *Store) CompleteSessionCompactionJob(ctx context.Context, job SessionCompactionJob, skipped bool) error {
	state := "succeeded"
	artifactCondition := "AND artifact_summary_id IS NOT NULL"
	if skipped {
		state = "skipped"
		artifactCondition = ""
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = ?, completed_at = ?, lease_owner = '', lease_until = NULL, last_error_code = '', corrective_error_code = '', updated_at = ? WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND julianday(lease_until) > julianday(?) `+artifactCondition, state, formatTime(now), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(now))
	if err != nil {
		return fmt.Errorf("complete session compaction job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// RetrySessionCompactionJob releases an exactly owned failed lease with bounded
// backoff. The returned retry/dead state comes from the persisted submission and
// artifact state, not the caller's attempt count. The lease must still be live.
func (s *Store) RetrySessionCompactionJob(ctx context.Context, job SessionCompactionJob, code string) (string, error) {
	now := time.Now().UTC()
	delay := time.Duration(1<<min(job.AttemptCount, 6)) * time.Second
	var state string
	err := s.sql.QueryRowContext(ctx, `
UPDATE durable_jobs
SET state = CASE WHEN model_submission_count >= ? AND artifact_payload = '' THEN 'dead' ELSE 'retry' END,
	available_at = ?, lease_owner = '', lease_until = NULL,
	completed_at = CASE WHEN model_submission_count >= ? AND artifact_payload = '' THEN ? ELSE NULL END,
	last_error_code = ?, updated_at = ?
WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) RETURNING state`,
		SessionCompactionModelSubmissionLimit, formatTime(now.Add(delay)), SessionCompactionModelSubmissionLimit, formatTime(now), safeErrorCode(code),
		formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now)).Scan(&state)
	if err != nil {
		return "", fmt.Errorf("retry session compaction job: %w", err)
	}
	return state, nil
}

// RetryInvalidSessionCompactionJob records a bounded reason-aware structured
// retry. Its submission has already consumed the shared durable budget.
func (s *Store) RetryInvalidSessionCompactionJob(ctx context.Context, job SessionCompactionJob, code string) error {
	now := time.Now().UTC()
	delay := time.Duration(1<<min(job.AttemptCount, 6)) * time.Second
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'retry', compaction_invalid_output_retry_count = compaction_invalid_output_retry_count + 1, available_at = ?, completed_at = NULL, lease_owner = '', lease_until = NULL, last_error_code = ?, corrective_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) AND compaction_invalid_output_retry_count < ? AND model_submission_count < ? AND artifact_payload = ''`, formatTime(now.Add(delay)), safeErrorCode(code), safeErrorCode(code), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now), SessionCompactionInvalidOutputRetryLimit, SessionCompactionModelSubmissionLimit)
	if err != nil {
		return fmt.Errorf("retry invalid session compaction job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// SkipSessionCompactionJob terminally skips a job that cannot succeed unchanged.
func (s *Store) SkipSessionCompactionJob(ctx context.Context, job SessionCompactionJob, code string) error {
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'skipped', completed_at = ?, lease_owner = '', lease_until = NULL, last_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?)`, formatTime(now), safeErrorCode(code), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now))
	if err != nil {
		return fmt.Errorf("skip session compaction job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// DeferSessionCompactionJob releases a lease preempted by foreground work
// without consuming the provider retry budget.
func (s *Store) DeferSessionCompactionJob(ctx context.Context, job SessionCompactionJob, delay time.Duration) error {
	if delay <= 0 {
		delay = time.Second
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `
UPDATE durable_jobs
SET state = 'retry', attempt_count = MAX(attempt_count - 1, 0), available_at = ?,
	lease_owner = '', lease_until = NULL, last_error_code = 'foreground_preempted', updated_at = ?
WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ?
	AND state = 'running' AND lease_owner = ? AND lease_until = ?`,
		formatTime(now.Add(delay)), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil))
	if err != nil {
		return fmt.Errorf("defer session compaction job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

const sessionCompactionJobSelect = `SELECT id, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id, compaction_target_turn_id, state, COALESCE(artifact_summary_id, 0), compaction_model, compaction_generator_version, attempt_count, compaction_invalid_output_retry_count, last_error_code, model_submission_count, corrective_error_code, available_at, lease_owner, lease_until FROM durable_jobs `

func validateSessionScope(userID, sessionID string, generation int) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || generation <= 0 {
		return fmt.Errorf("session compaction requires tenant, session, and generation")
	}
	return nil
}

func (s *Store) requireActiveSessionGeneration(ctx context.Context, userID, sessionID string, generation int) error {
	var active int
	err := s.sql.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE canonical_user_id = ? AND session_id = ? AND generation = ? AND is_active = 1 AND julianday(expires_at) > julianday(?)`, userID, sessionID, generation, formatTime(time.Now().UTC())).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session compaction: stale session generation")
	}
	if err != nil {
		return fmt.Errorf("session compaction: validate active generation: %w", err)
	}
	return nil
}

func scanSessionCompactionJob(row interface{ Scan(...any) error }) (SessionCompactionJob, error) {
	var job SessionCompactionJob
	var availableAt string
	var leaseUntil sql.NullString
	err := row.Scan(&job.ID, &job.UserID, &job.SessionID, &job.SessionGeneration,
		&job.CoveredFromTurnID, &job.CoveredThroughTurnID, &job.TargetTurnID, &job.State,
		&job.ArtifactSummaryID, &job.Model, &job.GeneratorVersion, &job.AttemptCount, &job.InvalidOutputRetryCount, &job.LastErrorCode, &job.ModelSubmissionCount, &job.CorrectiveErrorCode, &availableAt, &job.LeaseOwner, &leaseUntil)
	if err != nil {
		return SessionCompactionJob{}, err
	}
	job.AvailableAt = parseTime(availableAt)
	if leaseUntil.Valid {
		job.LeaseUntil = parseTime(leaseUntil.String)
	}
	return job, nil
}

func loadSessionCompactionJobTx(ctx context.Context, tx *sql.Tx, id int64) (SessionCompactionJob, error) {
	return scanSessionCompactionJob(tx.QueryRowContext(ctx, sessionCompactionJobSelect+` WHERE id = ? AND job_kind = 'session_compaction'`, id))
}

func loadSessionCompactionJobWithArtifactTx(ctx context.Context, tx *sql.Tx, expected SessionCompactionJob) (SessionCompactionJob, string, error) {
	var job SessionCompactionJob
	var payload, availableAt string
	var leaseUntil sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id, compaction_target_turn_id, state, COALESCE(artifact_summary_id, 0), compaction_model, compaction_generator_version, attempt_count, compaction_invalid_output_retry_count, last_error_code, model_submission_count, corrective_error_code, available_at, lease_owner, lease_until, artifact_payload FROM durable_jobs WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ?`, expected.ID, expected.UserID).Scan(
		&job.ID, &job.UserID, &job.SessionID, &job.SessionGeneration, &job.CoveredFromTurnID,
		&job.CoveredThroughTurnID, &job.TargetTurnID, &job.State, &job.ArtifactSummaryID,
		&job.Model, &job.GeneratorVersion, &job.AttemptCount, &job.InvalidOutputRetryCount, &job.LastErrorCode, &job.ModelSubmissionCount, &job.CorrectiveErrorCode, &availableAt,
		&job.LeaseOwner, &leaseUntil, &payload)
	if err != nil {
		return SessionCompactionJob{}, "", err
	}
	if job.SessionID != expected.SessionID || job.SessionGeneration != expected.SessionGeneration || job.CoveredFromTurnID != expected.CoveredFromTurnID || job.CoveredThroughTurnID != expected.CoveredThroughTurnID || job.TargetTurnID != expected.TargetTurnID || job.Model != expected.Model || job.GeneratorVersion != expected.GeneratorVersion {
		return SessionCompactionJob{}, "", fmt.Errorf("session compaction job scope mismatch")
	}
	job.AvailableAt = parseTime(availableAt)
	if leaseUntil.Valid {
		job.LeaseUntil = parseTime(leaseUntil.String)
	}
	if payload == "" && job.ArtifactSummaryID == 0 {
		return SessionCompactionJob{}, "", fmt.Errorf("publish session summary: artifact is missing")
	}
	return job, payload, nil
}
