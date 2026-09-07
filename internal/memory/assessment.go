package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// AssessmentExtractorVersion pins the independent observation assessment contract.
const AssessmentExtractorVersion = "assessment-v1"

// AssessmentMemory exposes canonical claim state with optimistic concurrency metadata.
type AssessmentMemory struct {
	Context          string
	RetiredAt        time.Time
	RetirementReason string
	ID               int64
	Statement        string
	Category         string
	ClaimSlot        string
	ClaimValue       string
	Provenance       string
	Confidence       float64
	Revision         int64
}

// AssessmentInput is a frozen, tenant-owned assessment view. Context is reference only.
type AssessmentInput struct {
	Version      int
	Anchor       StoredSessionTurn
	Context      []StoredSessionTurn
	Observations []MemoryObservation
	Memories     []AssessmentMemory
	Suppressions []MemorySuppression
	// Staged assessments are deduplication hints, not independent evidence.
	Staged []ForegroundMemoryCandidate
}

// AssessmentBatch contains untrusted proposals, never preapproved decisions.
type AssessmentBatch struct {
	Version int                         `json:"version"`
	Items   []ForegroundMemoryCandidate `json:"items"`
}

// AssessmentResult contains committed counts only; replay returns zero counts.
type AssessmentResult struct {
	ObservationCount int
	PublishedCount   int
	ReinforcedCount  int
	RetiredCount     int
	RejectedCount    int
}

// ValidateAssessmentBatch validates bounded structure; source trust is checked by storage.
func ValidateAssessmentBatch(batch AssessmentBatch) error {
	if batch.Version != 1 || batch.Items == nil || len(batch.Items) > 5 {
		return fmt.Errorf("invalid assessment batch")
	}
	for _, c := range batch.Items {
		if err := ValidateForegroundMemoryCandidate(&c, true); err != nil {
			return err
		}
		if (c.Intent == "retire" || c.Intent == "correction") && (c.TargetMemoryID <= 0 || c.ExpectedRevision <= 0) {
			return fmt.Errorf("assessment mutation requires target revision")
		}
		if c.Intent == "correction" && c.Retention != "durable" {
			return fmt.Errorf("canonical correction requires durable retention")
		}
	}
	return nil
}

// MarshalAssessmentBatchArtifact encodes a validated durable result.
func MarshalAssessmentBatchArtifact(batch AssessmentBatch) ([]byte, error) {
	if batch.Items == nil {
		return nil, fmt.Errorf("invalid assessment batch")
	}
	batch.Items = append([]ForegroundMemoryCandidate{}, batch.Items...)
	for i := range batch.Items {
		if err := ValidateForegroundMemoryCandidate(&batch.Items[i], true); err != nil {
			return nil, err
		}
	}
	if err := ValidateAssessmentBatch(batch); err != nil {
		return nil, err
	}
	data, err := json.Marshal(batch)
	if len(data) > MaxForegroundMemoryBytes {
		return nil, fmt.Errorf("assessment exceeds byte bound")
	}
	return data, err
}

// DecodeAssessmentBatchJSON strictly decodes a version-pinned result.
func DecodeAssessmentBatchJSON(data []byte) (AssessmentBatch, error) {
	var batch AssessmentBatch
	if len(data) > MaxForegroundMemoryBytes {
		return batch, fmt.Errorf("assessment exceeds byte bound")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&batch); err != nil {
		return batch, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return batch, fmt.Errorf("trailing assessment JSON")
	}
	for i := range batch.Items {
		if err := ValidateForegroundMemoryCandidate(&batch.Items[i], true); err != nil {
			return batch, err
		}
	}
	return batch, ValidateAssessmentBatch(batch)
}

// DecodeAssessmentBatch decodes model tool arguments with the same durable validation.
func DecodeAssessmentBatch(arguments map[string]interface{}) (AssessmentBatch, error) {
	data, err := json.Marshal(arguments)
	if err != nil {
		return AssessmentBatch{}, err
	}
	return DecodeAssessmentBatchJSON(data)
}
