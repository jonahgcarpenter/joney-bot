package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const (
	// ForegroundMemoryVersion is the durable foreground-memory artifact format.
	ForegroundMemoryVersion = 3
	// MaxForegroundMemoryBytes is the maximum encoded artifact size.
	MaxForegroundMemoryBytes = 32 * 1024
)

// ForegroundMemoryArtifact contains untrusted candidate inputs captured during a turn.
type ForegroundMemoryArtifact struct {
	Version    int                         `json:"version"`
	Candidates []ForegroundMemoryCandidate `json:"candidates"`
}

// ForegroundMemoryCandidate retains only the inputs needed to run formation policy again.
type ForegroundMemoryCandidate struct {
	Statement            string            `json:"statement"`
	Evidence             string            `json:"evidence"`
	Category             policy.Category   `json:"category"`
	ClaimSlot            string            `json:"claim_slot"`
	ClaimValue           string            `json:"claim_value"`
	EvidenceType         string            `json:"evidence_type"`
	Provenance           policy.Provenance `json:"provenance"`
	Confidence           float64           `json:"confidence"`
	TargetMemoryID       int64             `json:"target_memory_id,omitempty"`
	SupersedesStatement  string            `json:"supersedes_statement,omitempty"`
	Retention            string            `json:"retention,omitempty"`
	Intent               string            `json:"intent,omitempty"`
	Context              string            `json:"context,omitempty"`
	TTLDays              int               `json:"ttl_days,omitempty"`
	Cardinality          string            `json:"cardinality,omitempty"`
	SourceObservationIDs []int64           `json:"source_observation_ids,omitempty"`
	ExpectedRevision     int64             `json:"expected_revision,omitempty"`
	legacyV2             bool
}

// Evaluate reruns current formation policy using the trusted foreground-save
// constants rather than any policy decision from durable JSON.
func (c ForegroundMemoryCandidate) Evaluate(sourceUserText string) (policy.CandidateOutput, error) {
	in := policy.CandidateInput{
		SourceUserText: sourceUserText, Statement: c.Statement, Evidence: c.Evidence,
		Provenance: c.Provenance, ClaimedAuthority: AuthorityForForegroundProvenance(c.Provenance),
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave, Scope: policy.ScopeLongTerm,
		Category: c.Category, Context: policy.ContextDirectAssertion, Confidence: c.Confidence, Importance: 3,
		ClaimSlot: c.ClaimSlot, ClaimValue: c.ClaimValue,
	}
	if c.legacyV2 {
		return policy.Evaluate(in)
	}
	if err := ValidateForegroundMemoryCandidate(&c, true); err != nil {
		return policy.CandidateOutput{}, err
	}
	if c.Retention == "observation" {
		in.Scope, in.Context, in.TTL = policy.ScopeShortTerm, policy.ContextTemporaryState, time.Duration(c.TTLDays)*24*time.Hour
	}
	out, err := policy.EvaluateAssessment(in)
	if err == nil && out.Approval != policy.ApprovalRejected && (c.Retention == "observation" || c.Intent == "retire") {
		out.Approval, out.Decision = policy.ApprovalProposed, policy.DecisionProposed
		out.Reason = "assessment awaits delivery and durable reconciliation"
	}
	return out, err
}

// ValidateForegroundMemoryCandidate validates, defaults and normalizes a v3/assessment-v1
// candidate in place. Background callers may allow up to five distinct positive
// observation references; foreground callers must pass false. Source ownership,
// source membership, target revision and semantic support remain caller checks.
func ValidateForegroundMemoryCandidate(c *ForegroundMemoryCandidate, allowObservationRefs bool) error {
	if c == nil {
		return fmt.Errorf("missing memory candidate")
	}
	if !validForegroundString(c.Statement, 1000, false) || !validForegroundString(c.Evidence, 1000, false) || !validForegroundString(c.ClaimSlot, 128, false) || !validForegroundString(c.ClaimValue, 256, false) || !validForegroundString(c.SupersedesStatement, 1000, true) || !validForegroundString(c.Context, 500, true) || c.TargetMemoryID < 0 || c.ExpectedRevision < 0 || math.IsNaN(c.Confidence) || math.IsInf(c.Confidence, 0) || c.Confidence < 0 || c.Confidence > 1 {
		return fmt.Errorf("invalid memory candidate fields")
	}
	for _, value := range []string{c.Statement, c.Evidence, c.ClaimSlot, c.ClaimValue, c.Context, c.SupersedesStatement} {
		for _, r := range value {
			if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' || r == '\u061c' || r == '\u200e' || r == '\u200f' || r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069' {
				return fmt.Errorf("invalid memory candidate control character")
			}
		}
	}
	if (c.EvidenceType != "direct_statement" || c.Provenance != policy.ProvenanceUserStatement) && (c.EvidenceType != "model_inference" || c.Provenance != policy.ProvenanceModelInference) {
		return fmt.Errorf("invalid memory evidence assessment")
	}
	if c.Retention == "" {
		c.Retention = "durable"
	}
	if c.Intent == "" {
		c.Intent = "automatic"
	}
	if c.Cardinality == "" {
		c.Cardinality = "single"
		if strings.HasSuffix(c.ClaimSlot, ".fact") {
			c.Cardinality = "multiple"
		}
	}
	if c.Retention != "durable" && c.Retention != "observation" || c.Cardinality != "single" && c.Cardinality != "multiple" {
		return fmt.Errorf("invalid memory retention or cardinality")
	}
	switch c.Intent {
	case "automatic", "remember":
	case "correction", "retire":
		if c.EvidenceType != "direct_statement" || c.TargetMemoryID <= 0 || c.ExpectedRevision <= 0 {
			return fmt.Errorf("correction or retirement requires direct evidence, target memory ID and expected revision")
		}
		if c.Intent == "correction" && c.Retention != "durable" {
			return fmt.Errorf("canonical correction requires durable retention")
		}
	default:
		return fmt.Errorf("invalid memory intent")
	}
	if c.ExpectedRevision > 0 && c.TargetMemoryID == 0 {
		return fmt.Errorf("expected revision requires target memory ID")
	}
	if c.Retention == "observation" && c.TTLDays == 0 {
		c.TTLDays = 7
	}
	if c.TTLDays < 0 || c.TTLDays > 30 || c.Retention == "durable" && c.TTLDays != 0 {
		return fmt.Errorf("invalid memory TTL")
	}
	if len(c.SourceObservationIDs) > 5 || !allowObservationRefs && len(c.SourceObservationIDs) != 0 {
		return fmt.Errorf("invalid source observation references")
	}
	seen := make(map[int64]bool, len(c.SourceObservationIDs))
	for _, id := range c.SourceObservationIDs {
		if id <= 0 || seen[id] {
			return fmt.Errorf("invalid source observation reference")
		}
		seen[id] = true
	}
	slot, value := policy.NormalizeAssessmentClaimIdentity(c.Category, c.ClaimSlot, c.ClaimValue, c.Statement)
	if !policy.AssessmentClaimSlotCompatible(c.Category, slot) || !validForegroundString(slot, 128, false) || !validForegroundString(value, 256, false) {
		return fmt.Errorf("invalid category-compatible claim identity")
	}
	c.ClaimSlot, c.ClaimValue = slot, value
	return nil
}

// AuthorityForForegroundProvenance derives the authority of a foreground observation.
func AuthorityForForegroundProvenance(provenance policy.Provenance) policy.SourceAuthority {
	if provenance == policy.ProvenanceModelInference {
		return policy.AuthorityModel
	}
	return policy.AuthorityUserDirect
}

// EmptyForegroundMemory returns the canonical empty artifact.
func EmptyForegroundMemory() ForegroundMemoryArtifact {
	return ForegroundMemoryArtifact{Version: ForegroundMemoryVersion, Candidates: []ForegroundMemoryCandidate{}}
}

// EncodeForegroundMemory validates and encodes request-local candidates without
// persisting their earlier policy decision.
func EncodeForegroundMemory(userID string, staged []requestctx.StagedMemoryCandidate) (string, error) {
	artifact := EmptyForegroundMemory()
	if len(staged) > requestctx.MaxStagedMemoryCandidates {
		return "", fmt.Errorf("foreground memory contains more than %d candidates", requestctx.MaxStagedMemoryCandidates)
	}
	for _, item := range staged {
		if strings.TrimSpace(item.CanonicalUserID) != strings.TrimSpace(userID) || item.TargetMemoryID < 0 || (item.Candidate.Approval != policy.ApprovalApproved && item.Candidate.Approval != policy.ApprovalProposed) {
			return "", fmt.Errorf("foreground memory candidate is not valid for this user")
		}
		provenance := item.Candidate.Provenance
		if provenance == "" {
			provenance = policy.ProvenanceUserStatement
		}
		evidenceType := "direct_statement"
		if provenance == policy.ProvenanceModelInference {
			evidenceType = "model_inference"
		} else if provenance != policy.ProvenanceUserStatement {
			return "", fmt.Errorf("foreground memory candidate has invalid provenance")
		}
		artifact.Candidates = append(artifact.Candidates, ForegroundMemoryCandidate{
			Statement: item.Candidate.Statement, Evidence: item.Candidate.Evidence,
			Category: item.Candidate.Category, ClaimSlot: item.Candidate.ClaimSlot,
			ClaimValue: item.Candidate.ClaimValue, EvidenceType: evidenceType,
			Provenance: provenance, Confidence: item.Candidate.Confidence,
			TargetMemoryID:      item.TargetMemoryID,
			SupersedesStatement: strings.TrimSpace(item.SupersedesStatement),
			Retention:           item.Retention, Intent: item.Intent, Context: item.Context, TTLDays: item.TTLDays,
			Cardinality: item.Cardinality, SourceObservationIDs: append([]int64(nil), item.SourceObservationIDs...), ExpectedRevision: item.ExpectedRevision,
		})
		if err := ValidateForegroundMemoryCandidate(&artifact.Candidates[len(artifact.Candidates)-1], false); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("encode foreground memory: %w", err)
	}
	if len(encoded) > MaxForegroundMemoryBytes {
		return "", fmt.Errorf("foreground memory exceeds %d bytes", MaxForegroundMemoryBytes)
	}
	return string(encoded), nil
}

// DecodeForegroundMemory decodes a bounded artifact without treating its fields
// as a formation-policy result.
func DecodeForegroundMemory(encoded string) (ForegroundMemoryArtifact, error) {
	if len(encoded) > MaxForegroundMemoryBytes {
		return ForegroundMemoryArtifact{}, fmt.Errorf("foreground memory exceeds %d bytes", MaxForegroundMemoryBytes)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	var artifact ForegroundMemoryArtifact
	if err := decoder.Decode(&artifact); err != nil {
		return ForegroundMemoryArtifact{}, fmt.Errorf("decode foreground memory: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ForegroundMemoryArtifact{}, fmt.Errorf("decode foreground memory: trailing JSON")
	}
	if (artifact.Version != 2 && artifact.Version != ForegroundMemoryVersion) || artifact.Candidates == nil || len(artifact.Candidates) > requestctx.MaxStagedMemoryCandidates {
		return ForegroundMemoryArtifact{}, fmt.Errorf("invalid foreground memory artifact")
	}
	if artifact.Version == 2 {
		// Presence, even null or a zero value, was an unknown-field error in v2.
		var raw struct {
			Candidates []map[string]json.RawMessage `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(encoded), &raw); err != nil {
			return ForegroundMemoryArtifact{}, err
		}
		for _, fields := range raw.Candidates {
			for _, key := range []string{"retention", "intent", "context", "ttl_days", "cardinality", "source_observation_ids", "expected_revision"} {
				if _, present := fields[key]; present {
					return ForegroundMemoryArtifact{}, fmt.Errorf("v2 candidate contains v3 fields")
				}
			}
		}
	}
	for i := range artifact.Candidates {
		candidate := &artifact.Candidates[i]
		if artifact.Version == 2 {
			candidate.legacyV2 = true
		} else if err := ValidateForegroundMemoryCandidate(candidate, false); err != nil {
			return ForegroundMemoryArtifact{}, err
		}
		if !validForegroundString(candidate.Statement, 1000, false) || !validForegroundString(candidate.Evidence, 1000, false) || !validForegroundString(string(candidate.Category), 32, false) || !validForegroundString(candidate.ClaimSlot, 128, false) || !validForegroundString(candidate.ClaimValue, 256, false) || !validForegroundString(candidate.SupersedesStatement, 1000, true) || candidate.TargetMemoryID < 0 || math.IsNaN(candidate.Confidence) || math.IsInf(candidate.Confidence, 0) || candidate.Confidence < 0 || candidate.Confidence > 1 {
			return ForegroundMemoryArtifact{}, fmt.Errorf("invalid foreground memory candidate")
		}
		if (candidate.EvidenceType == "direct_statement" && candidate.Provenance != policy.ProvenanceUserStatement) || (candidate.EvidenceType == "model_inference" && candidate.Provenance != policy.ProvenanceModelInference) || (candidate.EvidenceType != "direct_statement" && candidate.EvidenceType != "model_inference") {
			return ForegroundMemoryArtifact{}, fmt.Errorf("invalid foreground memory assessment")
		}
		output, err := candidate.Evaluate(candidate.Evidence)
		if err != nil {
			return ForegroundMemoryArtifact{}, fmt.Errorf("invalid foreground memory candidate: %w", err)
		}
		if output.Approval == policy.ApprovalRejected {
			return ForegroundMemoryArtifact{}, fmt.Errorf("rejected foreground memory candidate")
		}
	}
	return artifact, nil
}

func validForegroundString(value string, maxRunes int, allowEmpty bool) bool {
	trimmed := strings.TrimSpace(value)
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxRunes && (allowEmpty || trimmed != "")
}

// SessionTurnForegroundMemory loads one tenant-fenced durable artifact.
func (s *Store) SessionTurnForegroundMemory(ctx context.Context, userID string, turnID int64) (ForegroundMemoryArtifact, error) {
	var encoded string
	if err := s.sql.QueryRowContext(ctx, `SELECT foreground_memory FROM session_turns WHERE id = ? AND canonical_user_id = ?`, turnID, strings.TrimSpace(userID)).Scan(&encoded); err != nil {
		return ForegroundMemoryArtifact{}, fmt.Errorf("read session turn foreground memory: %w", err)
	}
	return DecodeForegroundMemory(encoded)
}
