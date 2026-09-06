package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const (
	// ForegroundMemoryVersion is the durable foreground-memory artifact format.
	ForegroundMemoryVersion = 2
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
	Statement           string            `json:"statement"`
	Evidence            string            `json:"evidence"`
	Category            policy.Category   `json:"category"`
	ClaimSlot           string            `json:"claim_slot"`
	ClaimValue          string            `json:"claim_value"`
	EvidenceType        string            `json:"evidence_type"`
	Provenance          policy.Provenance `json:"provenance"`
	Confidence          float64           `json:"confidence"`
	TargetMemoryID      int64             `json:"target_memory_id,omitempty"`
	SupersedesStatement string            `json:"supersedes_statement,omitempty"`
}

// Evaluate reruns current formation policy using the trusted foreground-save
// constants rather than any policy decision from durable JSON.
func (c ForegroundMemoryCandidate) Evaluate(sourceUserText string) (policy.CandidateOutput, error) {
	return policy.Evaluate(policy.CandidateInput{
		SourceUserText: sourceUserText, Statement: c.Statement, Evidence: c.Evidence,
		Provenance: c.Provenance, ClaimedAuthority: AuthorityForForegroundProvenance(c.Provenance),
		Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave, Scope: policy.ScopeLongTerm,
		Category: c.Category, Context: policy.ContextDirectAssertion, Confidence: c.Confidence, Importance: 3,
		ClaimSlot: c.ClaimSlot, ClaimValue: c.ClaimValue,
	})
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
		})
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
	if artifact.Version != ForegroundMemoryVersion || artifact.Candidates == nil || len(artifact.Candidates) > requestctx.MaxStagedMemoryCandidates {
		return ForegroundMemoryArtifact{}, fmt.Errorf("invalid foreground memory artifact")
	}
	for _, candidate := range artifact.Candidates {
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
