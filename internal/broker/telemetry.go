package broker

import (
	"context"
	"errors"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// This final execution-stage summary is not a provider measurement or a second
// gateway terminal record. It observes late usage after immediate stop delivery.
func (b *Broker) logExecutionComplete(ctx context.Context, usage *requestctx.UsageCollector, response *agent.Response, err error) {
	// Typed failures in joined errors take precedence over cancellation.
	errorCode := config.ErrorCode(err)
	canceled := errorCode == "canceled" || (errorCode == "unknown_error" && errors.Is(err, ErrAgentWorkCanceled))
	e := usage.ExecutionSnapshot()
	if !e.IsComplete {
		e.IsComplete = true
		if e.PersistenceStatus == "" {
			e.PersistenceStatus = "not_attempted"
		}
		e.ResponseKind = "answer"
		if response != nil {
			e.Model = response.Model
			e.ToolExecutionCount, e.BlockedCount = response.ToolExecutionCount, response.ToolBlockedCount
			if response.Kind != "" {
				e.ResponseKind = response.Kind
			}
			if response.Error != "" {
				e.ResponseKind = "provider_error"
			}
			if response.PersistenceStatus != "" {
				e.PersistenceStatus = response.PersistenceStatus
			}
		}
		if err != nil {
			e.ResponseKind = "error"
		}
		if canceled {
			e.ResponseKind = "canceled"
		}
		usage.SetExecution(e)
	}
	status, outcome := "ok", "ok"
	if e.ResponseKind != "answer" {
		status = "degraded"
	}
	if err != nil || e.ResponseKind == "provider_error" {
		status, outcome = "error", "error"
	}
	if canceled || (err == nil && status != "error" && context.Cause(ctx) != nil) {
		status, outcome = "ok", "canceled"
	}
	s := usage.Snapshot()
	b.log.Info("broker.request.execution.complete", "completed broker processor execution", append(requestctx.LogFields(ctx),
		config.F("record_kind", "summary"), config.F("is_execution_complete", true), config.F("status", status), config.F("outcome", outcome),
		config.ErrorField(err),
		config.F("model", e.Model), config.F("response_kind", e.ResponseKind), config.F("persistence_status", e.PersistenceStatus),
		config.F("execution_tool_count", e.ToolExecutionCount), config.F("execution_blocked_count", e.BlockedCount),
		config.F("execution_model_call_count", s.ModelCallCount), config.F("execution_model_submission_count", s.ModelSubmissionCount),
		config.F("execution_model_failure_count", s.ModelFailureCount), config.F("execution_usage_reported_call_count", s.UsageReportedCallCount),
		config.F("execution_usage_complete_call_count", s.UsageCompleteCallCount), config.F("is_execution_usage_complete", s.ModelSubmissionCount > 0 && s.UsageCompleteCallCount == s.ModelSubmissionCount),
		config.F("execution_prompt_tokens", s.PromptTokens), config.F("execution_completion_tokens", s.CompletionTokens), config.F("execution_total_tokens", s.TotalTokens),
		config.F("execution_model_duration_ms", s.ModelDurationMS), config.F("execution_embedding_call_count", s.EmbeddingCallCount),
		config.F("execution_embedding_submission_count", s.EmbeddingSubmissionCount), config.F("execution_embedding_failure_count", s.EmbeddingFailureCount),
		config.F("execution_embedding_usage_reported_call_count", s.EmbeddingUsageReportedCallCount), config.F("execution_embedding_usage_complete_call_count", s.EmbeddingUsageCompleteCallCount),
		config.F("execution_embedding_prompt_tokens", s.EmbeddingPromptTokens), config.F("execution_embedding_completion_tokens", s.EmbeddingCompletionTokens), config.F("execution_embedding_total_tokens", s.EmbeddingTotalTokens),
		config.F("execution_embedding_duration_ms", s.EmbeddingDurationMS))...)
}

// Snapshot is a consistent point-in-time view of foreground and background work.
type Snapshot struct {
	WorkerCount, QueuedCount, ActiveCount, OutstandingCount, Capacity int
	OldestQueuedAgeMS                                                 int64
	IsAccepting, IsBackgroundActive                                   bool
}

// Snapshot reports outstanding work, including queued work waiting for fences.
func (b *Broker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{WorkerCount: b.workerCount, OutstandingCount: b.outstanding, Capacity: cap(b.ready), IsAccepting: b.accepting, IsBackgroundActive: b.background != nil}
	for _, lane := range b.lanes {
		for _, w := range lane {
			if w.workerActive {
				s.ActiveCount++
			} else {
				s.QueuedCount++
				if !w.queuedAt.IsZero() {
					s.OldestQueuedAgeMS = max(s.OldestQueuedAgeMS, time.Since(w.queuedAt).Milliseconds())
				}
			}
		}
	}
	return s
}

func (b *Broker) logHealth() {
	s := b.Snapshot()
	b.log.Info("broker.health", "broker health snapshot",
		config.F("record_kind", "snapshot"),
		config.F("worker_count", s.WorkerCount), config.F("queued_count", s.QueuedCount),
		config.F("active_count", s.ActiveCount), config.F("outstanding_count", s.OutstandingCount),
		config.F("capacity", s.Capacity), config.F("oldest_queued_age_ms", s.OldestQueuedAgeMS),
		config.F("is_accepting", s.IsAccepting), config.F("is_background_active", s.IsBackgroundActive), config.F("status", "ok"))
}
