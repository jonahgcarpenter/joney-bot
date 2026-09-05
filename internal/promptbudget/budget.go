package promptbudget

import (
	"encoding/json"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const (
	defaultContextWindow   = 32768
	defaultResponseReserve = 8192
	defaultToolReserve     = 768
	defaultSafetyMargin    = 256
	messageTokenOverhead   = 12
	imageTokenEstimate     = 768
)

// ContextBudget describes the request-time prompt budget derived from the
// active model's context window.
type ContextBudget struct {
	ContextWindow   int
	ResponseReserve int
	ToolReserve     int
	SafetyMargin    int
	PromptLimit     int
}

// UsableInputLimit returns the model input capacity after output and safety
// reserves. An explicit prompt limit can further constrain that capacity.
func (b ContextBudget) UsableInputLimit() int {
	limit := b.PromptLimit
	if b.ContextWindow > 0 {
		contextLimit := b.ContextWindow - b.ResponseReserve
		if contextLimit < 0 {
			contextLimit = 0
		}
		if limit <= 0 || contextLimit < limit {
			limit = contextLimit
		}
	}
	limit -= b.SafetyMargin
	if limit < 0 {
		return 0
	}
	return limit
}

// PromptBudget returns the legacy input budget, including its fixed tool
// reserve. New request assembly should estimate the actual tool schemas with
// EstimateRequest and use UsableInputLimit.
func (b ContextBudget) PromptBudget() int {
	limit := b.UsableInputLimit() - b.ToolReserve
	if limit < 0 {
		return 0
	}
	return limit
}

// NewContextBudget derives prompt-budget settings from configured model limits.
// Non-positive limits use conservative package defaults.
func NewContextBudget(contextWindow, maxOutputTokens int) ContextBudget {
	budget := ContextBudget{
		ContextWindow:   defaultContextWindow,
		ResponseReserve: defaultResponseReserve,
		ToolReserve:     defaultToolReserve,
		SafetyMargin:    defaultSafetyMargin,
	}
	if contextWindow > 0 {
		budget.ContextWindow = contextWindow
	}
	if maxOutputTokens > 0 {
		budget.ResponseReserve = maxOutputTokens
	}
	return budget
}

// Result reports request-time prompt estimate details.
type Result struct {
	EstimatedBefore int
	EstimatedAfter  int
}

// EstimateTokens provides the shared token estimate used by the agent.
func EstimateTokens(systemPrompt string, history []llm.ChatMessage, userPrompt string, userImageCount int, tools []llm.Tool) int {
	messages := make([]llm.ChatMessage, 0, len(history)+2)
	messages = append(messages, llm.ChatMessage{Role: "system", Content: systemPrompt})
	messages = append(messages, history...)
	messages = append(messages, llm.ChatMessage{Role: "user", Content: userPrompt, Images: make([]llm.InputImage, userImageCount)})
	return EstimateRequest(messages, tools)
}

// EstimateRequest estimates the input tokens consumed by messages and tool
// schemas in one model request.
func EstimateRequest(messages []llm.ChatMessage, tools []llm.Tool) int {
	total := estimateToolTokens(tools)
	for _, msg := range messages {
		total += estimateMessageTokens(msg)
	}
	return total
}

func estimateMessageTokens(msg llm.ChatMessage) int {
	tokens := estimateTextTokens(msg.Role, msg.Content, msg.Thinking, msg.ToolName, msg.ToolCallID)
	for _, tc := range msg.ToolCalls {
		tokens += estimateTextTokens(tc.ID, tc.Function.Name, "function")
		if encoded, err := json.Marshal(tc.Function.Arguments); err == nil {
			tokens += estimateTextTokens(string(encoded))
		}
	}
	return tokens + messageTokenOverhead + len(msg.Images)*imageTokenEstimate
}

func estimateToolTokens(tools []llm.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return len(tools) * 64
	}
	return estimateTextTokens(string(encoded)) + 32
}

func estimateTextTokens(values ...string) int {
	asciiCount := 0
	nonASCIIcount := 0
	for _, value := range values {
		for _, r := range value {
			if r <= 0x7f {
				asciiCount++
			} else {
				nonASCIIcount++
			}
		}
	}
	return (asciiCount+3)/4 + nonASCIIcount
}
