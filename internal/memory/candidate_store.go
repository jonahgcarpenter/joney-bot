package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

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
	case policy.ApprovalApproved:
		state = "approved"
	case policy.ApprovalRejected:
		state = "rejected"
	default:
		switch proposal.Output.Decision {
		case policy.DecisionAutomatic, policy.DecisionInferredActive, policy.DecisionShortTerm:
			state = "approved"
		case policy.DecisionDisallowed:
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
				(proposal.Output.Mode == policy.ModeExplicitRemember && candidateEvidenceAtLeastAsStrong(string(proposal.Output.Provenance), proposal.Output.Confidence, existingProvenance, existingConfidence)) {
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
	rows, err := tx.QueryContext(ctx, `SELECT candidate.id, candidate.confidence, candidate.importance, candidate.sensitivity, candidate.source_turn_id, candidate.published_memory_id FROM memory_candidates candidate JOIN session_turns turn ON turn.id = candidate.source_turn_id AND turn.canonical_user_id = candidate.canonical_user_id WHERE candidate.canonical_user_id = ? AND candidate.formation_mode = ? AND candidate.provenance_type = ? AND candidate.claim_slot = ? AND candidate.claim_value = ? AND candidate.state != 'rejected' AND turn.delivered_at IS NOT NULL AND turn.delivery_failed_at IS NULL ORDER BY candidate.confidence DESC, candidate.source_turn_id, candidate.id`, job.UserID, policy.ModeBackgroundPattern, policy.ProvenanceModelInference, strings.TrimSpace(claimSlot), strings.TrimSpace(claimValue))
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
	sensitivity := string(policy.SensitivityLow)
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
	if proposal.Output.Mode == policy.ModeExplicitRemember || mode == string(policy.ModeExplicitRemember) {
		mode = string(policy.ModeExplicitRemember)
	} else if proposal.Output.Mode == policy.ModeBackgroundPattern && existing.PublishedMemoryID == 0 && provenance == string(policy.ProvenanceModelInference) {
		mode = string(policy.ModeBackgroundPattern)
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
	if candidate.SourceTurnID <= 0 || candidate.FormationMode == string(policy.ModeBackgroundPattern) {
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
	args := []any{memoryID, reason, formatTime(now), candidate.UserID, candidate.Scope, candidate.ClaimSlot, candidate.ClaimValue, policy.ModeBackgroundPattern, candidate.UserID}
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
	case string(policy.ProvenanceUserStatement):
		return 3
	case string(policy.ProvenanceModelInference):
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
	case string(policy.ProvenanceUserStatement):
		return string(policy.AuthorityUserDirect)
	case string(policy.ProvenanceModelInference):
		return string(policy.AuthorityModel)
	default:
		return "unknown"
	}
}

func strongestSensitivity(oldSensitivity, newSensitivity string) string {
	rank := func(value string) int {
		switch value {
		case string(policy.SensitivityHighImpactInteraction):
			return 3
		case string(policy.SensitivityIdentityOrContact):
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
