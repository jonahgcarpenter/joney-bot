package promptbudget

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func TestNewContextBudgetUsesConfiguredLimitsAndFallbacks(t *testing.T) {
	tests := []struct {
		name                 string
		contextWindow        int
		maxOutputTokens      int
		wantContextWindow    int
		wantResponseReserve  int
		wantUsableInputLimit int
	}{
		{name: "configured", contextWindow: 10000, maxOutputTokens: 999, wantContextWindow: 10000, wantResponseReserve: 999, wantUsableInputLimit: 8745},
		{name: "defaults", wantContextWindow: 32768, wantResponseReserve: 8192, wantUsableInputLimit: 24320},
		{name: "context configured", contextWindow: 16000, wantContextWindow: 16000, wantResponseReserve: 8192, wantUsableInputLimit: 7552},
		{name: "output configured", maxOutputTokens: 2048, wantContextWindow: 32768, wantResponseReserve: 2048, wantUsableInputLimit: 30464},
		{name: "negative values", contextWindow: -1, maxOutputTokens: -1, wantContextWindow: 32768, wantResponseReserve: 8192, wantUsableInputLimit: 24320},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget := NewContextBudget(tt.contextWindow, tt.maxOutputTokens)
			if budget.ContextWindow != tt.wantContextWindow || budget.ResponseReserve != tt.wantResponseReserve || budget.UsableInputLimit() != tt.wantUsableInputLimit {
				t.Fatalf("unexpected budget: %+v", budget)
			}
		})
	}
}

func TestContextBudgetCannotExceedCapacity(t *testing.T) {
	budget := NewContextBudget(100, 100)
	if budget.UsableInputLimit() != 0 {
		t.Fatalf("budget must not exceed actual capacity: %+v", budget)
	}
}

func TestContextBudgetCapsExplicitLimitAtContextCapacity(t *testing.T) {
	budget := ContextBudget{ContextWindow: 8000, ResponseReserve: 2000, PromptLimit: 7000, SafetyMargin: 250}
	if got := budget.UsableInputLimit(); got != 5750 {
		t.Fatalf("UsableInputLimit() = %d, want 5750", got)
	}

	budget = ContextBudget{PromptLimit: 4000, SafetyMargin: 250}
	if got := budget.UsableInputLimit(); got != 3750 {
		t.Fatalf("explicit-only UsableInputLimit() = %d, want 3750", got)
	}
}

func TestEstimateRequestIncludesMessagesImagesAndTools(t *testing.T) {
	messages := []llm.ChatMessage{
		{Role: "system", Content: "system"},
		{Role: "user", Content: strings.Repeat("a", 100)},
		{Role: "assistant", Content: strings.Repeat("b", 100)},
		{Role: "user", Content: "now"},
	}
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "test.tool"}}}
	withoutTools := EstimateRequest(messages, nil)
	withoutImage := EstimateRequest(messages, tools)
	messages[len(messages)-1].Images = make([]llm.InputImage, 1)
	withImage := EstimateRequest(messages, tools)
	if withoutImage <= 0 {
		t.Fatalf("expected positive estimate, got %d", withoutImage)
	}
	if withoutImage <= withoutTools {
		t.Fatalf("expected tools to increase token count, got without=%d with=%d", withoutTools, withoutImage)
	}
	if withImage <= withoutImage {
		t.Fatalf("expected image estimate to increase token count, got without=%d with=%d", withoutImage, withImage)
	}
}

func TestEstimateRequestConservativelyCountsNonASCII(t *testing.T) {
	ascii := EstimateRequest([]llm.ChatMessage{{Role: "user", Content: strings.Repeat("a", 40)}}, nil)
	unicode := EstimateRequest([]llm.ChatMessage{{Role: "user", Content: strings.Repeat("界", 40)}}, nil)
	if unicode <= ascii {
		t.Fatalf("non-ASCII estimate = %d, want greater than ASCII estimate %d", unicode, ascii)
	}
}
