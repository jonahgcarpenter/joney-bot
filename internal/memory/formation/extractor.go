package formation

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/extraction"
)

var errPermanentExtraction = extraction.ErrPermanentExtraction

var errInvalidOutput = extraction.ErrInvalidOutput

var invalidOutputCode = extraction.InvalidOutputCode

// Extractor proposes a shared memory-save batch from one completed turn.
type Extractor interface {
	Extract(context.Context, memory.StoredSessionTurn, string) (memory.MemorySaveBatch, error)
}

// PatternExtractor proposes repeated signals from a frozen user-turn window.
type PatternExtractor interface {
	ExtractPatterns(context.Context, []memory.StoredSessionTurn, string) (memory.MemoryPatternBatch, error)
}

// AssessmentExtractor assesses one delivered anchor against its frozen evidence.
type AssessmentExtractor interface {
	ExtractAssessment(context.Context, memory.AssessmentInput, string) (memory.AssessmentBatch, error)
}
