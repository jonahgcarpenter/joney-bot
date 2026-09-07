package extraction

import "context"

// AssessmentMetrics describes the actual projected request, without private content.
// Text counts are omitted Unicode runes; record counts are whole omitted records.
type AssessmentMetrics struct {
	InputTokens             int
	OmittedTextChars        int
	OmittedContextCount     int
	OmittedObservationCount int
	OmittedMemoryCount      int
	OmittedSuppressionCount int
	OmittedStagedCount      int
}

type assessmentMetricsKey struct{}

// WithAssessmentMetrics reports projection once, synchronously before model invocation.
// Replay does not project a request and does not invoke the callback.
func WithAssessmentMetrics(ctx context.Context, report func(AssessmentMetrics)) context.Context {
	return context.WithValue(ctx, assessmentMetricsKey{}, report)
}
