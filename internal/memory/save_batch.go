package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

const maxExtractedMemoryBatch = 5

var memorySaveRequiredFields = []string{
	"statement", "evidence", "scope", "category", "context", "provenance", "sensitivity",
	"confidence", "importance", "ttl_days", "supersedes", "claim_slot", "claim_value",
}

// MemorySaveRequiredFields returns the complete private extraction item contract.
func MemorySaveRequiredFields() []string {
	return append([]string(nil), memorySaveRequiredFields...)
}

// MemorySaveItem is the untrusted candidate shape used by background extraction.
type MemorySaveItem struct {
	InputIndex  int     `json:"-"`
	Statement   string  `json:"statement"`
	Evidence    string  `json:"evidence"`
	Scope       string  `json:"scope"`
	Category    string  `json:"category"`
	Context     string  `json:"context"`
	Provenance  string  `json:"provenance"`
	Sensitivity string  `json:"sensitivity"`
	Confidence  float64 `json:"confidence"`
	Importance  int     `json:"importance"`
	TTLDays     int     `json:"ttl_days"`
	Supersedes  string  `json:"supersedes"`
	ClaimSlot   string  `json:"claim_slot"`
	ClaimValue  string  `json:"claim_value"`
}

// MemorySaveBatch is the input contract for background memory formation.
type MemorySaveBatch struct {
	Memories       []MemorySaveItem `json:"memories"`
	SubmittedCount int              `json:"-"`
	MalformedCount int              `json:"-"`
}

type memorySaveBatchArtifact struct {
	Memories       []MemorySaveItem `json:"memories"`
	SubmittedCount int              `json:"submitted_count"`
	MalformedCount int              `json:"malformed_count"`
}

// MemorySaveOutcome reports one independently evaluated batch item.
type MemorySaveOutcome struct {
	InputIndex        int
	CandidateID       int64
	State             string
	PublishedMemoryID int64
	Reason            string
	Err               error
	Operational       bool
}

// MemorySaveItemError identifies one malformed item without rejecting valid siblings.
type MemorySaveItemError struct {
	InputIndex int
	Err        error
}

func (e MemorySaveItemError) Error() string {
	return e.Err.Error()
}

// DecodeMemorySaveBatch strictly decodes the outer object while dropping only
// malformed individual items so valid siblings can still be evaluated.
func DecodeMemorySaveBatch(arguments map[string]interface{}) (MemorySaveBatch, []MemorySaveItemError, error) {
	if arguments == nil {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories is required")
	}
	for key := range arguments {
		if key != "memories" {
			return MemorySaveBatch{}, nil, fmt.Errorf("unknown batch field %q", key)
		}
	}
	rawItems, ok := arguments["memories"].([]interface{})
	if !ok {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories must be an array")
	}
	if len(rawItems) > maxExtractedMemoryBatch {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories contains %d items; maximum is %d", len(rawItems), maxExtractedMemoryBatch)
	}
	batch := MemorySaveBatch{Memories: make([]MemorySaveItem, 0, len(rawItems)), SubmittedCount: len(rawItems)}
	itemErrors := make([]MemorySaveItemError, 0)
	for index, raw := range rawItems {
		encoded, err := json.Marshal(raw)
		if err != nil {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("encode item: %w", err)})
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("item must be an object")})
			continue
		}
		missing := ""
		for _, field := range memorySaveRequiredFields {
			value, ok := fields[field]
			if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				missing = field
				break
			}
		}
		if missing != "" {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("%s is required", missing)})
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		var item MemorySaveItem
		if err := decoder.Decode(&item); err != nil {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: err})
			continue
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("trailing JSON")})
			continue
		}
		if strings.TrimSpace(item.Statement) == "" || strings.TrimSpace(item.Evidence) == "" || strings.TrimSpace(item.Scope) == "" || strings.TrimSpace(item.Category) == "" || strings.TrimSpace(item.Context) == "" || strings.TrimSpace(item.Provenance) == "" || strings.TrimSpace(item.Sensitivity) == "" || strings.TrimSpace(item.ClaimSlot) == "" || strings.TrimSpace(item.ClaimValue) == "" {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("required string fields except supersedes must be non-empty")})
			continue
		}
		if item.Confidence < 0 || item.Confidence > 1 || item.Importance < 1 || item.Importance > 5 || item.TTLDays < 0 || item.TTLDays > 30 {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("confidence, importance, or ttl_days is outside the schema range")})
			continue
		}
		if !validMemorySaveEnums(item) {
			itemErrors = append(itemErrors, MemorySaveItemError{InputIndex: index, Err: fmt.Errorf("scope, category, context, provenance, or sensitivity is outside the schema enum")})
			continue
		}
		item.InputIndex = index
		batch.Memories = append(batch.Memories, item)
	}
	batch.MalformedCount = len(itemErrors)
	return batch, itemErrors, nil
}

func validMemorySaveEnums(item MemorySaveItem) bool {
	validScope := item.Scope == string(policy.ScopeShortTerm) || item.Scope == string(policy.ScopeLongTerm)
	validCategory := item.Category == string(policy.CategoryIdentity) ||
		item.Category == string(policy.CategoryCommunicationPreferences) ||
		item.Category == string(policy.CategoryDurablePreferences) ||
		item.Category == string(policy.CategoryProjects) ||
		item.Category == string(policy.CategoryRelationships) ||
		item.Category == string(policy.CategoryEnvironment) ||
		item.Category == string(policy.CategoryNotes)
	validContext := item.Context == string(policy.ContextDirectAssertion) ||
		item.Context == string(policy.ContextTemporaryState) ||
		item.Context == string(policy.ContextHypothetical) ||
		item.Context == string(policy.ContextQuotation)
	validProvenance := item.Provenance == string(policy.ProvenanceUserStatement) ||
		item.Provenance == string(policy.ProvenanceModelInference) ||
		item.Provenance == string(policy.ProvenanceThirdParty) ||
		item.Provenance == string(policy.ProvenancePublicSource) ||
		item.Provenance == string(policy.ProvenanceToolOutput)
	validSensitivity := item.Sensitivity == string(policy.SensitivityLow) ||
		item.Sensitivity == string(policy.SensitivityIdentityOrContact) ||
		item.Sensitivity == string(policy.SensitivityHighImpactInteraction)
	return validScope && validCategory && validContext && validProvenance && validSensitivity
}

// MarshalMemorySaveBatchArtifact encodes one validated batch for durable replay.
func MarshalMemorySaveBatchArtifact(batch MemorySaveBatch) ([]byte, error) {
	if batch.SubmittedCount < 0 || batch.SubmittedCount > maxExtractedMemoryBatch || batch.MalformedCount < 0 || batch.MalformedCount > batch.SubmittedCount || len(batch.Memories)+batch.MalformedCount != batch.SubmittedCount {
		return nil, fmt.Errorf("memory save artifact counts are inconsistent")
	}
	memories := batch.Memories
	if memories == nil {
		memories = []MemorySaveItem{}
	}
	return json.Marshal(memorySaveBatchArtifact{
		Memories:       memories,
		SubmittedCount: batch.SubmittedCount,
		MalformedCount: batch.MalformedCount,
	})
}

// DecodeMemorySaveBatchJSON strictly decodes a legacy or current persisted extraction artifact.
func DecodeMemorySaveBatchJSON(data []byte) (MemorySaveBatch, []MemorySaveItemError, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var arguments map[string]interface{}
	if err := decoder.Decode(&arguments); err != nil {
		return MemorySaveBatch{}, nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return MemorySaveBatch{}, nil, fmt.Errorf("trailing JSON")
	}
	if memories, exists := arguments["memories"]; exists && memories == nil {
		arguments["memories"] = []interface{}{}
	}
	_, hasSubmittedCount := arguments["submitted_count"]
	_, hasMalformedCount := arguments["malformed_count"]
	if hasSubmittedCount != hasMalformedCount {
		return MemorySaveBatch{}, nil, fmt.Errorf("persisted artifact counts must be provided together")
	}
	if !hasSubmittedCount {
		return DecodeMemorySaveBatch(arguments)
	}
	for key := range arguments {
		if key != "memories" && key != "submitted_count" && key != "malformed_count" {
			return MemorySaveBatch{}, nil, fmt.Errorf("unknown persisted artifact field %q", key)
		}
	}
	submittedCount, err := artifactInteger(arguments["submitted_count"])
	if err != nil {
		return MemorySaveBatch{}, nil, fmt.Errorf("submitted_count: %w", err)
	}
	malformedCount, err := artifactInteger(arguments["malformed_count"])
	if err != nil {
		return MemorySaveBatch{}, nil, fmt.Errorf("malformed_count: %w", err)
	}
	rawItems, ok := arguments["memories"].([]interface{})
	if !ok {
		return MemorySaveBatch{}, nil, fmt.Errorf("memories must be an array")
	}
	if submittedCount < 0 || submittedCount > maxExtractedMemoryBatch || malformedCount < 0 || malformedCount > submittedCount || len(rawItems)+malformedCount != submittedCount {
		return MemorySaveBatch{}, nil, fmt.Errorf("persisted artifact counts are inconsistent")
	}
	delete(arguments, "submitted_count")
	delete(arguments, "malformed_count")
	batch, itemErrors, err := DecodeMemorySaveBatch(arguments)
	if err != nil {
		return MemorySaveBatch{}, nil, err
	}
	batch.SubmittedCount = submittedCount
	batch.MalformedCount = malformedCount + len(itemErrors)
	return batch, itemErrors, nil
}

func artifactInteger(value interface{}) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("must be an integer")
	}
	integer, err := number.Int64()
	if err != nil || int64(int(integer)) != integer {
		return 0, fmt.Errorf("must be an integer")
	}
	return int(integer), nil
}

// SubmitMemorySaveBatch evaluates and atomically applies each item independently.
func (s *Store) SubmitMemorySaveBatch(ctx context.Context, userID, sourceText string, source FormationSource, batch MemorySaveBatch, formationJob *FormationJob) []MemorySaveOutcome {
	outcomes := make([]MemorySaveOutcome, 0, len(batch.Memories))
	remembered, hasExplicitIntent := policy.ParseExplicitRemember(sourceText)
	for _, item := range batch.Memories {
		mode := policy.ModeAutomaticExtraction
		if hasExplicitIntent && strings.Contains(normalizedMemoryText(remembered), normalizedMemoryText(item.Evidence)) {
			mode = policy.ModeExplicitRemember
		}
		output, err := policy.Evaluate(policy.CandidateInput{
			SourceUserText:   sourceText,
			Statement:        item.Statement,
			Evidence:         item.Evidence,
			Provenance:       policy.Provenance(item.Provenance),
			ClaimedAuthority: claimedAuthority(item.Provenance),
			Sensitivity:      policy.Sensitivity(item.Sensitivity),
			Mode:             mode,
			Scope:            policy.Scope(item.Scope),
			Category:         policy.Category(item.Category),
			Context:          policy.ContentContext(item.Context),
			Confidence:       item.Confidence,
			Importance:       item.Importance,
			TTL:              durationFromDays(item.TTLDays),
			ClaimSlot:        item.ClaimSlot,
			ClaimValue:       item.ClaimValue,
		})
		if err != nil {
			outcomes = append(outcomes, MemorySaveOutcome{InputIndex: item.InputIndex, Err: err})
			continue
		}
		candidate, _, err := s.ProposeCandidate(ctx, userID, CandidateProposal{
			Output: output, Source: source, SupersedesStatement: item.Supersedes, FormationJob: formationJob,
		})
		if err != nil {
			outcomes = append(outcomes, MemorySaveOutcome{InputIndex: item.InputIndex, Err: err, Operational: true})
			continue
		}
		reason := candidate.DecisionReason
		outcomes = append(outcomes, MemorySaveOutcome{InputIndex: item.InputIndex, CandidateID: candidate.ID, State: candidate.State, PublishedMemoryID: candidate.PublishedMemoryID, Reason: reason})
	}
	return outcomes
}

func claimedAuthority(provenance string) policy.SourceAuthority {
	switch policy.Provenance(provenance) {
	case policy.ProvenanceUserStatement:
		return policy.AuthorityUserDirect
	case policy.ProvenanceThirdParty:
		return policy.AuthorityThirdParty
	case policy.ProvenancePublicSource:
		return policy.AuthorityPublic
	case policy.ProvenanceToolOutput:
		return policy.AuthorityTool
	default:
		return policy.AuthorityModel
	}
}

func durationFromDays(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

func normalizedMemoryText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
