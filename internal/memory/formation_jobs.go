package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EnqueueFormationJob records one replay-safe extraction job per source turn/version.
func (s *Store) EnqueueFormationJob(ctx context.Context, source FormationSource, userID string) (int64, error) {
	id, _, err := s.enqueueFormationJob(ctx, source, userID, FormationPurposeBackgroundPattern)
	return id, err
}

// EnqueuePatternFormationJob freezes the newest eligible window ending at the
// anchor turn. A one-turn session intentionally produces no background job.
func (s *Store) EnqueuePatternFormationJob(ctx context.Context, source FormationSource, userID string) (int64, bool, error) {
	return s.enqueueWindowFormationJob(ctx, source, userID, PatternExtractorVersion)
}

// EnqueueAssessmentFormationJob freezes source membership independently of later observations.
func (s *Store) EnqueueAssessmentFormationJob(ctx context.Context, source FormationSource, userID string) (int64, bool, error) {
	return s.enqueueWindowFormationJob(ctx, source, userID, AssessmentExtractorVersion)
}

func (s *Store) enqueueWindowFormationJob(ctx context.Context, source FormationSource, userID, version string) (int64, bool, error) {
	if source.TurnID <= 0 || strings.TrimSpace(userID) == "" {
		return 0, false, fmt.Errorf("pattern job requires tenant and anchor turn")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() // nolint:errcheck
	var anchor StoredSessionTurn
	var storedRequestID string
	if err := tx.QueryRowContext(ctx, `SELECT id, canonical_user_id, session_id, session_generation, user_text, source_request_id FROM session_turns WHERE id = ? AND canonical_user_id = ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL`, source.TurnID, userID).Scan(&anchor.ID, &anchor.UserID, &anchor.SessionID, &anchor.Generation, &anchor.UserText, &storedRequestID); err != nil {
		return 0, false, fmt.Errorf("resolve delivered pattern anchor: %w", err)
	}
	if source.RequestID == "" {
		source.RequestID = storedRequestID
	}
	if source.SessionID == "" {
		source.SessionID = anchor.SessionID
	}
	if source.SessionGeneration <= 0 {
		source.SessionGeneration = anchor.Generation
	}
	if source.RequestID != storedRequestID || source.SessionID != anchor.SessionID || source.SessionGeneration != anchor.Generation {
		return 0, false, fmt.Errorf("pattern anchor scope does not match persisted turn")
	}
	if version == AssessmentExtractorVersion {
		var applied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_assessment_receipts WHERE canonical_user_id=? AND source_turn_id=? AND purpose='background_pattern')`, userID, anchor.ID).Scan(&applied); err != nil {
			return 0, false, err
		}
		if applied {
			return 0, false, nil
		}
		var eligible bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_turns t JOIN sessions s ON s.canonical_user_id=t.canonical_user_id AND s.session_id=t.session_id AND s.generation=t.session_generation WHERE t.id=? AND t.canonical_user_id=? AND s.is_active=1 AND julianday(s.expires_at)>julianday('now') AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday('now')))`, anchor.ID, userID).Scan(&eligible); err != nil {
			return 0, false, err
		}
		if !eligible {
			return 0, false, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM session_turns WHERE canonical_user_id = ? AND session_id = ? AND session_generation = ? AND id <= ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL AND (? != 'assessment-v1' OR expires_at IS NULL OR julianday(expires_at)>julianday('now')) ORDER BY id DESC LIMIT ?`, userID, anchor.SessionID, anchor.Generation, source.TurnID, version, MaxPatternContextTurns)
	if err != nil {
		return 0, false, err
	}
	var reversed []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, false, err
		}
		reversed = append(reversed, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, false, err
	}
	if err := rows.Close(); err != nil {
		return 0, false, err
	}
	if len(reversed) < 2 && version == PatternExtractorVersion {
		return 0, false, nil
	}
	turnIDs := make([]int64, len(reversed))
	for i := range reversed {
		turnIDs[len(reversed)-1-i] = reversed[i]
	}
	payload, err := MarshalPatternContext(turnIDs)
	if version == AssessmentExtractorVersion {
		payload, err = json.Marshal(struct {
			Version int     `json:"version"`
			TurnIDs []int64 `json:"turn_ids"`
		}{1, turnIDs})
	}
	if err != nil {
		return 0, false, err
	}
	source.ExtractorVersion = version
	key := fmt.Sprintf("turn:%d:%s", source.TurnID, version)
	now := formatTime(time.Now().UTC())
	result, err := tx.ExecContext(ctx, `
INSERT INTO durable_jobs (job_kind, canonical_user_id, idempotency_key, state, source_request_id,
	source_session_id, source_session_generation, source_turn_id, extraction_model,
	extractor_version, formation_purpose, artifact_payload, available_at, updated_at)
VALUES ('memory_formation', ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING`, userID, key, source.RequestID, anchor.SessionID, anchor.Generation, source.TurnID, source.Model, version, FormationPurposeBackgroundPattern, string(payload), now, now)
	if err != nil {
		return 0, false, fmt.Errorf("enqueue pattern formation job: %w", err)
	}
	created, _ := result.RowsAffected()
	var id int64
	var storedPayload string
	if err := tx.QueryRowContext(ctx, `SELECT id, artifact_payload FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = ? AND idempotency_key = ?`, userID, key).Scan(&id, &storedPayload); err != nil {
		return 0, false, err
	}
	if storedPayload != string(payload) {
		return 0, false, fmt.Errorf("existing pattern job has a different frozen context")
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, created == 1, nil
}

// EnqueueAgentSaveFormationJob records a replay-safe foreground-save job when
// the immutable source turn contains candidates.
func (s *Store) EnqueueAgentSaveFormationJob(ctx context.Context, source FormationSource, userID string) (int64, bool, error) {
	artifact, err := s.SessionTurnForegroundMemory(ctx, userID, source.TurnID)
	if err != nil {
		return 0, false, err
	}
	if len(artifact.Candidates) == 0 {
		return 0, false, nil
	}
	source.ExtractorVersion = AgentSaveExtractorVersion
	return s.enqueueFormationJob(ctx, source, userID, FormationPurposeAgentSave)
}

func (s *Store) enqueueFormationJob(ctx context.Context, source FormationSource, userID, purpose string) (int64, bool, error) {
	if source.TurnID <= 0 || strings.TrimSpace(userID) == "" {
		return 0, false, fmt.Errorf("formation job requires tenant and source turn")
	}
	var storedRequestID, storedSessionID string
	var storedGeneration int
	if err := s.sql.QueryRowContext(ctx, `SELECT source_request_id, session_id, session_generation FROM session_turns WHERE id = ? AND canonical_user_id = ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL`, source.TurnID, userID).Scan(&storedRequestID, &storedSessionID, &storedGeneration); err != nil {
		return 0, false, fmt.Errorf("resolve formation job source turn: %w", err)
	}
	if source.RequestID == "" {
		source.RequestID = storedRequestID
	}
	if source.SessionID == "" {
		source.SessionID = storedSessionID
	}
	if source.SessionGeneration <= 0 {
		source.SessionGeneration = storedGeneration
	}
	if source.RequestID != storedRequestID || source.SessionID != storedSessionID || source.SessionGeneration != storedGeneration {
		return 0, false, fmt.Errorf("formation job source scope does not match persisted turn")
	}
	version := firstNonEmptyFormation(source.ExtractorVersion, FormationExtractorVersion)
	key := fmt.Sprintf("turn:%d:%s", source.TurnID, version)
	if purpose == FormationPurposeAgentSave {
		key = fmt.Sprintf("turn:%d:%s:%s", source.TurnID, purpose, version)
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, state, source_request_id,
	source_session_id, source_session_generation, source_turn_id, extraction_model,
		extractor_version, formation_purpose, available_at, updated_at
	)
VALUES ('memory_formation', ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING
`, userID, key, source.RequestID, source.SessionID, source.SessionGeneration, source.TurnID,
		source.Model, version, purpose, formatTime(now), formatTime(now))
	if err != nil {
		return 0, false, fmt.Errorf("enqueue memory formation job: %w", err)
	}
	created, _ := result.RowsAffected()
	var id int64
	if err := s.sql.QueryRowContext(ctx, `SELECT id FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = ? AND idempotency_key = ?`, userID, key).Scan(&id); err != nil {
		return 0, false, err
	}
	return id, created == 1, nil
}

// MarkFormationEligible records successful response delivery before enqueue.
func (s *Store) MarkFormationEligible(ctx context.Context, userID string, turnID int64) error {
	now := formatTime(time.Now().UTC())
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark turn eligible: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	lateDelivery, err := sessionTurnHadDeliveryFailureTx(ctx, tx, userID, turnID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_turns SET delivered_at = COALESCE(delivered_at, ?), delivery_failed_at = NULL WHERE id = ? AND canonical_user_id = ?`, now, turnID, userID)
	if err != nil {
		return fmt.Errorf("mark turn eligible for memory formation: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	if lateDelivery {
		if err := invalidateCompactionAfterLateDeliveryTx(ctx, tx, userID, turnID); err != nil {
			return err
		}
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "session_turn", turnID, "upsert", "delivered:"+now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark turn eligible: %w", err)
	}
	s.signalDerivedIndex()
	return nil
}

// ReconcileFormationJobs restores jobs for recent completed turns whose
// post-delivery enqueue was interrupted.
func (s *Store) ReconcileFormationJobs(ctx context.Context, model, version string) (int64, error) {
	version = firstNonEmptyFormation(version, FormationExtractorVersion)
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, state, source_request_id,
	source_session_id, source_session_generation, source_turn_id, extraction_model,
		extractor_version, formation_purpose, available_at, updated_at
	)
SELECT 'memory_formation', turns.canonical_user_id, 'turn:' || turns.id || ':' || ?, 'queued',
	turns.source_request_id, turns.session_id, turns.session_generation, turns.id, ?, ?, 'background_pattern', ?, ?
FROM session_turns turns
WHERE turns.created_at >= ? AND turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL
	AND NOT EXISTS (
		SELECT 1 FROM durable_jobs jobs
		WHERE jobs.job_kind = 'memory_formation' AND jobs.canonical_user_id = turns.canonical_user_id
			AND jobs.idempotency_key = 'turn:' || turns.id || ':' || ?
	)
`, version, model, version, formatTime(now), formatTime(now), formatTime(now.Add(-24*time.Hour)), version)
	if err != nil {
		return 0, fmt.Errorf("reconcile memory formation jobs: %w", err)
	}
	return result.RowsAffected()
}

// ReconcilePatternFormationJobs deterministically rebuilds missing frozen
// windows for recently delivered anchor turns.
func (s *Store) ReconcilePatternFormationJobs(ctx context.Context, model string) (int64, error) {
	return s.reconcileWindowFormationJobs(ctx, model, PatternExtractorVersion)
}

// ReconcileAssessmentFormationJobs backfills recent delivered assessment anchors.
func (s *Store) ReconcileAssessmentFormationJobs(ctx context.Context, model string) (int64, error) {
	return s.reconcileWindowFormationJobs(ctx, model, AssessmentExtractorVersion)
}

func (s *Store) reconcileWindowFormationJobs(ctx context.Context, model, version string) (int64, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT canonical_user_id, source_request_id, session_id, session_generation, id FROM session_turns WHERE created_at >= ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL ORDER BY id`, formatTime(time.Now().UTC().Add(-24*time.Hour)))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var sources []struct {
		userID string
		source FormationSource
	}
	for rows.Next() {
		var item struct {
			userID string
			source FormationSource
		}
		if err := rows.Scan(&item.userID, &item.source.RequestID, &item.source.SessionID, &item.source.SessionGeneration, &item.source.TurnID); err != nil {
			return 0, err
		}
		item.source.Model = model
		sources = append(sources, item)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var count int64
	for _, item := range sources {
		_, created, err := s.enqueueWindowFormationJob(ctx, item.source, item.userID, version)
		if err != nil {
			return count, err
		}
		if created {
			count++
		}
	}
	return count, nil
}

// ReconcileAgentSaveFormationJobs restores missing jobs for recently delivered
// turns with a nonempty immutable foreground-memory artifact.
func (s *Store) ReconcileAgentSaveFormationJobs(ctx context.Context, model string) (int64, error) {
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `
INSERT INTO durable_jobs (
	job_kind, canonical_user_id, idempotency_key, state, source_request_id,
	source_session_id, source_session_generation, source_turn_id, extraction_model,
	extractor_version, formation_purpose, available_at, updated_at
)
SELECT 'memory_formation', turns.canonical_user_id,
	'turn:' || turns.id || ':agent_save:`+AgentSaveExtractorVersion+`', 'queued',
	turns.source_request_id, turns.session_id, turns.session_generation, turns.id, ?,
	'`+AgentSaveExtractorVersion+`', 'agent_save', ?, ?
FROM session_turns turns
WHERE turns.created_at >= ? AND turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL
	AND json_array_length(turns.foreground_memory, '$.candidates') > 0
	AND NOT EXISTS (
		SELECT 1 FROM durable_jobs jobs
		WHERE jobs.job_kind = 'memory_formation' AND jobs.canonical_user_id = turns.canonical_user_id
			AND jobs.idempotency_key = 'turn:' || turns.id || ':agent_save:`+AgentSaveExtractorVersion+`'
	)
`, model, formatTime(now), formatTime(now), formatTime(now.Add(-24*time.Hour)))
	if err != nil {
		return 0, fmt.Errorf("reconcile agent-save formation jobs: %w", err)
	}
	return result.RowsAffected()
}

// ClaimFormationJob leases the oldest ready job.
func (s *Store) ClaimFormationJob(ctx context.Context, lease time.Duration) (FormationJob, error) {
	if lease <= 0 {
		lease = time.Minute
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	leaseOwner, err := newLeaseOwner()
	if err != nil {
		return FormationJob{}, fmt.Errorf("create memory formation lease owner: %w", err)
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return FormationJob{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	var job FormationJob
	err = tx.QueryRowContext(ctx, `
SELECT id, canonical_user_id, source_request_id, source_session_id,
	source_session_generation, COALESCE(source_turn_id, 0), extraction_model,
	extractor_version, formation_purpose, attempt_count, invalid_output_retry_count, last_error_code,
	model_submission_count, corrective_error_code
FROM durable_jobs
WHERE job_kind = 'memory_formation' AND ((state IN ('queued', 'retry') AND available_at <= ?)
	OR (state = 'running' AND lease_until <= ?))
	`+formationSourceFenceSQL+`
ORDER BY available_at, id LIMIT 1
	`, formatTime(now), formatTime(now)).Scan(&job.ID, &job.UserID, &job.RequestID, &job.SessionID,
		&job.SessionGeneration, &job.TurnID, &job.Model, &job.ExtractorVersion, &job.Purpose, &job.AttemptCount, &job.InvalidOutputRetryCount, &job.LastErrorCode,
		&job.ModelSubmissionCount, &job.CorrectiveErrorCode)
	if err != nil {
		return FormationJob{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET state = 'running', attempt_count = attempt_count + 1, lease_owner = ?, lease_until = ?, updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? `+formationSourceFenceSQL, leaseOwner, formatTime(leaseUntil), formatTime(now), job.ID, job.UserID)
	if err != nil {
		return FormationJob{}, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return FormationJob{}, sql.ErrNoRows
	}
	job.AttemptCount++
	job.LeaseOwner = leaseOwner
	job.LeaseUntil = leaseUntil
	if err := tx.Commit(); err != nil {
		return FormationJob{}, err
	}
	return job, nil
}

// RenewFormationJobLease extends an exactly owned, still-live formation lease.
func (s *Store) RenewFormationJobLease(ctx context.Context, job FormationJob, lease time.Duration) (time.Time, error) {
	if lease <= 0 {
		return time.Time{}, fmt.Errorf("renew memory formation lease: duration must be positive")
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET lease_until = ?, updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) `+formationSourceFenceSQL, formatTime(leaseUntil), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now))
	if err := requireFormationLeaseMutation(result, err); err != nil {
		return time.Time{}, err
	}
	return leaseUntil, nil
}

// ReserveFormationModelSubmission transactionally consumes one provider
// submission immediately before invocation under the exact live lease.
func (s *Store) ReserveFormationModelSubmission(ctx context.Context, job FormationJob) (int, error) {
	if job.Purpose == FormationPurposeAgentSave {
		return job.ModelSubmissionCount, fmt.Errorf("agent-save formation jobs do not use model submissions")
	}
	now := time.Now().UTC()
	var count int
	err := s.sql.QueryRowContext(ctx, `UPDATE durable_jobs
SET model_submission_count = model_submission_count + 1, updated_at = ?
WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ?
	AND formation_purpose != 'agent_save' AND state = 'running' AND lease_owner = ? AND lease_until = ?
	AND julianday(lease_until) > julianday(?) AND model_submission_count < ? `+formationSourceFenceSQL+`
RETURNING model_submission_count`, formatTime(now), job.ID, job.UserID, job.LeaseOwner,
		formatTime(job.LeaseUntil), formatTime(now), DurableModelSubmissionLimit).Scan(&count)
	if err == nil {
		return count, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("reserve memory formation model submission: %w", err)
	}
	var storedCount int
	if readErr := s.sql.QueryRowContext(ctx, `SELECT model_submission_count FROM durable_jobs
WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ?`, job.ID, job.UserID).Scan(&storedCount); readErr == nil && storedCount >= DurableModelSubmissionLimit {
		return storedCount, ErrModelSubmissionBudgetExhausted
	}
	return 0, ErrStaleFormationJobLease
}

// RefundFormationModelSubmission restores the current reservation owned by
// the exact live lease when foreground work intentionally preempts it.
func (s *Store) RefundFormationModelSubmission(ctx context.Context, job FormationJob) error {
	if job.Purpose == FormationPurposeAgentSave || job.ModelSubmissionCount <= 0 {
		return fmt.Errorf("refund memory formation model submission: invalid reservation")
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs
SET model_submission_count = model_submission_count - 1, updated_at = ?
WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ?
	AND formation_purpose = 'background_pattern' AND state = 'running'
	AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?)
	AND model_submission_count = ? `+formationSourceFenceSQL,
		formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil),
		formatTime(now), job.ModelSubmissionCount)
	return requireFormationLeaseMutation(result, err)
}

// FormationJobArtifact returns the first persisted extractor result for replay.
func (s *Store) FormationJobArtifact(ctx context.Context, job FormationJob) (string, error) {
	var payload string
	now := time.Now().UTC()
	err := s.sql.QueryRowContext(ctx, `SELECT extraction_payload FROM durable_jobs WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) `+formationSourceFenceSQL, job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now)).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", ErrStaleFormationJobLease
	}
	return payload, err
}

// ValidateFormationJobLease verifies exact ownership of a currently live lease.
func (s *Store) ValidateFormationJobLease(ctx context.Context, job FormationJob) error {
	var exists int
	now := time.Now().UTC()
	err := s.sql.QueryRowContext(ctx, `SELECT 1 FROM durable_jobs WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) `+formationSourceFenceSQL, job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now)).Scan(&exists)
	if err == sql.ErrNoRows {
		return ErrStaleFormationJobLease
	}
	return err
}

// SaveFormationJobArtifact persists the first extractor result and never revises it.
func (s *Store) SaveFormationJobArtifact(ctx context.Context, job FormationJob, payload string) error {
	if strings.TrimSpace(payload) == "" {
		payload = "[]"
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET extraction_payload = CASE WHEN extraction_payload = '' THEN ? ELSE extraction_payload END, updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) `+formationSourceFenceSQL, payload, formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now))
	return requireFormationLeaseMutation(result, err)
}

// CompleteFormationJob records a terminal successful or skipped state.
func (s *Store) CompleteFormationJob(ctx context.Context, job FormationJob, skipped bool) error {
	state := "succeeded"
	if skipped {
		state = "skipped"
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = ?, completed_at = ?, lease_owner = '', lease_until = NULL, updated_at = ?, last_error_code = '', corrective_error_code = '' WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) `+formationSourceFenceSQL, state, formatTime(now), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now))
	return requireFormationLeaseMutation(result, err)
}

// SkipFormationJob terminally skips a running job that cannot succeed by retrying.
func (s *Store) SkipFormationJob(ctx context.Context, job FormationJob, code string) error {
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'skipped', completed_at = ?, lease_owner = '', lease_until = NULL, last_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? `+formationSourceFenceSQL, formatTime(now), safeErrorCode(code), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil))
	return requireFormationLeaseMutation(result, err)
}

// RetryFormationJob releases a failed lease with bounded exponential backoff,
// returning the persisted retry/dead state only after a successful mutation.
func (s *Store) RetryFormationJob(ctx context.Context, job FormationJob, code string, maxAttempts int) (string, error) {
	now := time.Now().UTC()
	state := "retry"
	if job.ModelSubmissionCount >= DurableModelSubmissionLimit || (job.Purpose == FormationPurposeAgentSave && maxAttempts > 0 && job.AttemptCount >= maxAttempts) {
		state = "dead"
	}
	delay := time.Duration(1<<min(job.AttemptCount, 6)) * time.Second
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = ?, available_at = ?, lease_owner = '', lease_until = NULL, completed_at = CASE WHEN ? = 'dead' THEN ? ELSE NULL END, last_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? `+formationSourceFenceSQL, state, formatTime(now.Add(delay)), state, formatTime(now), safeErrorCode(code), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil))
	if err := requireFormationLeaseMutation(result, err); err != nil {
		return "", err
	}
	return state, nil
}

// RetryInvalidFormationJob records the one reason-aware structured-output retry.
// Its model submission has already consumed the shared durable budget.
func (s *Store) RetryInvalidFormationJob(ctx context.Context, job FormationJob, code string) error {
	now := time.Now().UTC()
	delay := time.Duration(1<<min(job.AttemptCount, 6)) * time.Second
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'retry', invalid_output_retry_count = invalid_output_retry_count + 1, available_at = ?, lease_owner = '', lease_until = NULL, completed_at = NULL, last_error_code = ?, corrective_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND invalid_output_retry_count = 0 AND model_submission_count < ? AND extraction_payload = '' `+formationSourceFenceSQL, formatTime(now.Add(delay)), safeErrorCode(code), safeErrorCode(code), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil), DurableModelSubmissionLimit)
	return requireFormationLeaseMutation(result, err)
}

// DeferFormationJob releases a lease preempted by foreground work without
// consuming the job's provider retry budget.
func (s *Store) DeferFormationJob(ctx context.Context, job FormationJob, delay time.Duration) error {
	if delay <= 0 {
		delay = time.Second
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'retry', attempt_count = MAX(attempt_count - 1, 0), available_at = ?, lease_owner = '', lease_until = NULL, last_error_code = 'foreground_preempted', updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? `+formationSourceFenceSQL, formatTime(now.Add(delay)), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil))
	return requireFormationLeaseMutation(result, err)
}

func requireFormationLeaseMutation(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStaleFormationJobLease
	}
	return nil
}

// FormationJobState returns one tenant-owned job state for observability/tests.
func (s *Store) FormationJobState(ctx context.Context, userID string, jobID int64) (string, error) {
	var state string
	err := s.sql.QueryRowContext(ctx, `SELECT state FROM durable_jobs WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ?`, jobID, userID).Scan(&state)
	return state, err
}

// FormationPatternContext loads and fences every turn in an immutable pattern window.
func (s *Store) FormationPatternContext(ctx context.Context, job FormationJob) (PatternContext, error) {
	if job.ExtractorVersion != PatternExtractorVersion || job.Purpose != FormationPurposeBackgroundPattern {
		return PatternContext{}, fmt.Errorf("job is not a pattern extraction")
	}
	var payload string
	if err := s.sql.QueryRowContext(ctx, `SELECT artifact_payload FROM durable_jobs WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ?`, job.ID, job.UserID).Scan(&payload); err != nil {
		return PatternContext{}, err
	}
	window, err := DecodePatternContext([]byte(payload))
	if err != nil {
		return PatternContext{}, fmt.Errorf("decode frozen pattern context: %w", err)
	}
	if window.TurnIDs[len(window.TurnIDs)-1] != job.TurnID {
		return PatternContext{}, fmt.Errorf("pattern context does not end at anchor")
	}
	for _, id := range window.TurnIDs {
		var turn StoredSessionTurn
		err := s.sql.QueryRowContext(ctx, `SELECT id, canonical_user_id, session_id, session_generation, user_text FROM session_turns WHERE id = ? AND canonical_user_id = ? AND session_id = ? AND session_generation = ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL`, id, job.UserID, job.SessionID, job.SessionGeneration).Scan(&turn.ID, &turn.UserID, &turn.SessionID, &turn.Generation, &turn.UserText)
		if err != nil {
			return PatternContext{}, fmt.Errorf("load frozen pattern turn %d: %w", id, err)
		}
		window.Turns = append(window.Turns, turn)
	}
	return window, nil
}

func (s *Store) formationStage(stage string) error {
	if s.formationFailpoint != nil {
		return s.formationFailpoint(stage)
	}
	return nil
}

func formationKey(values ...any) string {
	var parts []string
	for _, value := range values {
		parts = append(parts, fmt.Sprint(value))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func nullableFormationTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatTime(value)
}

func firstNonEmptyFormation(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func safeErrorCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	if len(value) > 80 {
		value = value[:80]
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

const formationSourceFenceSQL = `
	AND EXISTS (
		SELECT 1 FROM session_turns source
		WHERE source.id = durable_jobs.source_turn_id
			AND source.canonical_user_id = durable_jobs.canonical_user_id
			AND source.session_id = durable_jobs.source_session_id
			AND source.session_generation = durable_jobs.source_session_generation
			AND source.source_request_id = durable_jobs.source_request_id
			AND source.delivered_at IS NOT NULL
			AND source.delivery_failed_at IS NULL
	)`
