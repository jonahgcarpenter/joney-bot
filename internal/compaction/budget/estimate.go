package budget

import (
	"encoding/json"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const (
	messageTokenOverhead = 12
	imageTokenEstimate   = 768
)

// EstimateCompletedRequest adds the completed exchange to the original prompt
// estimate without counting its current user text twice.
func EstimateCompletedRequest(promptTokens int, currentUserText string, exchange []llm.ChatMessage) int {
	userOnly := EstimateRequest([]llm.ChatMessage{{Role: "user", Content: currentUserText}}, nil)
	pressure := promptTokens + EstimateRequest(exchange, nil) - userOnly
	if pressure < 0 {
		return 0
	}
	return pressure
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
