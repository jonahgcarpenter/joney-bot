package agent

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// ModelMetrics holds performance data from a single LLM call.
type ModelMetrics struct {
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	DurationMS       int64   `json:"duration_ms,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
}

// Response is the final payload returned to the gateway after processing.
type Response struct {
	Kind               string                   `json:"-"`
	PersistenceStatus  string                   `json:"-"`
	ToolExecutionCount int                      `json:"-"`
	ToolBlockedCount   int                      `json:"-"`
	Model              string                   `json:"model"`
	Response           string                   `json:"response,omitempty"`
	Thinking           string                   `json:"thinking,omitempty"` // reasoning tokens emitted before the response
	Error              string                   `json:"error,omitempty"`
	Metrics            *ModelMetrics            `json:"metrics,omitempty"`
	Attachments        []media.OutputAttachment `json:"-"`

	SourceTurnID      int64 `json:"-"`
	SessionGeneration int   `json:"-"`
}

// Request contains one fully resolved request submitted to the agent.
type Request struct {
	RequestID   string
	Principal   identity.Principal
	DisplayName string
	SessionKey  string
	IsDirect    bool
	Prompt      string
	Images      []llm.InputImage
	StreamFunc  func(StreamChunk)
}

// mapMetrics converts an LLM response into a model metrics summary.
func mapMetrics(resp *llm.ChatResponse) *ModelMetrics {
	if resp == nil {
		return nil
	}
	tps := 0.0
	if resp.DurationMS > 0 && resp.CompletionTokens > 0 {
		tps = float64(resp.CompletionTokens) / (float64(resp.DurationMS) / 1000)
	}
	return &ModelMetrics{
		Model:            resp.Model,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		TotalTokens:      resp.TotalTokens,
		DurationMS:       resp.DurationMS,
		TokensPerSecond:  tps,
	}
}
