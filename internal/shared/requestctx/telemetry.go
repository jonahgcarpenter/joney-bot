package requestctx

import (
	"context"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// LogFields returns only canonical/server-generated correlation, never session
// identifiers, external identities, or current user text. Empty values are omitted.
func LogFields(ctx context.Context) []config.Field {
	if ctx == nil {
		return nil
	}
	m := MetadataFromContext(ctx)
	p, _ := PrincipalFromContext(ctx)
	fields := make([]config.Field, 0, 9)
	for _, f := range []config.Field{
		config.F("request_id", m.RequestID), config.F("user_id", p.CanonicalUserID),
		config.F("gateway", p.Gateway), config.F("model", m.Model), config.F("workload", m.Workload),
		config.F("operation_id", m.OperationID), config.F("parent_operation_id", m.ParentOperationID),
	} {
		if f.Value != "" {
			fields = append(fields, f)
		}
	}
	if m.JobID > 0 {
		fields = append(fields, config.F("job_id", m.JobID))
	}
	return fields
}

// ModelUsage describes one completed provider call. Operation="embedding"
// selects the separate embedding meter; all other operations use the chat meter.
// Submitted means a provider submission was attempted. UsageReported distinguishes
// actual zero usage from missing telemetry. DurationMS should use time.Since.
// Valid statuses are ok, error, rejected, retry, degraded. Canceled calls use ok.
type ModelUsage struct {
	Operation                                   string
	Submitted                                   bool
	Status                                      string
	UsageReported                               bool
	UsageComplete                               bool
	PromptTokens, CompletionTokens, TotalTokens int
	DurationMS                                  int64
}

// UsageSnapshot is a value copy of request-local counters. Tokens are summed only
// for UsageReported calls; negative tokens/durations are clamped to zero. Failure
// counts exclude successful and degraded responses; invalid/missing statuses
// count as failures. Complete usage counts distinguish partial provider reports.
type UsageSnapshot struct {
	ModelCallCount, ModelSubmissionCount, ModelFailureCount, UsageReportedCallCount                      int
	PromptTokens, CompletionTokens, TotalTokens                                                          int
	ModelDurationMS                                                                                      int64
	UsageCompleteCallCount                                                                               int
	EmbeddingCallCount, EmbeddingSubmissionCount, EmbeddingFailureCount, EmbeddingUsageReportedCallCount int
	EmbeddingPromptTokens, EmbeddingCompletionTokens, EmbeddingTotalTokens                               int
	EmbeddingDurationMS                                                                                  int64
	EmbeddingUsageCompleteCallCount                                                                      int
}

// UsageCollector is a concurrent, request-local meter. Its zero value is ready
// for use; it never logs and must not be copied after first use.
type UsageCollector struct {
	mu        sync.Mutex
	snapshot  UsageSnapshot
	execution ExecutionSnapshot
}

// ExecutionSnapshot is agent execution state, independent of response delivery.
// IsComplete certifies processor return, not provider usage completeness.
type ExecutionSnapshot struct {
	ToolExecutionCount, BlockedCount       int
	PersistenceStatus, ResponseKind, Model string
	IsComplete                             bool
}

// SetExecution publishes a value copy without changing provider meters.
func (c *UsageCollector) SetExecution(execution ExecutionSnapshot) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execution = execution
}

// ExecutionSnapshot returns the latest execution state; a nil collector is empty.
func (c *UsageCollector) ExecutionSnapshot() ExecutionSnapshot {
	if c == nil {
		return ExecutionSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.execution
}

// NewUsageCollector creates an empty meter.
func NewUsageCollector() *UsageCollector { return &UsageCollector{} }

type usageCollectorKey struct{}

// WithUsageCollector propagates the same meter across request execution contexts.
// A nil ctx uses Background; a nil collector masks any inherited collector.
func WithUsageCollector(ctx context.Context, collector *UsageCollector) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, usageCollectorKey{}, collector)
}

// UsageCollectorFromContext returns nil if no meter is attached or ctx is nil.
func UsageCollectorFromContext(ctx context.Context) *UsageCollector {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(usageCollectorKey{}).(*UsageCollector)
	return c
}

// Record adds one call, not one stream chunk. A nil receiver is a no-op.
func (c *UsageCollector) Record(u ModelUsage) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.snapshot
	calls, submissions, failures, reported := &s.ModelCallCount, &s.ModelSubmissionCount, &s.ModelFailureCount, &s.UsageReportedCallCount
	complete := &s.UsageCompleteCallCount
	prompt, completion, total, duration := &s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.ModelDurationMS
	if u.Operation == "embedding" {
		calls, submissions, failures, reported = &s.EmbeddingCallCount, &s.EmbeddingSubmissionCount, &s.EmbeddingFailureCount, &s.EmbeddingUsageReportedCallCount
		complete = &s.EmbeddingUsageCompleteCallCount
		prompt, completion, total, duration = &s.EmbeddingPromptTokens, &s.EmbeddingCompletionTokens, &s.EmbeddingTotalTokens, &s.EmbeddingDurationMS
	}
	*calls++
	if u.Submitted {
		*submissions++
	}
	if u.Status != "ok" && u.Status != "degraded" {
		*failures++
	}
	*duration += max(u.DurationMS, 0)
	if u.UsageReported {
		*reported++
		if u.UsageComplete {
			*complete++
		}
		*prompt += max(u.PromptTokens, 0)
		*completion += max(u.CompletionTokens, 0)
		*total += max(u.TotalTokens, 0)
	}
}

// Snapshot returns a consistent value copy, or zero counters for a nil receiver.
func (c *UsageCollector) Snapshot() UsageSnapshot {
	if c == nil {
		return UsageSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot
}
