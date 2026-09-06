package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

const (
	PatternExtractorVersion = "pattern-v1"
	MaxPatternContextTurns  = 8
	MaxExtractedPatterns    = 3
)

// PatternContext is the immutable set of source turns available to one pattern job.
type PatternContext struct {
	Version int                 `json:"version"`
	Turns   []StoredSessionTurn `json:"-"`
	TurnIDs []int64             `json:"turn_ids"`
}

// PatternObservation identifies one exact whole-turn observation.
type PatternObservation struct {
	SourceTurnID int64  `json:"source_turn_id"`
	Evidence     string `json:"evidence"`
}

// MemoryPattern is one repeated implicit signal proposed by the private extractor.
type MemoryPattern struct {
	Statement    string               `json:"statement"`
	Category     string               `json:"category"`
	ClaimSlot    string               `json:"claim_slot"`
	ClaimValue   string               `json:"claim_value"`
	Sensitivity  string               `json:"sensitivity"`
	Confidence   float64              `json:"confidence"`
	Observations []PatternObservation `json:"observations"`
}

// MemoryPatternBatch is the strict private pattern extraction result.
type MemoryPatternBatch struct {
	Patterns []MemoryPattern `json:"patterns"`
}

// MarshalPatternContext stores only ordered source IDs on the durable job.
func MarshalPatternContext(turnIDs []int64) ([]byte, error) {
	if len(turnIDs) < 2 || len(turnIDs) > MaxPatternContextTurns {
		return nil, fmt.Errorf("pattern context requires two to eight turns")
	}
	seen := make(map[int64]struct{}, len(turnIDs))
	for i, id := range turnIDs {
		if id <= 0 || (i > 0 && id <= turnIDs[i-1]) {
			return nil, fmt.Errorf("pattern context turn IDs must be positive and ordered")
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("pattern context turn IDs must be distinct")
		}
		seen[id] = struct{}{}
	}
	return json.Marshal(PatternContext{Version: 1, TurnIDs: turnIDs})
}

// DecodePatternContext strictly validates a persisted pattern window.
func DecodePatternContext(data []byte) (PatternContext, error) {
	var context PatternContext
	if err := decodeStrictJSON(data, &context); err != nil {
		return PatternContext{}, err
	}
	encoded, err := MarshalPatternContext(context.TurnIDs)
	if err != nil {
		return PatternContext{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(data), encoded) {
		return PatternContext{}, fmt.Errorf("pattern context is not canonical")
	}
	return context, nil
}

// DecodeMemoryPatternBatch strictly validates private extractor output.
func DecodeMemoryPatternBatch(arguments map[string]interface{}) (MemoryPatternBatch, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return MemoryPatternBatch{}, err
	}
	var batch MemoryPatternBatch
	if err := decodeStrictJSON(encoded, &batch); err != nil {
		return MemoryPatternBatch{}, err
	}
	var raw struct {
		Patterns []map[string]json.RawMessage `json:"patterns"`
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return MemoryPatternBatch{}, err
	}
	if batch.Patterns == nil || len(batch.Patterns) > MaxExtractedPatterns {
		return MemoryPatternBatch{}, fmt.Errorf("patterns must contain zero to three items")
	}
	for i, pattern := range batch.Patterns {
		if _, present := raw.Patterns[i]["confidence"]; !present {
			return MemoryPatternBatch{}, fmt.Errorf("patterns[%d] is missing confidence", i)
		}
		if strings.TrimSpace(pattern.Statement) == "" || strings.TrimSpace(pattern.Category) == "" || strings.TrimSpace(pattern.ClaimSlot) == "" || strings.TrimSpace(pattern.ClaimValue) == "" || strings.TrimSpace(pattern.Sensitivity) == "" {
			return MemoryPatternBatch{}, fmt.Errorf("patterns[%d] has empty required fields", i)
		}
		if math.IsNaN(pattern.Confidence) || math.IsInf(pattern.Confidence, 0) || pattern.Confidence < 0 || pattern.Confidence > 1 {
			return MemoryPatternBatch{}, fmt.Errorf("patterns[%d] has invalid confidence", i)
		}
		if len(pattern.Observations) < 2 || len(pattern.Observations) > 5 {
			return MemoryPatternBatch{}, fmt.Errorf("patterns[%d] requires two to five observations", i)
		}
		seen := make(map[int64]struct{}, len(pattern.Observations))
		for _, observation := range pattern.Observations {
			if observation.SourceTurnID <= 0 || strings.TrimSpace(observation.Evidence) == "" {
				return MemoryPatternBatch{}, fmt.Errorf("patterns[%d] has invalid observation", i)
			}
			if _, exists := seen[observation.SourceTurnID]; exists {
				return MemoryPatternBatch{}, fmt.Errorf("patterns[%d] repeats a source turn", i)
			}
			seen[observation.SourceTurnID] = struct{}{}
		}
	}
	return batch, nil
}

// MarshalMemoryPatternBatchArtifact encodes a validated result for stable retry replay.
func MarshalMemoryPatternBatchArtifact(batch MemoryPatternBatch) ([]byte, error) {
	patterns := batch.Patterns
	if patterns == nil {
		patterns = []MemoryPattern{}
	}
	arguments := map[string]interface{}{"patterns": patterns}
	if _, err := DecodeMemoryPatternBatch(arguments); err != nil {
		return nil, err
	}
	return json.Marshal(MemoryPatternBatch{Patterns: patterns})
}

// DecodeMemoryPatternBatchJSON decodes a persisted private pattern result.
func DecodeMemoryPatternBatchJSON(data []byte) (MemoryPatternBatch, error) {
	var arguments map[string]interface{}
	if err := decodeStrictJSON(data, &arguments); err != nil {
		return MemoryPatternBatch{}, err
	}
	return DecodeMemoryPatternBatch(arguments)
}

// EvaluatePatternObservation applies server-owned classification to one observation.
func EvaluatePatternObservation(turn StoredSessionTurn, pattern MemoryPattern, evidence string) (policy.CandidateOutput, error) {
	if evidence != turn.UserText {
		return policy.CandidateOutput{}, fmt.Errorf("pattern evidence must equal the complete source turn")
	}
	importance := 3
	if pattern.Category == string(policy.CategoryDurablePreferences) || pattern.Category == string(policy.CategoryEnvironment) {
		importance = 4
	}
	return policy.Evaluate(policy.CandidateInput{
		SourceUserText: turn.UserText, Statement: pattern.Statement, Evidence: evidence,
		Provenance: policy.ProvenanceModelInference, ClaimedAuthority: policy.AuthorityModel,
		Sensitivity: policy.Sensitivity(pattern.Sensitivity), Mode: policy.ModeBackgroundPattern,
		Scope: policy.ScopeLongTerm, Category: policy.Category(pattern.Category),
		Context: policy.ContextDirectAssertion, Confidence: pattern.Confidence, Importance: importance,
		ClaimSlot: pattern.ClaimSlot, ClaimValue: pattern.ClaimValue,
	})
}

func decodeStrictJSON(data []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
