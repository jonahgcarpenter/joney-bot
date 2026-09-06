package usermemory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

const (
	FormationExtractorVersion   = "formation-v4"
	AgentSaveExtractorVersion   = "agent-save-v1"
	DurableModelSubmissionLimit = 3

	FormationPurposeBackgroundPattern = "background_pattern"
	FormationPurposeAgentSave         = "agent_save"
)

// ErrStaleFormationJobLease indicates that the exact claimed lease is no longer live.
var ErrStaleFormationJobLease = errors.New("stale memory formation job lease")

// ErrModelSubmissionBudgetExhausted indicates that a durable model-backed job
// has already reserved every allowed provider submission.
var ErrModelSubmissionBudgetExhausted = errors.New("durable model submission budget exhausted")

// ErrStaleSessionCompactionJobLease indicates that the exact compaction lease is no longer live.
var ErrStaleSessionCompactionJobLease = errors.New("stale session compaction job lease")

// FormationSource identifies the canonical turn and request that formed memory.
type FormationSource struct {
	RequestID         string
	SessionID         string
	SessionGeneration int
	TurnID            int64
	Model             string
	ExtractorVersion  string
}

// CandidateProposal is one validated policy result ready for canonical staging.
type CandidateProposal struct {
	Output               memoryformation.CandidateOutput
	Source               FormationSource
	IdempotencyKey       string
	TargetMemoryID       int64
	SupersedesStatement  string
	RequireCorroboration bool
	FormationJob         *FormationJob
	CompactionJob        *SessionCompactionJob
}

// FormationCandidate is a persisted memory proposal.
type FormationCandidate struct {
	ID                  int64
	UserID              string
	State               string
	UpdatedAt           time.Time
	Scope               string
	Category            string
	Statement           string
	Evidence            string
	Confidence          float64
	Importance          int
	Provenance          string
	SourceAuthority     string
	Sensitivity         string
	FormationMode       string
	DecisionReason      string
	SourceRequestID     string
	SourceSessionID     string
	SourceGeneration    int
	SourceTurnID        int64
	ExtractionModel     string
	ExtractorVersion    string
	SupersedesMemoryID  int64
	SupersedesStatement string
	PublishedMemoryID   int64
	ExpiresAt           time.Time
	ClaimSlot           string
	ClaimValue          string
}

// FormationJob is one leased post-turn extraction operation.
type FormationJob struct {
	ID                      int64
	UserID                  string
	RequestID               string
	SessionID               string
	SessionGeneration       int
	TurnID                  int64
	Model                   string
	ExtractorVersion        string
	Purpose                 string
	AttemptCount            int
	InvalidOutputRetryCount int
	LastErrorCode           string
	ModelSubmissionCount    int
	CorrectiveErrorCode     string
	LeaseOwner              string
	LeaseUntil              time.Time
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

// StoredSessionTurn identifies an exchange that was actually persisted.
type StoredSessionTurn struct {
	ID         int64
	UserID     string
	SessionID  string
	Generation int
	UserText   string
}

// ProposeCandidate persists a validated decision once per tenant/idempotency key.
func (s *Store) ProposeCandidate(ctx context.Context, userID string, proposal CandidateProposal) (FormationCandidate, bool, error) {
	if job := proposal.CompactionJob; job != nil {
		if userID != job.UserID || proposal.Source.SessionID != job.SessionID || proposal.Source.SessionGeneration != job.SessionGeneration || proposal.Source.TurnID < job.CoveredFromTurnID || proposal.Source.TurnID > job.CoveredThroughTurnID {
			return FormationCandidate{}, false, fmt.Errorf("pre-compaction candidate scope does not match fenced job")
		}
	}
	if err := s.ensureAccountUser(userID); err != nil {
		return FormationCandidate{}, false, err
	}
	unlock := s.lockUsers(userID)
	defer unlock()
	key := strings.TrimSpace(proposal.IdempotencyKey)
	if key == "" {
		key = formationKey(
			proposal.Source.RequestID, proposal.Source.TurnID, proposal.Output.Statement, proposal.Output.Evidence,
			proposal.Output.ClaimSlot, proposal.Output.ClaimValue, proposal.Output.Scope, proposal.Output.Category, proposal.Output.Provenance,
			proposal.Output.Context, proposal.Output.Confidence, proposal.Output.TTL, proposal.SupersedesStatement,
			proposal.Output.Mode, proposal.Source.ExtractorVersion,
		)
	}
	state := "proposed"
	decisionReason := proposal.Output.Reason
	switch proposal.Output.Approval {
	case memoryformation.ApprovalApproved:
		state = "approved"
	case memoryformation.ApprovalRejected:
		state = "rejected"
	default:
		switch proposal.Output.Decision {
		case memoryformation.DecisionAutomatic, memoryformation.DecisionInferredActive, memoryformation.DecisionShortTerm:
			state = "approved"
		case memoryformation.DecisionDisallowed:
			state = "rejected"
		}
	}
	if proposal.RequireCorroboration && state == "approved" {
		state = "proposed"
		decisionReason = "candidate requires corroboration"
	}
	blockedConflict := false
	now := time.Now().UTC()
	var expires any
	if proposal.Output.TTL > 0 {
		expires = formatTime(now.Add(proposal.Output.TTL))
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return FormationCandidate{}, false, fmt.Errorf("begin memory candidate application: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	var supersedesID int64
	if target := strings.TrimSpace(proposal.SupersedesStatement); target != "" {
		var id int64
		var existingConfidence float64
		var existingProvenance string
		id, err = resolveActiveMemoryByStatementTx(ctx, tx, userID, string(proposal.Output.Scope), target)
		if err != nil {
			return FormationCandidate{}, false, fmt.Errorf("resolve candidate supersession: %w", err)
		}
		if id > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT confidence, provenance_type FROM memory_entries WHERE id = ? AND canonical_user_id = ? AND scope = ? AND status = 'active'`, id, userID, proposal.Output.Scope).Scan(&existingConfidence, &existingProvenance); err != nil {
				return FormationCandidate{}, false, fmt.Errorf("read candidate supersession strength: %w", err)
			}
			if candidateEvidenceAtLeastAsStrong(string(proposal.Output.Provenance), proposal.Output.Confidence, existingProvenance, existingConfidence) {
				supersedesID = id
			} else if state == "approved" {
				blockedConflict = true
				decisionReason = "replacement evidence is weaker than the active memory"
			}
		}
	}
	if supersedesID == 0 && state == "approved" && proposal.Output.ClaimSlot != "" && !strings.HasSuffix(proposal.Output.ClaimSlot, ".fact") {
		var id int64
		var existingConfidence float64
		var existingProvenance string
		err := tx.QueryRowContext(ctx, `SELECT id, confidence, provenance_type FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND claim_slot = ? AND claim_value != ? AND status = 'active' ORDER BY CASE provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, confidence DESC, updated_at DESC, id DESC LIMIT 1`, userID, proposal.Output.Scope, proposal.Output.ClaimSlot, proposal.Output.ClaimValue).Scan(&id, &existingConfidence, &existingProvenance)
		if err != nil {
			if err != sql.ErrNoRows {
				return FormationCandidate{}, false, fmt.Errorf("resolve conflicting memory claim: %w", err)
			}
		}
		if id > 0 {
			if candidateEvidenceStronger(string(proposal.Output.Provenance), proposal.Output.Confidence, existingProvenance, existingConfidence) ||
				(proposal.Output.Mode == memoryformation.ModeExplicitRemember && candidateEvidenceAtLeastAsStrong(string(proposal.Output.Provenance), proposal.Output.Confidence, existingProvenance, existingConfidence)) {
				supersedesID = id
				decisionReason = "stronger evidence supersedes a conflicting claim"
			} else {
				blockedConflict = true
				decisionReason = "conflicting evidence is weaker than the active claim"
			}
		}
	}
	if job := proposal.FormationJob; job != nil {
		isPattern := job.ExtractorVersion == PatternExtractorVersion && job.Purpose == FormationPurposeBackgroundPattern
		if userID != job.UserID || proposal.Source.SessionID != job.SessionID || proposal.Source.SessionGeneration != job.SessionGeneration || job.LeaseOwner == "" || job.LeaseUntil.IsZero() || (!isPattern && (proposal.Source.RequestID != job.RequestID || proposal.Source.TurnID != job.TurnID)) {
			return FormationCandidate{}, false, fmt.Errorf("formation candidate scope does not match fenced job")
		}
		fenceSQL := `
UPDATE durable_jobs SET updated_at = updated_at
WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running'
	AND formation_purpose = ? AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?)
	` + formationSourceFenceSQL + `
	AND EXISTS (
		SELECT 1 FROM account_users active
		WHERE active.canonical_user_id = durable_jobs.canonical_user_id
			AND active.lifecycle_state = 'active'
		)`
		args := []any{job.ID, job.UserID, job.Purpose, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now)}
		if isPattern {
			fenceSQL += ` AND EXISTS (SELECT 1 FROM json_each(durable_jobs.artifact_payload, '$.turn_ids') member WHERE member.type = 'integer' AND member.value = ?)
	AND EXISTS (SELECT 1 FROM session_turns observed WHERE observed.id = ? AND observed.canonical_user_id = durable_jobs.canonical_user_id AND observed.session_id = durable_jobs.source_session_id AND observed.session_generation = durable_jobs.source_session_generation AND observed.delivered_at IS NOT NULL AND observed.delivery_failed_at IS NULL)`
			args = append(args, proposal.Source.TurnID, proposal.Source.TurnID)
		}
		fenced, err := tx.ExecContext(ctx, fenceSQL, args...)
		if err != nil {
			return FormationCandidate{}, false, fmt.Errorf("fence formation candidate: %w", err)
		}
		if count, _ := fenced.RowsAffected(); count != 1 {
			return FormationCandidate{}, false, ErrStaleFormationJobLease
		}
	}
	if job := proposal.CompactionJob; job != nil {
		fenced, err := tx.ExecContext(ctx, `
UPDATE durable_jobs SET updated_at = updated_at
WHERE id = ? AND job_kind = 'session_compaction' AND canonical_user_id = ? AND session_id = ? AND session_generation = ?
	AND covered_from_turn_id = ? AND covered_through_turn_id = ?
	AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?)
	AND EXISTS (
		SELECT 1 FROM sessions active
		WHERE active.canonical_user_id = durable_jobs.canonical_user_id
			AND active.session_id = durable_jobs.session_id
			AND active.generation = durable_jobs.session_generation
			AND active.is_active = 1
			AND julianday(active.expires_at) > julianday(?)
	)
	AND ? BETWEEN covered_from_turn_id AND covered_through_turn_id
	AND EXISTS (
		SELECT 1 FROM session_turns source
		WHERE source.id = ? AND source.canonical_user_id = durable_jobs.canonical_user_id
			AND source.session_id = durable_jobs.session_id
			AND source.session_generation = durable_jobs.session_generation
			AND source.delivered_at IS NOT NULL
	)`,
			job.ID, job.UserID, job.SessionID, job.SessionGeneration,
			job.CoveredFromTurnID, job.CoveredThroughTurnID, job.LeaseOwner,
			formatTime(job.LeaseUntil), formatTime(now), formatTime(now), proposal.Source.TurnID, proposal.Source.TurnID)
		if err != nil {
			return FormationCandidate{}, false, fmt.Errorf("fence pre-compaction candidate: %w", err)
		}
		if count, _ := fenced.RowsAffected(); count != 1 {
			return FormationCandidate{}, false, ErrStaleSessionCompactionJobLease
		}
	}
	if proposal.Source.TurnID > 0 {
		candidate, reconciled, publishable, err := s.reconcileSameTurnCandidateTx(ctx, tx, userID, proposal, state, decisionReason, blockedConflict, supersedesID, now)
		if err != nil {
			return FormationCandidate{}, false, err
		}
		if reconciled {
			if publishable && !proposal.RequireCorroboration {
				if _, err := s.publishCandidateTx(ctx, tx, candidate, proposal.TargetMemoryID); err != nil {
					return FormationCandidate{}, false, err
				}
				candidate, err = loadCandidateTx(ctx, tx, userID, candidate.ID)
				if err != nil {
					return FormationCandidate{}, false, err
				}
			} else if candidate.State == "proposed" && candidate.PublishedMemoryID == 0 && !proposal.RequireCorroboration {
				if _, err := s.attachCandidateToActiveClaimTx(ctx, tx, candidate, now); err != nil {
					return FormationCandidate{}, false, err
				}
				candidate, err = loadCandidateTx(ctx, tx, userID, candidate.ID)
				if err != nil {
					return FormationCandidate{}, false, err
				}
			}
			if err := tx.Commit(); err != nil {
				return FormationCandidate{}, false, err
			}
			if candidate.PublishedMemoryID > 0 {
				s.signalDerivedIndex()
			}
			return candidate, false, nil
		}
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO memory_candidates (
	canonical_user_id, idempotency_key, state, scope, category, statement,
	evidence, confidence, importance, provenance_type, source_turn_id,
	extraction_model, extractor_version, formation_mode, sensitivity,
	supersedes_memory_id, created_at, updated_at, expires_at,
	decision_reason, claim_slot, claim_value
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, idempotency_key) DO NOTHING
`, userID, key, state, proposal.Output.Scope, proposal.Output.Category, proposal.Output.Statement,
		proposal.Output.Evidence, proposal.Output.Confidence, proposal.Output.Importance, proposal.Output.Provenance,
		nullableID(proposal.Source.TurnID), proposal.Source.Model, firstNonEmptyFormation(proposal.Source.ExtractorVersion, FormationExtractorVersion),
		proposal.Output.Mode, proposal.Output.Sensitivity, nullableID(supersedesID), formatTime(now), formatTime(now), expires, decisionReason,
		proposal.Output.ClaimSlot, proposal.Output.ClaimValue)
	if err != nil {
		return FormationCandidate{}, false, fmt.Errorf("insert memory candidate: %w", err)
	}
	created, err := result.RowsAffected()
	if err != nil {
		return FormationCandidate{}, false, fmt.Errorf("check memory candidate insert: %w", err)
	}
	candidate, err := loadCandidateByKeyTx(ctx, tx, userID, key)
	if err != nil {
		return FormationCandidate{}, false, err
	}
	if created == 0 && (candidate.Statement != proposal.Output.Statement || candidate.Evidence != proposal.Output.Evidence || candidate.Scope != string(proposal.Output.Scope) || candidate.Category != string(proposal.Output.Category) || candidate.Provenance != string(proposal.Output.Provenance) || candidate.Sensitivity != string(proposal.Output.Sensitivity) || candidate.FormationMode != string(proposal.Output.Mode) || candidate.Confidence != proposal.Output.Confidence || candidate.Importance != proposal.Output.Importance || candidate.SourceTurnID != proposal.Source.TurnID || candidate.ExtractionModel != proposal.Source.Model || candidate.ExtractorVersion != firstNonEmptyFormation(proposal.Source.ExtractorVersion, FormationExtractorVersion) || candidate.SupersedesMemoryID != supersedesID || candidate.ClaimSlot != proposal.Output.ClaimSlot || candidate.ClaimValue != proposal.Output.ClaimValue) {
		return FormationCandidate{}, false, fmt.Errorf("memory candidate idempotency payload mismatch")
	}
	if created == 1 && candidate.State == "approved" && !blockedConflict && candidate.PublishedMemoryID == 0 && !proposal.RequireCorroboration {
		if _, err := s.publishCandidateTx(ctx, tx, candidate, proposal.TargetMemoryID); err != nil {
			return FormationCandidate{}, false, err
		}
		candidate, err = loadCandidateTx(ctx, tx, userID, candidate.ID)
		if err != nil {
			return FormationCandidate{}, false, err
		}
	} else if candidate.State == "proposed" && candidate.PublishedMemoryID == 0 && !proposal.RequireCorroboration {
		if _, err := s.attachCandidateToActiveClaimTx(ctx, tx, candidate, now); err != nil {
			return FormationCandidate{}, false, err
		}
		candidate, err = loadCandidateTx(ctx, tx, userID, candidate.ID)
		if err != nil {
			return FormationCandidate{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return FormationCandidate{}, false, fmt.Errorf("commit memory candidate proposal: %w", err)
	}
	if candidate.PublishedMemoryID > 0 {
		s.signalDerivedIndex()
	}
	return candidate, created == 1, nil
}

// AggregatePatternCandidates promotes repeated unpublished observations in one
// lease-fenced transaction and links every contribution to one canonical memory.
func (s *Store) AggregatePatternCandidates(ctx context.Context, job FormationJob, claimSlot, claimValue string) (int64, error) {
	if job.ExtractorVersion != PatternExtractorVersion || job.Purpose != FormationPurposeBackgroundPattern {
		return 0, fmt.Errorf("aggregate requires a pattern job")
	}
	unlock := s.lockUsers(job.UserID)
	defer unlock()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // nolint:errcheck
	now := time.Now().UTC()
	fenced, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET updated_at = updated_at WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND extractor_version = ? AND formation_purpose = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) `+formationSourceFenceSQL, job.ID, job.UserID, PatternExtractorVersion, FormationPurposeBackgroundPattern, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now))
	if err != nil {
		return 0, err
	}
	if count, _ := fenced.RowsAffected(); count != 1 {
		return 0, ErrStaleFormationJobLease
	}
	rows, err := tx.QueryContext(ctx, `SELECT candidate.id, candidate.confidence, candidate.importance, candidate.sensitivity, candidate.source_turn_id, candidate.published_memory_id FROM memory_candidates candidate JOIN session_turns turn ON turn.id = candidate.source_turn_id AND turn.canonical_user_id = candidate.canonical_user_id WHERE candidate.canonical_user_id = ? AND candidate.formation_mode = ? AND candidate.provenance_type = ? AND candidate.claim_slot = ? AND candidate.claim_value = ? AND candidate.state != 'rejected' AND turn.delivered_at IS NOT NULL AND turn.delivery_failed_at IS NULL ORDER BY candidate.confidence DESC, candidate.source_turn_id, candidate.id`, job.UserID, memoryformation.ModeBackgroundPattern, memoryformation.ProvenanceModelInference, strings.TrimSpace(claimSlot), strings.TrimSpace(claimValue))
	if err != nil {
		return 0, err
	}
	type contribution struct {
		id          int64
		confidence  float64
		importance  int
		sensitivity string
		turnID      int64
		memoryID    sql.NullInt64
	}
	var contributions []contribution
	for rows.Next() {
		var item contribution
		if err := rows.Scan(&item.id, &item.confidence, &item.importance, &item.sensitivity, &item.turnID, &item.memoryID); err != nil {
			rows.Close()
			return 0, err
		}
		contributions = append(contributions, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	seenTurns := make(map[int64]struct{}, len(contributions))
	for _, item := range contributions {
		seenTurns[item.turnID] = struct{}{}
	}
	if len(seenTurns) < 2 {
		return 0, nil
	}
	confidence := contributions[0].confidence
	importance := 1
	sensitivity := string(memoryformation.SensitivityLow)
	for _, item := range contributions {
		importance = max(importance, item.importance)
		sensitivity = strongestSensitivity(sensitivity, item.sensitivity)
	}
	if confidence < 0.35 {
		return 0, nil
	}
	representative, err := loadCandidateTx(ctx, tx, job.UserID, contributions[0].id)
	if err != nil {
		return 0, err
	}
	var publicationCandidateID int64
	for _, item := range contributions {
		if !item.memoryID.Valid {
			publicationCandidateID = item.id
			break
		}
	}
	if publicationCandidateID == 0 {
		return 0, nil
	}
	representative.Confidence = confidence
	representative.Importance = importance
	representative.Sensitivity = sensitivity
	representative.ID = publicationCandidateID
	representative.PublishedMemoryID = 0
	representative.State = "approved"
	if !strings.HasSuffix(representative.ClaimSlot, ".fact") {
		var conflictID int64
		var conflictConfidence float64
		var conflictProvenance string
		err := tx.QueryRowContext(ctx, `SELECT id, confidence, provenance_type FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND claim_slot = ? AND claim_value != ? AND status = 'active' ORDER BY CASE provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, confidence DESC, id DESC LIMIT 1`, job.UserID, representative.Scope, representative.ClaimSlot, representative.ClaimValue).Scan(&conflictID, &conflictConfidence, &conflictProvenance)
		if err != nil && err != sql.ErrNoRows {
			return 0, err
		}
		if err == nil {
			if !candidateEvidenceStronger(representative.Provenance, confidence, conflictProvenance, conflictConfidence) {
				if err := tx.Commit(); err != nil {
					return 0, err
				}
				return 0, nil
			}
			representative.SupersedesMemoryID = conflictID
			if _, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET supersedes_memory_id = ? WHERE id = ? AND canonical_user_id = ?`, conflictID, representative.ID, job.UserID); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET state = 'approved', decision_reason = 'repeated implicit observations meet the active memory threshold', supersedes_memory_id = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'proposed' AND published_memory_id IS NULL`, nullableID(representative.SupersedesMemoryID), formatTime(now), representative.ID, job.UserID); err != nil {
		return 0, err
	}
	memoryID, err := s.publishCandidateTx(ctx, tx, representative, 0)
	if err != nil {
		return 0, err
	}
	if _, err := s.consolidateClaimEvidenceTx(ctx, tx, memoryID, representative, true, "repeated implicit observations support an active claim", now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.signalDerivedIndex()
	return memoryID, nil
}

func (s *Store) reconcileSameTurnCandidateTx(ctx context.Context, tx *sql.Tx, userID string, proposal CandidateProposal, incomingState, incomingReason string, incomingBlockedConflict bool, incomingSupersedesID int64, now time.Time) (FormationCandidate, bool, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
SELECT id FROM memory_candidates
WHERE canonical_user_id = ? AND source_turn_id = ? AND evidence = ?
	AND claim_slot = ? AND claim_value = ?
ORDER BY CASE WHEN published_memory_id IS NOT NULL THEN 0 ELSE 1 END,
	CASE state WHEN 'approved' THEN 0 WHEN 'proposed' THEN 1 ELSE 2 END,
	id
LIMIT 1`, userID, proposal.Source.TurnID, proposal.Output.Evidence, proposal.Output.ClaimSlot, proposal.Output.ClaimValue).Scan(&id)
	if err == sql.ErrNoRows {
		return FormationCandidate{}, false, false, nil
	}
	if err != nil {
		return FormationCandidate{}, false, false, fmt.Errorf("find equivalent same-turn memory candidate: %w", err)
	}
	existing, err := loadCandidateTx(ctx, tx, userID, id)
	if err != nil {
		return FormationCandidate{}, false, false, err
	}

	state, reason := existing.State, existing.DecisionReason
	blockedConflict := existing.State == "approved" && existing.PublishedMemoryID == 0
	incomingEligible := incomingState != "rejected"
	incomingPreferred := incomingEligible && (provenanceAuthorityRank(string(proposal.Output.Provenance)) > provenanceAuthorityRank(existing.Provenance) ||
		(provenanceAuthorityRank(string(proposal.Output.Provenance)) == provenanceAuthorityRank(existing.Provenance) && proposal.Output.Confidence > existing.Confidence))
	shouldReplacePolicy := candidateStateRank(incomingState) > candidateStateRank(existing.State) || (candidateStateRank(incomingState) == candidateStateRank(existing.State) && incomingPreferred)
	if shouldReplacePolicy {
		state, reason = incomingState, incomingReason
		if existing.PublishedMemoryID == 0 {
			blockedConflict = incomingBlockedConflict
		}
	}
	confidence, importance := existing.Confidence, existing.Importance
	provenance, sensitivity := existing.Provenance, existing.Sensitivity
	if incomingEligible {
		confidence = max(existing.Confidence, proposal.Output.Confidence)
		importance = max(existing.Importance, proposal.Output.Importance)
		provenance = strongestMemoryProvenance(existing.Provenance, string(proposal.Output.Provenance))
		sensitivity = strongestSensitivity(existing.Sensitivity, string(proposal.Output.Sensitivity))
	}
	statement, scope, category := existing.Statement, existing.Scope, existing.Category
	expiresAt := nullableFormationTime(existing.ExpiresAt)
	if incomingPreferred {
		statement, scope, category = proposal.Output.Statement, string(proposal.Output.Scope), string(proposal.Output.Category)
		if proposal.Output.TTL > 0 {
			expiresAt = formatTime(now.Add(proposal.Output.TTL))
		} else {
			expiresAt = nil
		}
	}
	claimSlot, claimValue := existing.ClaimSlot, existing.ClaimValue
	mode := existing.FormationMode
	if proposal.Output.Mode == memoryformation.ModeExplicitRemember || mode == string(memoryformation.ModeExplicitRemember) {
		mode = string(memoryformation.ModeExplicitRemember)
	} else if proposal.Output.Mode == memoryformation.ModeBackgroundPattern && existing.PublishedMemoryID == 0 && provenance == string(memoryformation.ProvenanceModelInference) {
		mode = string(memoryformation.ModeBackgroundPattern)
	}
	supersedesID := existing.SupersedesMemoryID
	if incomingSupersedesID > 0 && incomingEligible && (supersedesID == 0 || incomingPreferred) {
		supersedesID = incomingSupersedesID
	}
	if existing.PublishedMemoryID > 0 && supersedesID == existing.PublishedMemoryID {
		supersedesID = 0
	}
	model, extractorVersion := existing.ExtractionModel, existing.ExtractorVersion
	if model == "" {
		model = proposal.Source.Model
	}
	if extractorVersion == "" {
		extractorVersion = firstNonEmptyFormation(proposal.Source.ExtractorVersion, FormationExtractorVersion)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE memory_candidates SET state = ?, scope = ?, category = ?, statement = ?,
	confidence = ?, importance = ?, provenance_type = ?, extraction_model = ?,
	extractor_version = ?, formation_mode = ?, sensitivity = ?,
	decision_reason = ?, expires_at = ?,
	supersedes_memory_id = ?, claim_slot = ?, claim_value = ?, updated_at = ?
WHERE id = ? AND canonical_user_id = ?`, state, scope, category, statement,
		confidence, importance, provenance, model, extractorVersion, mode, sensitivity,
		reason, expiresAt,
		nullableID(supersedesID), claimSlot, claimValue, formatTime(now), existing.ID, userID)
	if err != nil {
		return FormationCandidate{}, false, false, fmt.Errorf("reconcile equivalent memory candidate: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return FormationCandidate{}, false, false, fmt.Errorf("reconcile equivalent memory candidate: candidate changed")
	}
	if existing.PublishedMemoryID > 0 {
		var memoryScope, memoryCategory, memoryStatement, memoryProvenance, memorySensitivity, memoryClaimSlot, memoryClaimValue string
		var memoryConfidence float64
		var memoryImportance int
		var memoryExpires sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT scope, category, statement, confidence, importance, provenance_type, sensitivity, expires_at, claim_slot, claim_value FROM memory_entries WHERE id = ? AND canonical_user_id = ? AND status = 'active'`, existing.PublishedMemoryID, userID).Scan(&memoryScope, &memoryCategory, &memoryStatement, &memoryConfidence, &memoryImportance, &memoryProvenance, &memorySensitivity, &memoryExpires, &memoryClaimSlot, &memoryClaimValue); err != nil {
			return FormationCandidate{}, false, false, fmt.Errorf("read published same-turn memory: %w", err)
		}
		memoryPreferred := provenanceAuthorityRank(provenance) > provenanceAuthorityRank(memoryProvenance) || (provenanceAuthorityRank(provenance) == provenanceAuthorityRank(memoryProvenance) && confidence > memoryConfidence)
		canonicalScope, canonicalCategory, canonicalStatement := memoryScope, memoryCategory, memoryStatement
		canonicalClaimSlot, canonicalClaimValue := memoryClaimSlot, memoryClaimValue
		canonicalExpires := any(nil)
		if memoryExpires.Valid {
			canonicalExpires = memoryExpires.String
		}
		if memoryPreferred {
			canonicalScope, canonicalCategory, canonicalStatement = scope, category, statement
			canonicalClaimSlot, canonicalClaimValue, canonicalExpires = claimSlot, claimValue, expiresAt
		}
		canonicalProvenance := strongestMemoryProvenance(memoryProvenance, provenance)
		canonicalSensitivity := strongestSensitivity(memorySensitivity, sensitivity)
		if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET scope = ?, category = ?, statement = ?, confidence = MAX(confidence, ?), importance = MAX(importance, ?), provenance_type = ?, sensitivity = ?, expires_at = ?, supersedes_id = CASE WHEN ? > 0 THEN ? ELSE supersedes_id END, claim_slot = ?, claim_value = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND status = 'active'`, canonicalScope, canonicalCategory, canonicalStatement, confidence, importance, canonicalProvenance, canonicalSensitivity, canonicalExpires, supersedesID, nullableID(supersedesID), canonicalClaimSlot, canonicalClaimValue, formatTime(now), existing.PublishedMemoryID, userID); err != nil {
			return FormationCandidate{}, false, false, fmt.Errorf("reconcile published same-turn memory: %w", err)
		}
		if supersedesID > 0 && supersedesID != existing.PublishedMemoryID {
			if err := s.supersedeActiveMemoryTx(ctx, tx, userID, supersedesID, existing.PublishedMemoryID, now); err != nil {
				return FormationCandidate{}, false, false, err
			}
		}
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", existing.PublishedMemoryID, "upsert", "same-turn-reconcile:"+formatTime(now)); err != nil {
			return FormationCandidate{}, false, false, err
		}
		if _, _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
			return FormationCandidate{}, false, false, fmt.Errorf("refresh profile after same-turn reconciliation: %w", err)
		}
	}
	merged, err := loadCandidateTx(ctx, tx, userID, existing.ID)
	return merged, true, merged.State == "approved" && merged.PublishedMemoryID == 0 && !blockedConflict, err
}

func candidateStateRank(state string) int {
	switch state {
	case "approved":
		return 3
	case "proposed":
		return 2
	default:
		return 1
	}
}

// publishCandidateTx applies one approved candidate inside its proposal transaction.
func (s *Store) publishCandidateTx(ctx context.Context, tx *sql.Tx, candidate FormationCandidate, targetMemoryID int64) (int64, error) {
	userID, candidateID := candidate.UserID, candidate.ID
	if candidate.PublishedMemoryID > 0 {
		return candidate.PublishedMemoryID, nil
	}
	if candidate.State != "approved" {
		return 0, fmt.Errorf("memory candidate %d is not publishable", candidateID)
	}
	if err := s.formationStage("validated"); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var duplicateID int64
	var err error
	if targetMemoryID > 0 {
		err = tx.QueryRowContext(ctx, `SELECT id FROM memory_entries WHERE id = ? AND canonical_user_id = ? AND scope = ? AND status = 'active' AND claim_slot = ? AND claim_value = ?`, targetMemoryID, userID, candidate.Scope, candidate.ClaimSlot, candidate.ClaimValue).Scan(&duplicateID)
		if err != nil && err != sql.ErrNoRows {
			return 0, fmt.Errorf("validate reinforcement target: %w", err)
		}
	}
	if duplicateID == 0 {
		err = tx.QueryRowContext(ctx, `SELECT id FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND status = 'active' AND claim_slot = ? AND claim_value = ? ORDER BY id LIMIT 1`, userID, candidate.Scope, candidate.ClaimSlot, candidate.ClaimValue).Scan(&duplicateID)
	}
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("read duplicate active memory: %w", err)
	}
	if err == nil {
		if candidate.SupersedesMemoryID > 0 && candidate.SupersedesMemoryID != duplicateID {
			if err := s.supersedeActiveMemoryTx(ctx, tx, userID, candidate.SupersedesMemoryID, duplicateID, now); err != nil {
				return 0, err
			}
			if _, _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
				return 0, fmt.Errorf("advance profile after duplicate correction: %w", err)
			}
		}
		if err := markCandidatePublishedTx(ctx, tx, candidate, duplicateID); err != nil {
			return 0, err
		}
		if _, err := s.consolidateClaimEvidenceTx(ctx, tx, duplicateID, candidate, false, "observation supports an already-active claim", now); err != nil {
			return 0, err
		}
		if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", duplicateID, "upsert", "reinforce:"+formatTime(now)); err != nil {
			return 0, err
		}
		if _, _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
			return 0, fmt.Errorf("advance profile after memory reinforcement: %w", err)
		}
		return duplicateID, nil
	}
	var memoryID int64
	var inactiveID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND status IN ('expired', 'superseded') AND claim_slot = ? AND claim_value = ? ORDER BY updated_at DESC, id DESC LIMIT 1`, userID, candidate.Scope, candidate.ClaimSlot, candidate.ClaimValue).Scan(&inactiveID)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("read inactive duplicate memory: %w", err)
	}
	if err == nil {
		if candidate.SupersedesMemoryID == inactiveID {
			candidate.SupersedesMemoryID = 0
		}
		err = tx.QueryRowContext(ctx, `
UPDATE memory_entries SET category = ?, statement = ?, confidence = ?, importance = ?,
	status = 'active', updated_at = ?, expires_at = ?, supersedes_id = ?,
	provenance_type = ?, sensitivity = ?, claim_slot = ?, claim_value = ?
WHERE id = ? AND canonical_user_id = ?
RETURNING id
`, candidate.Category, candidate.Statement, candidate.Confidence,
			candidate.Importance, formatTime(now), nullableFormationTime(candidate.ExpiresAt), nullableID(candidate.SupersedesMemoryID),
			candidate.Provenance, candidate.Sensitivity,
			candidate.ClaimSlot, candidate.ClaimValue, inactiveID, userID).Scan(&memoryID)
	} else {
		err = tx.QueryRowContext(ctx, `
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, confidence,
	importance, status, created_at, updated_at, expires_at, supersedes_id, provenance_type,
	sensitivity, claim_slot, claim_value
)
VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id
`, userID, candidate.Scope, candidate.Category, candidate.Statement,
			candidate.Confidence, candidate.Importance, formatTime(now), formatTime(now), nullableFormationTime(candidate.ExpiresAt), nullableID(candidate.SupersedesMemoryID),
			candidate.Provenance, candidate.Sensitivity, candidate.ClaimSlot, candidate.ClaimValue).Scan(&memoryID)
	}
	if err != nil {
		return 0, fmt.Errorf("insert active memory: %w", err)
	}
	if err := s.formationStage("canonical_written"); err != nil {
		return 0, err
	}
	if candidate.SupersedesMemoryID > 0 {
		if err := s.supersedeActiveMemoryTx(ctx, tx, userID, candidate.SupersedesMemoryID, memoryID, now); err != nil {
			return 0, err
		}
	}
	if err := s.formationStage("supersession_written"); err != nil {
		return 0, err
	}
	if err := markCandidatePublishedTx(ctx, tx, candidate, memoryID); err != nil {
		return 0, err
	}
	if _, err := s.consolidateClaimEvidenceTx(ctx, tx, memoryID, candidate, false, "observation supports an already-active claim", now); err != nil {
		return 0, err
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", memoryID, "upsert", "publish:"+formatTime(now)); err != nil {
		return 0, err
	}
	if err := s.formationStage("vector_written"); err != nil {
		return 0, err
	}
	if _, _, err := refreshProfileTx(ctx, tx, userID, now); err != nil {
		return 0, fmt.Errorf("advance profile after memory publication: %w", err)
	}
	if err := s.formationStage("profile_written"); err != nil {
		return 0, err
	}
	if err := s.formationStage("candidate_published"); err != nil {
		return 0, err
	}
	return memoryID, nil
}

func (s *Store) attachCandidateToActiveClaimTx(ctx context.Context, tx *sql.Tx, candidate FormationCandidate, now time.Time) (int64, error) {
	if candidate.SourceTurnID <= 0 || candidate.FormationMode == string(memoryformation.ModeBackgroundPattern) {
		return 0, nil
	}
	var memoryID int64
	err := tx.QueryRowContext(ctx, `
SELECT memory.id
FROM memory_entries memory
JOIN session_turns source ON source.id = ? AND source.canonical_user_id = memory.canonical_user_id
WHERE memory.canonical_user_id = ? AND memory.scope = ? AND memory.claim_slot = ? AND memory.claim_value = ?
	AND memory.status = 'active' AND source.delivered_at IS NOT NULL AND source.delivery_failed_at IS NULL
ORDER BY memory.id LIMIT 1`, candidate.SourceTurnID, candidate.UserID, candidate.Scope, candidate.ClaimSlot, candidate.ClaimValue).Scan(&memoryID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find active memory for proposed observation: %w", err)
	}
	if _, err := s.consolidateClaimEvidenceTx(ctx, tx, memoryID, candidate, false, "observation supports an already-active claim", now); err != nil {
		return 0, err
	}
	if err := enqueueDerivedChangeTx(ctx, tx, candidate.UserID, "memory", memoryID, "upsert", "evidence-link:"+formatTime(now)); err != nil {
		return 0, err
	}
	if _, _, err := refreshProfileTx(ctx, tx, candidate.UserID, now); err != nil {
		return 0, fmt.Errorf("refresh profile after evidence attachment: %w", err)
	}
	return memoryID, nil
}

func (s *Store) consolidateClaimEvidenceTx(ctx context.Context, tx *sql.Tx, memoryID int64, candidate FormationCandidate, includePatterns bool, reason string, now time.Time) (int64, error) {
	modeFilter := `AND candidate.formation_mode != ?`
	args := []any{memoryID, reason, formatTime(now), candidate.UserID, candidate.Scope, candidate.ClaimSlot, candidate.ClaimValue, memoryformation.ModeBackgroundPattern, candidate.UserID}
	if includePatterns {
		modeFilter = ""
		args = []any{memoryID, reason, formatTime(now), candidate.UserID, candidate.Scope, candidate.ClaimSlot, candidate.ClaimValue, candidate.UserID}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE memory_candidates AS candidate
SET state = 'approved', published_memory_id = ?, decision_reason = ?, updated_at = ?
WHERE candidate.canonical_user_id = ? AND candidate.scope = ? AND candidate.claim_slot = ? AND candidate.claim_value = ?
	AND candidate.state = 'proposed' AND candidate.published_memory_id IS NULL
	`+modeFilter+`
	AND candidate.source_turn_id IN (
		SELECT source.id FROM session_turns source
		WHERE source.canonical_user_id = ? AND source.delivered_at IS NOT NULL AND source.delivery_failed_at IS NULL
	)`, args...)
	if err != nil {
		return 0, fmt.Errorf("attach proposed claim evidence: %w", err)
	}
	attached, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count attached claim evidence: %w", err)
	}

	var statement, category, provenance, sensitivity string
	var expires sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT statement, category, provenance_type, sensitivity, expires_at
FROM memory_candidates
WHERE canonical_user_id = ? AND published_memory_id = ?
ORDER BY CASE provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC,
	confidence DESC, id
LIMIT 1`, candidate.UserID, memoryID).Scan(&statement, &category, &provenance, &sensitivity, &expires); err != nil {
		return 0, fmt.Errorf("select strongest claim assessment: %w", err)
	}
	var confidence float64
	var importance int
	if err := tx.QueryRowContext(ctx, `SELECT MAX(confidence), MAX(importance) FROM memory_candidates WHERE canonical_user_id = ? AND published_memory_id = ?`, candidate.UserID, memoryID).Scan(&confidence, &importance); err != nil {
		return 0, fmt.Errorf("aggregate claim assessments: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT sensitivity FROM memory_candidates WHERE canonical_user_id = ? AND published_memory_id = ?`, candidate.UserID, memoryID)
	if err != nil {
		return 0, fmt.Errorf("read claim sensitivities: %w", err)
	}
	for rows.Next() {
		var assessment string
		if err := rows.Scan(&assessment); err != nil {
			rows.Close()
			return 0, err
		}
		sensitivity = strongestSensitivity(sensitivity, assessment)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var expiresAt any
	if expires.Valid {
		expiresAt = expires.String
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET statement = ?, category = ?, confidence = ?, importance = ?, provenance_type = ?, sensitivity = ?, expires_at = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND status = 'active'`, statement, category, confidence, importance, provenance, sensitivity, expiresAt, formatTime(now), memoryID, candidate.UserID); err != nil {
		return 0, fmt.Errorf("recompute canonical memory from claim evidence: %w", err)
	}
	return attached, nil
}

// EnqueueFormationJob records one replay-safe extraction job per source turn/version.
func (s *Store) EnqueueFormationJob(ctx context.Context, source FormationSource, userID string) (int64, error) {
	id, _, err := s.enqueueFormationJob(ctx, source, userID, FormationPurposeBackgroundPattern)
	return id, err
}

// EnqueuePatternFormationJob freezes the newest eligible window ending at the
// anchor turn. A one-turn session intentionally produces no background job.
func (s *Store) EnqueuePatternFormationJob(ctx context.Context, source FormationSource, userID string) (int64, bool, error) {
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
	rows, err := tx.QueryContext(ctx, `SELECT id FROM session_turns WHERE canonical_user_id = ? AND session_id = ? AND session_generation = ? AND id <= ? AND delivered_at IS NOT NULL AND delivery_failed_at IS NULL ORDER BY id DESC LIMIT ?`, userID, anchor.SessionID, anchor.Generation, source.TurnID, MaxPatternContextTurns)
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
	if len(reversed) < 2 {
		return 0, false, nil
	}
	turnIDs := make([]int64, len(reversed))
	for i := range reversed {
		turnIDs[len(reversed)-1-i] = reversed[i]
	}
	payload, err := MarshalPatternContext(turnIDs)
	if err != nil {
		return 0, false, err
	}
	source.ExtractorVersion = PatternExtractorVersion
	key := fmt.Sprintf("turn:%d:%s", source.TurnID, PatternExtractorVersion)
	now := formatTime(time.Now().UTC())
	result, err := tx.ExecContext(ctx, `
INSERT INTO durable_jobs (job_kind, canonical_user_id, idempotency_key, state, source_request_id,
	source_session_id, source_session_generation, source_turn_id, extraction_model,
	extractor_version, formation_purpose, artifact_payload, available_at, updated_at)
VALUES ('memory_formation', ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING`, userID, key, source.RequestID, anchor.SessionID, anchor.Generation, source.TurnID, source.Model, PatternExtractorVersion, FormationPurposeBackgroundPattern, string(payload), now, now)
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
		_, created, err := s.EnqueuePatternFormationJob(ctx, item.source, item.userID)
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

// RetryFormationJob releases a failed lease with bounded exponential backoff.
func (s *Store) RetryFormationJob(ctx context.Context, job FormationJob, code string, maxAttempts int) error {
	now := time.Now().UTC()
	state := "retry"
	if job.ModelSubmissionCount >= DurableModelSubmissionLimit || (job.Purpose == FormationPurposeAgentSave && maxAttempts > 0 && job.AttemptCount >= maxAttempts) {
		state = "dead"
	}
	delay := time.Duration(1<<min(job.AttemptCount, 6)) * time.Second
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = ?, available_at = ?, lease_owner = '', lease_until = NULL, completed_at = CASE WHEN ? = 'dead' THEN ? ELSE NULL END, last_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? `+formationSourceFenceSQL, state, formatTime(now.Add(delay)), state, formatTime(now), safeErrorCode(code), formatTime(now), job.ID, job.UserID, job.LeaseOwner, formatTime(job.LeaseUntil))
	return requireFormationLeaseMutation(result, err)
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

// SessionTurnByID reloads a source turn under canonical tenant scope.
func (s *Store) SessionTurnByID(ctx context.Context, userID string, turnID int64) (StoredSessionTurn, error) {
	var turn StoredSessionTurn
	err := s.sql.QueryRowContext(ctx, `SELECT id, canonical_user_id, session_id, session_generation, user_text FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turnID, userID).Scan(&turn.ID, &turn.UserID, &turn.SessionID, &turn.Generation, &turn.UserText)
	return turn, err
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

const candidateSelect = `SELECT memory_candidates.id, memory_candidates.canonical_user_id, memory_candidates.state, memory_candidates.updated_at, memory_candidates.scope, memory_candidates.category, memory_candidates.statement, memory_candidates.evidence, memory_candidates.confidence, memory_candidates.importance, memory_candidates.provenance_type, memory_candidates.sensitivity, memory_candidates.formation_mode, memory_candidates.decision_reason, COALESCE(source.source_request_id, ''), COALESCE(source.session_id, ''), COALESCE(source.session_generation, 0), COALESCE(memory_candidates.source_turn_id, 0), memory_candidates.extraction_model, memory_candidates.extractor_version, COALESCE(memory_candidates.supersedes_memory_id, 0), COALESCE((SELECT statement FROM memory_entries WHERE id = memory_candidates.supersedes_memory_id), ''), COALESCE(memory_candidates.published_memory_id, 0), memory_candidates.expires_at, memory_candidates.claim_slot, memory_candidates.claim_value FROM memory_candidates LEFT JOIN session_turns source ON source.id = memory_candidates.source_turn_id AND source.canonical_user_id = memory_candidates.canonical_user_id`

func loadCandidateByKeyTx(ctx context.Context, tx *sql.Tx, userID, key string) (FormationCandidate, error) {
	return scanFormationCandidate(tx.QueryRowContext(ctx, candidateSelect+` WHERE memory_candidates.canonical_user_id = ? AND memory_candidates.idempotency_key = ?`, userID, key))
}

func loadCandidateTx(ctx context.Context, tx *sql.Tx, userID string, id int64) (FormationCandidate, error) {
	return scanFormationCandidate(tx.QueryRowContext(ctx, candidateSelect+` WHERE memory_candidates.canonical_user_id = ? AND memory_candidates.id = ?`, userID, id))
}

func scanFormationCandidate(row interface{ Scan(...any) error }) (FormationCandidate, error) {
	var candidate FormationCandidate
	var updated string
	var expires sql.NullString
	err := row.Scan(&candidate.ID, &candidate.UserID, &candidate.State,
		&updated, &candidate.Scope, &candidate.Category,
		&candidate.Statement, &candidate.Evidence, &candidate.Confidence, &candidate.Importance,
		&candidate.Provenance, &candidate.Sensitivity,
		&candidate.FormationMode, &candidate.DecisionReason,
		&candidate.SourceRequestID, &candidate.SourceSessionID, &candidate.SourceGeneration,
		&candidate.SourceTurnID, &candidate.ExtractionModel, &candidate.ExtractorVersion,
		&candidate.SupersedesMemoryID, &candidate.SupersedesStatement, &candidate.PublishedMemoryID,
		&expires, &candidate.ClaimSlot, &candidate.ClaimValue)
	if err != nil {
		return FormationCandidate{}, err
	}
	if expires.Valid {
		candidate.ExpiresAt = parseTime(expires.String)
	}
	candidate.UpdatedAt = parseTime(updated)
	candidate.SourceAuthority = sourceAuthorityForProvenance(candidate.Provenance)
	return candidate, nil
}

func provenanceAuthorityRank(provenance string) int {
	switch provenance {
	case string(memoryformation.ProvenanceUserStatement):
		return 3
	case string(memoryformation.ProvenanceModelInference):
		return 2
	default:
		return 1
	}
}

func candidateEvidenceStronger(newProvenance string, newConfidence float64, oldProvenance string, oldConfidence float64) bool {
	newRank, oldRank := provenanceAuthorityRank(newProvenance), provenanceAuthorityRank(oldProvenance)
	return newRank > oldRank || (newRank == oldRank && newConfidence > oldConfidence)
}

func candidateEvidenceAtLeastAsStrong(newProvenance string, newConfidence float64, oldProvenance string, oldConfidence float64) bool {
	newRank, oldRank := provenanceAuthorityRank(newProvenance), provenanceAuthorityRank(oldProvenance)
	return newRank > oldRank || (newRank == oldRank && newConfidence >= oldConfidence)
}

func strongestMemoryProvenance(oldProvenance, newProvenance string) string {
	if provenanceAuthorityRank(newProvenance) > provenanceAuthorityRank(oldProvenance) {
		return newProvenance
	}
	return oldProvenance
}

func sourceAuthorityForProvenance(provenance string) string {
	switch provenance {
	case string(memoryformation.ProvenanceUserStatement):
		return string(memoryformation.AuthorityUserDirect)
	case string(memoryformation.ProvenanceModelInference):
		return string(memoryformation.AuthorityModel)
	default:
		return "unknown"
	}
}

func strongestSensitivity(oldSensitivity, newSensitivity string) string {
	rank := func(value string) int {
		switch value {
		case string(memoryformation.SensitivityHighImpactInteraction):
			return 3
		case string(memoryformation.SensitivityIdentityOrContact):
			return 2
		default:
			return 1
		}
	}
	if rank(newSensitivity) > rank(oldSensitivity) {
		return newSensitivity
	}
	return oldSensitivity
}

func (s *Store) supersedeActiveMemoryTx(ctx context.Context, tx *sql.Tx, userID string, oldMemoryID, replacementMemoryID int64, now time.Time) error {
	if oldMemoryID <= 0 || oldMemoryID == replacementMemoryID {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
UPDATE memory_entries SET status = 'superseded', updated_at = ?
WHERE id = ? AND canonical_user_id = ? AND status = 'active'
`, formatTime(now), oldMemoryID, userID)
	if err != nil {
		return fmt.Errorf("supersede old memory: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("superseded memory is no longer active")
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "memory", oldMemoryID, "delete", "supersede:"+formatTime(now)); err != nil {
		return err
	}
	return nil
}

func markCandidatePublishedTx(ctx context.Context, tx *sql.Tx, candidate FormationCandidate, memoryID int64) error {
	now := formatTime(time.Now().UTC())
	result, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET published_memory_id = ?, updated_at = ? WHERE id = ? AND canonical_user_id = ? AND state = 'approved' AND published_memory_id IS NULL`, memoryID, now, candidate.ID, candidate.UserID)
	if err != nil {
		return fmt.Errorf("mark memory candidate published: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("memory candidate publication lost idempotency race")
	}
	return nil
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
