package requestctx

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestLogFieldsAllowlist(t *testing.T) {
	ctx := WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "usr_1", Gateway: "discord", ExternalID: "private"})
	ctx = WithMetadata(ctx, Metadata{RequestID: "req_1", SessionID: "private", CurrentUserText: "private", Model: "model", Workload: "foreground", OperationID: "op_1", ParentOperationID: "op_0", JobID: 12})
	got := map[string]any{}
	for _, f := range LogFields(ctx) {
		got[f.Key] = f.Value
	}
	want := map[string]any{"request_id": "req_1", "user_id": "usr_1", "gateway": "discord", "model": "model", "workload": "foreground", "operation_id": "op_1", "parent_operation_id": "op_0", "job_id": int64(12)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields: %#v", got)
	}
	if len(LogFields(nil)) != 0 || len(LogFields(context.Background())) != 0 {
		t.Fatal("empty context has fields")
	}
}

func TestUsageCollectorConcurrentAndSeparateMeters(t *testing.T) {
	c := NewUsageCollector()
	ctx := WithUsageCollector(context.Background(), c)
	if UsageCollectorFromContext(ctx) != c {
		t.Fatal("collector not propagated")
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Record(ModelUsage{Operation: "chat", Submitted: true, Status: "ok", UsageReported: true, PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, DurationMS: 3})
			c.Record(ModelUsage{Operation: "embedding", Submitted: true, Status: "error", UsageReported: true, PromptTokens: 4, TotalTokens: 4, DurationMS: 5})
			_ = c.Snapshot()
		}()
	}
	wg.Wait()
	want := UsageSnapshot{ModelCallCount: 100, ModelSubmissionCount: 100, UsageReportedCallCount: 100, PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200, ModelDurationMS: 300, EmbeddingCallCount: 100, EmbeddingSubmissionCount: 100, EmbeddingFailureCount: 100, EmbeddingUsageReportedCallCount: 100, EmbeddingPromptTokens: 400, EmbeddingTotalTokens: 400, EmbeddingDurationMS: 500}
	if got := c.Snapshot(); got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	copy := c.Snapshot()
	copy.PromptTokens = -1
	if c.Snapshot() != want {
		t.Fatal("snapshot mutation changed collector")
	}
}

func TestExecutionTelemetryIsIndependentAndConcurrent(t *testing.T) {
	var absent *UsageCollector
	absent.SetExecution(ExecutionSnapshot{IsComplete: true})
	if absent.ExecutionSnapshot() != (ExecutionSnapshot{}) {
		t.Fatal("nil execution snapshot")
	}
	c := NewUsageCollector()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.SetExecution(ExecutionSnapshot{ToolExecutionCount: 1})
			_ = c.ExecutionSnapshot()
			c.Record(ModelUsage{Status: "degraded", UsageReported: true, UsageComplete: true})
		}()
	}
	wg.Wait()
	want := ExecutionSnapshot{Model: "model", PersistenceStatus: "failed", ResponseKind: "error", ToolExecutionCount: 3, IsComplete: true}
	c.SetExecution(want)
	if c.ExecutionSnapshot() != want {
		t.Fatal("lost final execution state")
	}
	s := c.Snapshot()
	if s.ModelCallCount != 20 || s.ModelFailureCount != 0 || s.UsageCompleteCallCount != 20 {
		t.Fatalf("execution changed provider meters: %+v", s)
	}
}

func TestUsageCollectorNilMissingUsageAndStatuses(t *testing.T) {
	var absent *UsageCollector
	absent.Record(ModelUsage{})
	if absent.Snapshot() != (UsageSnapshot{}) || UsageCollectorFromContext(nil) != nil || UsageCollectorFromContext(context.Background()) != nil {
		t.Fatal("nil behavior")
	}
	if UsageCollectorFromContext(WithUsageCollector(nil, nil)) != nil {
		t.Fatal("nil attachment")
	}
	c := NewUsageCollector()
	for _, status := range []string{"ok", "error", "rejected", "retry", "degraded", "invalid", ""} {
		c.Record(ModelUsage{Status: status, PromptTokens: 100, CompletionTokens: 100, TotalTokens: 200, DurationMS: -1})
	}
	c.Record(ModelUsage{Status: "ok", Submitted: true, UsageReported: true, PromptTokens: -1, CompletionTokens: -2, TotalTokens: -3})
	want := UsageSnapshot{ModelCallCount: 8, ModelSubmissionCount: 1, ModelFailureCount: 5, UsageReportedCallCount: 1}
	if got := c.Snapshot(); got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestUsageCompletenessIsSeparateFromReportedUsage(t *testing.T) {
	c := NewUsageCollector()
	c.Record(ModelUsage{Operation: "chat", Submitted: true, Status: "ok", UsageReported: true, UsageComplete: true})
	c.Record(ModelUsage{Operation: "chat", Submitted: true, Status: "degraded", UsageReported: true})
	c.Record(ModelUsage{Operation: "embedding", Submitted: true, Status: "ok", UsageReported: true, UsageComplete: true})
	s := c.Snapshot()
	if s.UsageReportedCallCount != 2 || s.UsageCompleteCallCount != 1 || s.ModelFailureCount != 0 || s.EmbeddingUsageCompleteCallCount != 1 {
		t.Fatalf("incorrect usage completeness: %+v", s)
	}
}
