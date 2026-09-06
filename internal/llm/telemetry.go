package llm

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// Call-local state survives failed streams without changing public responses.
type gatewayMeasurement struct {
	started      time.Time
	submitted    bool
	usage        gatewayUsage
	malformed    int
	firstOutput  *int64
	doneReason   string
	httpStatus   int
	phase        string
	invalidUsage bool
}

type gatewayMeasurementKey struct{}

func (m *gatewayMeasurement) observeUsage(usage gatewayUsage) {
	m.invalidUsage = m.invalidUsage || usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0
	// Invalid observations must not erase previously reported valid counts.
	if usage.promptReported && usage.PromptTokens >= 0 {
		m.usage.PromptTokens, m.usage.promptReported = usage.PromptTokens, true
	}
	if usage.completionReported && usage.CompletionTokens >= 0 {
		m.usage.CompletionTokens, m.usage.completionReported = usage.CompletionTokens, true
	}
	if usage.totalReported && usage.TotalTokens >= 0 {
		m.usage.TotalTokens, m.usage.totalReported = usage.TotalTokens, true
	}
	m.usage.reported = m.usage.reported || usage.reported
}

func measurementFromContext(ctx context.Context) *gatewayMeasurement {
	m, _ := ctx.Value(gatewayMeasurementKey{}).(*gatewayMeasurement)
	return m
}

func (c *GatewayClient) beginMeasurement(ctx context.Context, model, operation, transport string) (context.Context, func(error)) {
	m := &gatewayMeasurement{started: time.Now(), phase: "marshal"}
	meta := requestctx.MetadataFromContext(ctx)
	parent := meta.OperationID
	if parent == "" {
		parent = meta.ParentOperationID
	}
	meta.OperationID, meta.ParentOperationID = rand.Text(), parent
	switch meta.Workload {
	case "foreground", "formation", "compaction", "indexing", "maintenance", "system":
	default:
		meta.Workload = "system"
	}
	ctx = requestctx.WithMetadata(ctx, meta)
	ctx = context.WithValue(ctx, gatewayMeasurementKey{}, m)
	return ctx, func(err error) {
		invalidUsage := m.invalidUsage || m.usage.PromptTokens < 0 || m.usage.CompletionTokens < 0 || m.usage.TotalTokens < 0
		completeUsage := err == nil && !invalidUsage && m.usage.promptReported && m.usage.totalReported && (operation == "embedding" || m.usage.completionReported)
		status, outcome := "ok", "ok"
		if m.malformed > 0 || invalidUsage {
			status, outcome = "degraded", "degraded"
		}
		if err != nil {
			status, outcome = "error", "error"
			if errors.Is(err, context.Canceled) {
				status, outcome = "ok", "canceled"
			}
		}
		duration := time.Since(m.started)
		u := requestctx.ModelUsage{Operation: operation, Submitted: m.submitted, Status: status, UsageReported: m.usage.reported, PromptTokens: max(0, m.usage.PromptTokens), CompletionTokens: max(0, m.usage.CompletionTokens), TotalTokens: max(0, m.usage.TotalTokens), DurationMS: duration.Milliseconds()}
		u.UsageComplete = completeUsage
		requestctx.UsageCollectorFromContext(ctx).Record(u)
		fields := []config.Field{config.F("record_kind", "measurement"), config.F("operation", operation), config.F("transport", transport), config.F("status", status), config.F("outcome", outcome), config.F("is_submitted", m.submitted), config.F("is_usage_reported", u.UsageReported), config.F("duration_ms", u.DurationMS), config.F("malformed_chunk_count", m.malformed)}
		fields = append(fields, config.F("phase", m.phase), config.F("is_usage_complete", completeUsage), config.F("is_usage_invalid", invalidUsage))
		if u.UsageReported {
			if m.usage.promptReported && m.usage.PromptTokens >= 0 {
				fields = append(fields, config.F("prompt_tokens", u.PromptTokens))
			}
			if m.usage.completionReported && m.usage.CompletionTokens >= 0 {
				fields = append(fields, config.F("completion_tokens", u.CompletionTokens))
			}
			if m.usage.totalReported && m.usage.TotalTokens >= 0 {
				fields = append(fields, config.F("total_tokens", u.TotalTokens))
			}
			if operation == "chat" && m.usage.completionReported && !invalidUsage && duration > 0 {
				fields = append(fields, config.F("effective_output_tps", float64(u.CompletionTokens)/duration.Seconds()))
			}
		}
		if m.firstOutput != nil {
			fields = append(fields, config.F("time_to_first_output_ms", *m.firstOutput))
		}
		switch m.doneReason {
		case "stop", "length", "tool_calls", "function_call", "content_filter":
			fields = append(fields, config.F("done_reason", m.doneReason))
		}
		if code := config.HTTPStatus(err); code != 0 {
			m.httpStatus = code
		}
		if m.httpStatus != 0 {
			fields = append(fields, config.F("http_status", m.httpStatus))
		}
		if err != nil {
			fields = append(fields, config.ErrorField(err))
		}
		event := "provider.gateway.chat.complete"
		if operation == "embedding" {
			event = "provider.gateway.embed.complete"
		}
		c.requestLog(ctx, model).Info(event, "LLM gateway call completed", fields...)
	}
}
