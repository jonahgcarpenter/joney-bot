package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func fenceAssessmentTx(ctx context.Context, tx *sql.Tx, job FormationJob, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET updated_at = updated_at WHERE id = ? AND job_kind = 'memory_formation' AND canonical_user_id = ? AND extractor_version = ? AND formation_purpose = ? AND source_turn_id = ? AND state = 'running' AND lease_owner = ? AND lease_until = ? AND julianday(lease_until) > julianday(?) `+formationSourceFenceSQL, job.ID, job.UserID, job.ExtractorVersion, job.Purpose, job.TurnID, job.LeaseOwner, formatTime(job.LeaseUntil), formatTime(now))
	if err := requireFormationLeaseMutation(result, err); err != nil {
		return err
	}
	var eligible bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_turns t JOIN sessions s ON s.canonical_user_id=t.canonical_user_id AND s.session_id=t.session_id AND s.generation=t.session_generation WHERE t.id=? AND t.canonical_user_id=? AND t.session_id=? AND t.session_generation=? AND s.is_active=1 AND julianday(s.expires_at)>julianday(?) AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)))`, job.TurnID, job.UserID, job.SessionID, job.SessionGeneration, formatTime(now), formatTime(now)).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return ErrStaleFormationJobLease
	}
	return requireActiveUser(ctx, tx, job.UserID)
}

// FormationAssessmentInput freezes the complete view once under an exact live lease.
func (s *Store) FormationAssessmentInput(ctx context.Context, job FormationJob) (AssessmentInput, error) {
	if job.ExtractorVersion != AssessmentExtractorVersion || job.Purpose != FormationPurposeBackgroundPattern {
		return AssessmentInput{}, fmt.Errorf("not an assessment job")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return AssessmentInput{}, err
	}
	defer tx.Rollback()
	input, err := s.assessmentInputTx(ctx, tx, job, time.Now().UTC())
	if err != nil {
		return AssessmentInput{}, err
	}
	if err := tx.Commit(); err != nil {
		return AssessmentInput{}, err
	}
	return input, nil
}

func (s *Store) assessmentInputTx(ctx context.Context, tx *sql.Tx, job FormationJob, now time.Time) (AssessmentInput, error) {
	input := AssessmentInput{Version: 1, Context: []StoredSessionTurn{}, Memories: []AssessmentMemory{}, Suppressions: []MemorySuppression{}}
	if err := fenceAssessmentTx(ctx, tx, job, now); err != nil {
		return input, err
	}
	var frozen string
	err := tx.QueryRowContext(ctx, `SELECT payload FROM memory_assessment_inputs WHERE job_id = ? AND canonical_user_id = ?`, job.ID, job.UserID).Scan(&frozen)
	if err == nil {
		err = json.Unmarshal([]byte(frozen), &input)
		return input, err
	}
	if err != sql.ErrNoRows {
		return input, err
	}
	ids := []int64{job.TurnID}
	if job.ExtractorVersion == AssessmentExtractorVersion {
		var payload string
		if err := tx.QueryRowContext(ctx, `SELECT artifact_payload FROM durable_jobs WHERE id = ?`, job.ID).Scan(&payload); err != nil {
			return input, err
		}
		var window struct {
			Version int     `json:"version"`
			TurnIDs []int64 `json:"turn_ids"`
		}
		if err := json.Unmarshal([]byte(payload), &window); err != nil {
			return input, err
		}
		if window.Version != 1 || len(window.TurnIDs) < 1 || len(window.TurnIDs) > 8 || window.TurnIDs[len(window.TurnIDs)-1] != job.TurnID {
			return input, fmt.Errorf("invalid frozen assessment membership")
		}
		ids = window.TurnIDs
	}
	for _, id := range ids {
		var turn StoredSessionTurn
		var created, staged string
		err := tx.QueryRowContext(ctx, `SELECT t.id,t.canonical_user_id,t.session_id,t.session_generation,CASE WHEN t.group_gateway!='' THEN t.public_user_text ELSE t.user_text END,t.created_at,t.assistant_text,t.foreground_memory FROM session_turns t JOIN sessions s ON s.canonical_user_id=t.canonical_user_id AND s.session_id=t.session_id AND s.generation=t.session_generation WHERE t.id=? AND t.canonical_user_id=? AND t.session_id=? AND t.session_generation=? AND t.delivered_at IS NOT NULL AND t.delivery_failed_at IS NULL AND s.is_active=1 AND julianday(s.expires_at)>julianday(?) AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?))`, id, job.UserID, job.SessionID, job.SessionGeneration, formatTime(now), formatTime(now)).Scan(&turn.ID, &turn.UserID, &turn.SessionID, &turn.Generation, &turn.UserText, &created, &turn.AssistantResponse, &staged)
		if err != nil {
			return input, err
		}
		if id == job.TurnID {
			artifact, err := DecodeForegroundMemory(staged)
			if err != nil {
				return input, err
			}
			input.Staged = artifact.Candidates
			turn.CreatedAt = parseTime(created)
			turn.AssistantResponse = ""
			input.Anchor = turn
		} else {
			turn.CreatedAt = parseTime(created)
			input.Context = append(input.Context, turn)
		}
	}
	if len(input.Context) > 2 {
		input.Context = input.Context[len(input.Context)-2:]
	}
	input.Observations, err = listMemoryObservationsTx(ctx, tx, job.UserID, now)
	if err != nil {
		return input, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,statement,category,claim_slot,claim_value,provenance_type,confidence,revision,assessment_context FROM memory_entries WHERE canonical_user_id=? AND status='active' AND (expires_at IS NULL OR julianday(expires_at)>julianday(?)) ORDER BY updated_at DESC,id DESC LIMIT 100`, job.UserID, formatTime(now))
	if err != nil {
		return input, err
	}
	for rows.Next() {
		var m AssessmentMemory
		if err := rows.Scan(&m.ID, &m.Statement, &m.Category, &m.ClaimSlot, &m.ClaimValue, &m.Provenance, &m.Confidence, &m.Revision, &m.Context); err != nil {
			rows.Close()
			return input, err
		}
		input.Memories = append(input.Memories, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return input, err
	}
	// Foreground targets may be older than the bounded recent-memory scan.
	for _, staged := range input.Staged {
		if staged.TargetMemoryID <= 0 {
			continue
		}
		found := false
		for _, m := range input.Memories {
			found = found || m.ID == staged.TargetMemoryID
		}
		if found {
			continue
		}
		var m AssessmentMemory
		err := tx.QueryRowContext(ctx, `SELECT id,statement,category,claim_slot,claim_value,provenance_type,confidence,revision,assessment_context FROM memory_entries WHERE id=? AND canonical_user_id=? AND status='active' AND (expires_at IS NULL OR julianday(expires_at)>julianday(?))`, staged.TargetMemoryID, job.UserID, formatTime(now)).Scan(&m.ID, &m.Statement, &m.Category, &m.ClaimSlot, &m.ClaimValue, &m.Provenance, &m.Confidence, &m.Revision, &m.Context)
		if err != nil && err != sql.ErrNoRows {
			return input, err
		}
		if err == nil {
			input.Memories = append(input.Memories, m)
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,statement,claim_slot,claim_value FROM memory_suppressions WHERE canonical_user_id=? AND is_active=1 ORDER BY id DESC LIMIT 100`, job.UserID)
	if err != nil {
		return input, err
	}
	for rows.Next() {
		var r MemorySuppression
		if err := rows.Scan(&r.ID, &r.Statement, &r.ClaimSlot, &r.ClaimValue); err != nil {
			rows.Close()
			return input, err
		}
		input.Suppressions = append(input.Suppressions, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return input, err
	}
	selectAssessmentReferences(&input)
	data, err := json.Marshal(input)
	if err != nil {
		return input, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_assessment_inputs(job_id,canonical_user_id,payload) VALUES(?,?,?)`, job.ID, job.UserID, string(data))
	return input, err
}

// Selection is deterministic and tenant-local. It ranks a bounded canonical pool,
// without a provider call, and preserves whole records rather than joined excerpts.
func selectAssessmentReferences(input *AssessmentInput) {
	query := input.Anchor.UserText
	for _, c := range input.Staged {
		query += " " + c.Statement + " " + c.ClaimSlot + " " + c.ClaimValue
	}
	terms := ftsRecallTerms(query)
	score := func(text string) int {
		tokens := recallTokens(strings.ReplaceAll(text, "_", " "))
		n := 0
		for _, term := range terms {
			if _, ok := tokens[term]; ok {
				n++
			}
		}
		return n
	}
	observationScore := func(o MemoryObservation) int {
		return score(o.Statement + " " + o.Evidence + " " + o.Context + " " + o.ClaimSlot + " " + o.ClaimValue)
	}
	sort.SliceStable(input.Observations, func(i, j int) bool {
		a, b := input.Observations[i], input.Observations[j]
		if x, y := observationScore(a), observationScore(b); x != y {
			return x > y
		}
		if !a.ObservedAt.Equal(b.ObservedAt) {
			return a.ObservedAt.After(b.ObservedAt)
		}
		return a.ID > b.ID
	})
	selected := make([]MemoryObservation, 0, 6)
	seen := map[int64]bool{input.Anchor.ID: true}
	for _, o := range input.Observations {
		if seen[o.SourceTurnID] || o.SourceTurnID > input.Anchor.ID || observationScore(o) == 0 {
			continue
		}
		selected = append(selected, o)
		seen[o.SourceTurnID] = true
		if len(selected) == 6 {
			break
		}
	}
	input.Observations = selected
	memoryScore := func(m AssessmentMemory) int {
		n := score(m.Statement + " " + m.Context + " " + m.ClaimSlot + " " + m.ClaimValue)
		for _, c := range input.Staged {
			if c.TargetMemoryID == m.ID {
				n += len(terms) + 1
			}
		}
		return n
	}
	sort.SliceStable(input.Memories, func(i, j int) bool {
		return memoryScore(input.Memories[i]) > memoryScore(input.Memories[j])
	})
	if len(input.Memories) > 20 {
		input.Memories = input.Memories[:20]
	}
	sort.SliceStable(input.Suppressions, func(i, j int) bool {
		a, b := input.Suppressions[i], input.Suppressions[j]
		return score(a.Statement+" "+a.ClaimSlot+" "+a.ClaimValue) > score(b.Statement+" "+b.ClaimSlot+" "+b.ClaimValue)
	})
	if len(input.Suppressions) > 20 {
		input.Suppressions = input.Suppressions[:20]
	}
}

// ApplyMemoryAssessment applies a complete background batch in one lease-fenced transaction.
func (s *Store) ApplyMemoryAssessment(ctx context.Context, job FormationJob, batch AssessmentBatch) (AssessmentResult, error) {
	if job.ExtractorVersion != AssessmentExtractorVersion || job.Purpose != FormationPurposeBackgroundPattern {
		return AssessmentResult{}, fmt.Errorf("not an assessment job")
	}
	return s.applyAssessment(ctx, job, batch, nil)
}

// ApplyForegroundAssessment applies only the immutable delivered foreground artifact.
func (s *Store) ApplyForegroundAssessment(ctx context.Context, job FormationJob, artifact ForegroundMemoryArtifact) (AssessmentResult, error) {
	if artifact.Version != 3 || job.Purpose != FormationPurposeAgentSave {
		return AssessmentResult{}, fmt.Errorf("not a current foreground assessment")
	}
	return s.applyAssessment(ctx, job, AssessmentBatch{Version: 1, Items: artifact.Candidates}, &artifact)
}

func (s *Store) applyAssessment(ctx context.Context, job FormationJob, batch AssessmentBatch, foreground *ForegroundMemoryArtifact) (AssessmentResult, error) {
	var result AssessmentResult
	payload, err := MarshalAssessmentBatchArtifact(batch)
	if err != nil {
		return result, err
	}
	unlock := s.lockUsers(job.UserID)
	defer unlock()
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if err := fenceAssessmentTx(ctx, tx, job, now); err != nil {
		return result, err
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT digest FROM memory_assessment_receipts WHERE canonical_user_id=? AND source_turn_id=? AND purpose=?`, job.UserID, job.TurnID, job.Purpose).Scan(&previous)
	if err == nil {
		if previous != formationKey(string(payload)) {
			return result, fmt.Errorf("assessment replay payload mismatch")
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return result, err
	}
	if foreground != nil {
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT foreground_memory FROM session_turns WHERE id=? AND canonical_user_id=?`, job.TurnID, job.UserID).Scan(&stored); err != nil {
			return result, err
		}
		artifact, err := DecodeForegroundMemory(stored)
		if err != nil {
			return result, err
		}
		actual, _ := json.Marshal(artifact)
		normalized, err := DecodeAssessmentBatchJSON(payload)
		if err != nil {
			return result, err
		}
		expected, _ := json.Marshal(ForegroundMemoryArtifact{Version: foreground.Version, Candidates: normalized.Items})
		if string(actual) != string(expected) {
			return result, fmt.Errorf("foreground artifact mismatch")
		}
	} else {
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT extraction_payload FROM durable_jobs WHERE id=?`, job.ID).Scan(&stored); err != nil {
			return result, err
		}
		if stored != "" {
			decoded, err := DecodeAssessmentBatchJSON([]byte(stored))
			if err != nil {
				return result, err
			}
			canonical, _ := MarshalAssessmentBatchArtifact(decoded)
			if string(canonical) != string(payload) {
				return result, fmt.Errorf("assessment artifact mismatch")
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET extraction_payload=? WHERE id=? AND canonical_user_id=?`, string(payload), job.ID, job.UserID); err != nil {
				return result, err
			}
		}
	}
	input, err := s.assessmentInputTx(ctx, tx, job, now)
	if err != nil {
		return result, err
	}
items:
	for i, c := range batch.Items {
		if err := ValidateForegroundMemoryCandidate(&c, foreground == nil); err != nil {
			return AssessmentResult{}, err
		}
		// Only the anchor's original user text can supply fresh evidence. Context is not evidence.
		if !strings.Contains(input.Anchor.UserText, c.Evidence) {
			return AssessmentResult{}, fmt.Errorf("assessment evidence is not in original anchor")
		}
		output, err := c.Evaluate(input.Anchor.UserText)
		if err != nil {
			return AssessmentResult{}, err
		}
		if output.Approval == policy.ApprovalRejected {
			result.RejectedCount++
			continue
		}
		c.ClaimSlot, c.ClaimValue = output.ClaimSlot, output.ClaimValue
		blocked, err := suppressedClaimTx(ctx, tx, job.UserID, c.ClaimSlot, c.ClaimValue, job.TurnID)
		if err != nil {
			return AssessmentResult{}, err
		}
		if blocked {
			result.RejectedCount++
			continue
		}
		seenTurns := map[int64]bool{job.TurnID: true}
		for _, id := range c.SourceObservationIDs {
			found := false
			for _, o := range input.Observations {
				if o.ID == id && !seenTurns[o.SourceTurnID] {
					found = true
					seenTurns[o.SourceTurnID] = true
					break
				}
			}
			if !found {
				return AssessmentResult{}, fmt.Errorf("observation outside frozen source membership")
			}
			var live bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_observations WHERE id=? AND canonical_user_id=? AND julianday(expires_at)>julianday(?))`, id, job.UserID, formatTime(now)).Scan(&live); err != nil {
				return AssessmentResult{}, err
			}
			if !live {
				result.RejectedCount++
				continue items
			}
		}
		if c.Retention == "observation" && c.Intent != "retire" {
			created, err := insertObservationTx(ctx, tx, job.UserID, job.TurnID, c, now)
			if err != nil {
				return AssessmentResult{}, err
			}
			if created {
				result.ObservationCount++
			} else {
				result.RejectedCount++
			}
			continue
		}
		var newer bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_entries WHERE canonical_user_id=? AND claim_slot=? AND (?='single' OR claim_value=?) AND assessed_source_turn_id>?)`, job.UserID, c.ClaimSlot, c.Cardinality, c.ClaimValue, job.TurnID).Scan(&newer); err != nil {
			return AssessmentResult{}, err
		}
		if newer {
			result.RejectedCount++
			continue
		}
		if c.TargetMemoryID > 0 {
			found := false
			for _, m := range input.Memories {
				if m.ID == c.TargetMemoryID && (c.ExpectedRevision == 0 || m.Revision == c.ExpectedRevision) {
					found = true
					break
				}
			}
			if !found {
				result.RejectedCount++
				continue
			}
			var revision, source int64
			var slot string
			err := tx.QueryRowContext(ctx, `SELECT revision,assessed_source_turn_id,claim_slot FROM memory_entries WHERE id=? AND canonical_user_id=? AND status='active' AND (expires_at IS NULL OR julianday(expires_at)>julianday(?))`, c.TargetMemoryID, job.UserID, formatTime(now)).Scan(&revision, &source, &slot)
			if err == sql.ErrNoRows {
				result.RejectedCount++
				continue
			}
			if err != nil {
				return AssessmentResult{}, err
			}
			if (c.ExpectedRevision > 0 && revision != c.ExpectedRevision) || source > job.TurnID || slot != c.ClaimSlot {
				result.RejectedCount++
				continue
			}
		}
		if c.Intent == "retire" {
			if err := deletePriorClaimObservationsTx(ctx, tx, job.UserID, c.TargetMemoryID, job.TurnID); err != nil {
				return AssessmentResult{}, err
			}
			if err := assessmentBarrierTx(ctx, tx, job.UserID, c.TargetMemoryID, job.TurnID, now); err != nil {
				return AssessmentResult{}, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET status='superseded',retired_at=?,retirement_reason='retired',assessment_context=?,assessed_source_turn_id=?,updated_at=? WHERE id=? AND canonical_user_id=?`, formatTime(now), c.Context, job.TurnID, formatTime(now), c.TargetMemoryID, job.UserID); err != nil {
				return AssessmentResult{}, err
			}
			if err := enqueueDerivedChangeTx(ctx, tx, job.UserID, "memory", c.TargetMemoryID, "delete", "retire:"+formatTime(now)); err != nil {
				return AssessmentResult{}, err
			}
			if err := rebindProfileCopiesTx(ctx, tx, job.UserID, c.TargetMemoryID, now); err != nil {
				return AssessmentResult{}, err
			}
			result.RetiredCount++
			continue
		}
		proposal := CandidateProposal{Output: output, Source: FormationSource{RequestID: job.RequestID, SessionID: job.SessionID, SessionGeneration: job.SessionGeneration, TurnID: job.TurnID, Model: job.Model, ExtractorVersion: job.ExtractorVersion}, IdempotencyKey: fmt.Sprintf("assessment:%d:%s:%d", job.TurnID, job.Purpose, i), TargetMemoryID: c.TargetMemoryID, Cardinality: c.Cardinality}
		var sameClaimID int64
		var priorProvenance string
		canonicalChanged := true
		err = tx.QueryRowContext(ctx, `SELECT id,provenance_type,(statement IS NOT ? OR confidence IS NOT ? OR provenance_type IS NOT ? OR category IS NOT ? OR assessment_context IS NOT ? OR importance IS NOT ? OR expires_at IS NOT NULL) FROM memory_entries WHERE canonical_user_id=? AND scope=? AND claim_slot=? AND replace(claim_value,'_',' ')=replace(?,'_',' ') AND status='active' ORDER BY id LIMIT 1`, output.Statement, output.Confidence, output.Provenance, output.Category, c.Context, output.Importance, job.UserID, output.Scope, c.ClaimSlot, c.ClaimValue).Scan(&sameClaimID, &priorProvenance, &canonicalChanged)
		if err != nil && err != sql.ErrNoRows {
			return AssessmentResult{}, err
		}
		if sameClaimID > 0 {
			if provenanceAuthorityRank(string(output.Provenance)) < provenanceAuthorityRank(priorProvenance) && c.Intent != "correction" {
				result.RejectedCount++
				continue
			}
			// Prior evidence cannot reassert an older confidence/wording tuple on replay.
			if err := assessmentBarrierTx(ctx, tx, job.UserID, sameClaimID, job.TurnID-1, now); err != nil {
				return AssessmentResult{}, err
			}
		}
		if c.Intent == "correction" {
			if output.Approval != policy.ApprovalApproved {
				result.RejectedCount++
				continue
			}
			if err := deletePriorClaimObservationsTx(ctx, tx, job.UserID, c.TargetMemoryID, job.TurnID); err != nil {
				return AssessmentResult{}, err
			}
			proposal.Output.Mode = policy.ModeExplicitRemember
			proposal.Correction = true
			if err := assessmentBarrierTx(ctx, tx, job.UserID, c.TargetMemoryID, job.TurnID-1, now); err != nil {
				return AssessmentResult{}, err
			}
		}
		var existingActive bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_entries WHERE canonical_user_id=? AND scope=? AND claim_slot=? AND claim_value=? AND status='active')`, job.UserID, output.Scope, c.ClaimSlot, c.ClaimValue).Scan(&existingActive); err != nil {
			return AssessmentResult{}, err
		}
		candidate, _, err := s.proposeCandidateTx(ctx, tx, job.UserID, proposal)
		if err != nil {
			return AssessmentResult{}, err
		}
		if candidate.State == "rejected" {
			result.RejectedCount++
			continue
		}
		if candidate.PublishedMemoryID > 0 {
			var active bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_entries WHERE id=? AND canonical_user_id=? AND status='active')`, candidate.PublishedMemoryID, job.UserID).Scan(&active); err != nil {
				return AssessmentResult{}, err
			}
			if !active {
				result.RejectedCount++
				continue
			}
			if existingActive {
				result.ReinforcedCount++
			} else {
				result.PublishedCount++
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET statement=?,confidence=?,provenance_type=?,category=?,assessment_context=?,assessed_source_turn_id=MAX(assessed_source_turn_id,?),status=CASE WHEN ?<0.35 THEN 'superseded' ELSE 'active' END,retired_at=CASE WHEN ?<0.35 THEN ? ELSE NULL END,retirement_reason=CASE WHEN ?<0.35 THEN 'low_confidence' ELSE '' END,updated_at=? WHERE id=? AND canonical_user_id=?`, output.Statement, output.Confidence, output.Provenance, output.Category, c.Context, job.TurnID, output.Confidence, output.Confidence, formatTime(now), output.Confidence, formatTime(now), candidate.PublishedMemoryID, job.UserID); err != nil {
				return AssessmentResult{}, err
			}
			if output.Confidence < 0.35 {
				result.RetiredCount++
				if err := assessmentBarrierTx(ctx, tx, job.UserID, candidate.PublishedMemoryID, job.TurnID, now); err != nil {
					return AssessmentResult{}, err
				}
			}
			if err := assessmentBarrierTx(ctx, tx, job.UserID, candidate.PublishedMemoryID, job.TurnID, now); err != nil {
				return AssessmentResult{}, err
			}
			if output.Confidence >= 0.35 {
				if _, err := tx.ExecContext(ctx, `UPDATE memory_suppressions SET retained_source_turn_id=? WHERE canonical_user_id=? AND claim_slot=? AND replace(claim_value,'_',' ')=replace(?,'_',' ') AND is_active=0 AND through_turn_id=?`, job.TurnID, job.UserID, c.ClaimSlot, c.ClaimValue, job.TurnID); err != nil {
					return AssessmentResult{}, err
				}
			}
			op := "upsert"
			if output.Confidence < 0.35 {
				op = "delete"
			}
			if err := enqueueDerivedChangeTx(ctx, tx, job.UserID, "memory", candidate.PublishedMemoryID, op, "assessment:"+formatTime(now)); err != nil {
				return AssessmentResult{}, err
			}
			for _, id := range c.SourceObservationIDs {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_observation_evidence(memory_id,observation_id) SELECT ?,? WHERE (SELECT COUNT(*) FROM memory_observation_evidence WHERE memory_id=?)<5 AND EXISTS(SELECT 1 FROM memory_observations WHERE id=? AND canonical_user_id=?)`, candidate.PublishedMemoryID, id, candidate.PublishedMemoryID, id, job.UserID); err != nil {
					return AssessmentResult{}, err
				}
			}
			if canonicalChanged {
				if err := rebindProfileCopiesTx(ctx, tx, job.UserID, candidate.PublishedMemoryID, now); err != nil {
					return AssessmentResult{}, err
				}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_assessment_receipts(canonical_user_id,source_turn_id,purpose,digest) VALUES(?,?,?,?)`, job.UserID, job.TurnID, job.Purpose, formationKey(string(payload))); err != nil {
		return AssessmentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AssessmentResult{}, err
	}
	s.signalDerivedIndex()
	if s.log != nil {
		s.log.Server("user_memory").With(requestctx.LogFields(ctx)...).Info("memory.assessment.applied", "memory assessment transaction committed", config.F("record_kind", "measurement"), config.F("workload", "formation"), config.F("duration_ms", time.Since(now).Milliseconds()), config.F("source_turn_id", job.TurnID), config.F("job_id", job.ID), config.F("user_id", job.UserID), config.F("observation_count", result.ObservationCount), config.F("published_count", result.PublishedCount), config.F("reinforced_count", result.ReinforcedCount), config.F("retired_count", result.RetiredCount), config.F("rejected_count", result.RejectedCount), config.F("status", "ok"))
	}
	return result, nil
}

// Retired/corrected claims retain a non-serving cutoff, not an opposite inferred fact.
func assessmentBarrierTx(ctx context.Context, tx *sql.Tx, userID string, memoryID, through int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_suppressions(canonical_user_id,statement,claim_slot,claim_value,is_active,through_turn_id,created_at) SELECT canonical_user_id,'',claim_slot,claim_value,0,?,? FROM memory_entries WHERE id=? AND canonical_user_id=? ON CONFLICT(canonical_user_id,claim_slot,claim_value) DO UPDATE SET through_turn_id=MAX(through_turn_id,excluded.through_turn_id),retained_source_turn_id=CASE WHEN excluded.through_turn_id>=through_turn_id THEN 0 ELSE retained_source_turn_id END`, through, formatTime(now), memoryID, userID)
	return err
}

func deletePriorClaimObservationsTx(ctx context.Context, tx *sql.Tx, userID string, memoryID, through int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM memory_observations WHERE canonical_user_id=? AND source_turn_id<=? AND EXISTS(SELECT 1 FROM memory_entries m WHERE m.id=? AND m.canonical_user_id=? AND m.claim_slot=memory_observations.claim_slot AND replace(m.claim_value,'_',' ')=replace(memory_observations.claim_value,'_',' '))`, userID, through, memoryID, userID)
	return err
}
