package agent

import (
	"fmt"
	"strings"

	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func stripReplyContext(prompt string) (string, bool) {
	prompt = strings.TrimSpace(prompt)
	if !strings.HasPrefix(prompt, "[Replying ") {
		return prompt, false
	}
	parts := strings.SplitN(prompt, "\n\n", 2)
	if len(parts) < 2 {
		return "", true
	}
	return strings.TrimSpace(parts[1]), true
}

func sessionMemoryUserContent(prompt string, imageCount int) string {
	content, hadReplyContext := stripReplyContext(prompt)
	if content == "" && hadReplyContext {
		content = "[User replied to a prior message]"
	}
	if imageCount > 0 {
		content = strings.TrimSpace(content + fmt.Sprintf("\n\n[Attached %d image(s)]", imageCount))
	}
	return strings.TrimSpace(content)
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func providerUserValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "You are speaking with ")
	value = strings.TrimSuffix(value, ".")
	return strings.TrimSpace(value)
}

func gatewaySystemPrompt(gateway string) string {
	switch strings.TrimSpace(strings.ToLower(gateway)) {
	case "imessage":
		return "# Gateway Instructions\nThe user is reading this in iMessage, which does not render Markdown. Write responses in plain text. Do not use Markdown formatting such as **bold**, headings, tables, fenced code blocks, or inline code ticks. Use simple line breaks and plain bullets when helpful."
	default:
		return ""
	}
}

func promptPressureVersion(model string, inputLimit int) string {
	return fmt.Sprintf("%s:%s:%d", sessionPromptPressurePrefix, strings.TrimSpace(model), inputLimit)
}

// PromptContext is a role-correct model context assembled within an input
// token limit. SelectedTurns are returned in chronological message order.
type PromptContext struct {
	Messages            []llm.ChatMessage
	SelectedTurns       []memory.SessionTurn
	SelectedToolNames   []string
	SelectedTurnCount   int
	OmittedTurnCount    int
	SelectedRecallCount int
	OmittedRecallCount  int
	RecallChars         int
	SummaryIncluded     bool
	SummaryChars        int
	MinimumTailCount    int
	SelectedRecall      []memory.RecallResult
	RequiredEstimate    int
	EstimatedBefore     int
	EstimatedAfter      int
	InputLimit          int
	RequiredOverBudget  bool
}

// AssemblePromptContext reserves a bounded historical summary and a
// caller-selected newest verbatim tail before recall and additional history.
func AssemblePromptContext(
	deploymentPolicy string,
	tenantProfile string,
	currentPrompt string,
	currentImages []llm.InputImage,
	summary memory.SessionSummary,
	minimumTail int,
	recallResults []memory.RecallResult,
	recallCharLimit int,
	recentTurns []memory.SessionTurn,
	tools []llm.Tool,
	inputLimit int,
) PromptContext {
	recentTurns = prepareHistoricalTurns(recentTurns, tools)
	required := make([]llm.ChatMessage, 0, 3)
	required = append(required, llm.ChatMessage{Role: "system", Content: deploymentPolicy})
	if tenantProfile != "" {
		required = append(required, llm.ChatMessage{Role: "user", Content: tenantProfile})
	}
	current := llm.ChatMessage{
		Role:    "user",
		Content: currentPrompt,
		Images:  append([]llm.InputImage(nil), currentImages...),
	}
	required = append(required, current)

	result := PromptContext{
		InputLimit:         inputLimit,
		RequiredEstimate:   tokenbudget.EstimateRequest(required, tools),
		OmittedRecallCount: len(recallResults),
	}
	summaryBlock := memory.RenderSessionSummary(summary)
	allRequired := withRecall(required, memory.RenderDurableMemoryRecall(recallResults, recallCharLimit))
	allMessages := messagesWithSummaryAndTurns(allRequired, summaryBlock, recentTurns)
	result.EstimatedBefore = tokenbudget.EstimateRequest(allMessages, tools)
	result.RequiredOverBudget = result.RequiredEstimate > inputLimit

	selectedNewestFirst := make([]memory.SessionTurn, 0, len(recentTurns))
	selectedSummary := ""
	if !result.RequiredOverBudget {
		if minimumTail < 0 {
			minimumTail = 0
		}
		if minimumTail > len(recentTurns) {
			minimumTail = len(recentTurns)
		}
		for _, turn := range recentTurns[:minimumTail] {
			candidate := append(selectedNewestFirst, turn)
			candidateMessages := messagesWithSummaryAndTurns(required, "", candidate)
			if tokenbudget.EstimateRequest(candidateMessages, tools) > inputLimit {
				compact := withoutToolHistory(turn)
				candidate = append(selectedNewestFirst, compact)
				candidateMessages = messagesWithSummaryAndTurns(required, "", candidate)
				if tokenbudget.EstimateRequest(candidateMessages, tools) > inputLimit {
					break
				}
			}
			selectedNewestFirst = candidate
		}
		if summaryBlock != "" && tokenbudget.EstimateRequest(messagesWithSummaryAndTurns(required, summaryBlock, selectedNewestFirst), tools) <= inputLimit {
			selectedSummary = summaryBlock
		}
	}
	result.MinimumTailCount = len(selectedNewestFirst)
	result.SummaryIncluded = selectedSummary != ""
	result.SummaryChars = len([]rune(selectedSummary))

	selectedRecall := make([]memory.RecallResult, 0, len(recallResults))
	if !result.RequiredOverBudget {
		for _, recall := range recallResults {
			candidate := append(selectedRecall, recall)
			block := memory.RenderDurableMemoryRecall(candidate, recallCharLimit)
			if block == "" || len(block) == len(memory.RenderDurableMemoryRecall(selectedRecall, recallCharLimit)) {
				continue
			}
			candidateRequired := withRecall(required, block)
			if tokenbudget.EstimateRequest(messagesWithSummaryAndTurns(candidateRequired, selectedSummary, selectedNewestFirst), tools) > inputLimit {
				continue
			}
			selectedRecall = candidate
		}
	}
	recallBlock := memory.RenderDurableMemoryRecall(selectedRecall, recallCharLimit)
	required = withRecall(required, recallBlock)
	result.SelectedRecallCount = len(selectedRecall)
	result.SelectedRecall = append([]memory.RecallResult(nil), selectedRecall...)
	result.OmittedRecallCount = len(recallResults) - len(selectedRecall)
	result.RecallChars = len([]rune(recallBlock))

	if !result.RequiredOverBudget {
		for _, turn := range recentTurns[len(selectedNewestFirst):] {
			candidate := append(selectedNewestFirst, turn)
			if tokenbudget.EstimateRequest(messagesWithSummaryAndTurns(required, selectedSummary, candidate), tools) > inputLimit {
				candidate = append(selectedNewestFirst, withoutToolHistory(turn))
				if tokenbudget.EstimateRequest(messagesWithSummaryAndTurns(required, selectedSummary, candidate), tools) > inputLimit {
					break
				}
			}
			selectedNewestFirst = candidate
		}
	}

	result.SelectedTurns = reverseTurns(selectedNewestFirst)
	result.Messages = messagesWithSummaryAndChronologicalTurns(required, selectedSummary, result.SelectedTurns)
	result.SelectedToolNames = selectedToolNames(result.SelectedTurns)
	result.SelectedTurnCount = len(result.SelectedTurns)
	result.OmittedTurnCount = len(recentTurns) - result.SelectedTurnCount
	result.EstimatedAfter = tokenbudget.EstimateRequest(result.Messages, tools)
	return result
}

func prepareHistoricalTurns(turns []memory.SessionTurn, tools []llm.Tool) []memory.SessionTurn {
	available := make(map[string]bool, len(tools))
	for _, tool := range tools {
		available[tool.Function.Name] = true
	}
	prepared := append([]memory.SessionTurn(nil), turns...)
	for i := range prepared {
		for _, batch := range prepared[i].ToolHistory.Batches {
			for _, call := range batch.Calls {
				if !available[call.Name] || call.ArgumentsTruncated || (call.HistoryMode != "" && call.HistoryMode != string(governance.HistoryFull)) {
					prepared[i] = withoutToolHistory(prepared[i])
					break
				}
			}
			if len(prepared[i].ToolHistory.Batches) == 0 {
				break
			}
		}
	}
	return prepared
}

func withoutToolHistory(turn memory.SessionTurn) memory.SessionTurn {
	turn.ToolHistory = memory.EmptyToolHistory()
	return turn
}

func preservedRecentTailCount(recentTurns []memory.SessionTurn, inputLimit int) int {
	budget := tokenbudget.RecentTailLimit(inputLimit)
	total := 0
	count := 0
	for _, turn := range recentTurns {
		if count == 2 {
			break
		}
		size := tokenbudget.EstimateRequest(memory.SessionTurnMessages(turn), nil)
		if total+size > budget {
			break
		}
		total += size
		count++
	}
	return count
}

func messagesWithSummaryAndTurns(required []llm.ChatMessage, summary string, newestFirst []memory.SessionTurn) []llm.ChatMessage {
	return messagesWithSummaryAndChronologicalTurns(required, summary, reverseTurns(newestFirst))
}

func messagesWithSummaryAndChronologicalTurns(required []llm.ChatMessage, summary string, chronological []memory.SessionTurn) []llm.ChatMessage {
	messages := make([]llm.ChatMessage, 0, len(required)+len(chronological)*2+1)
	last := len(required) - 1
	messages = append(messages, required[:last]...)
	if summary != "" {
		messages = append(messages, llm.ChatMessage{Role: "user", Content: summary})
	}
	for _, turn := range chronological {
		messages = append(messages, memory.SessionTurnMessages(turn)...)
	}
	messages = append(messages, required[last])
	return messages
}

func withRecall(required []llm.ChatMessage, recallBlock string) []llm.ChatMessage {
	messages := append([]llm.ChatMessage(nil), required...)
	if recallBlock == "" {
		return messages
	}
	last := len(messages) - 1
	messages[last].Content = strings.TrimSpace(messages[last].Content + "\n\n" + recallBlock)
	return messages
}

func reverseTurns(turns []memory.SessionTurn) []memory.SessionTurn {
	reversed := make([]memory.SessionTurn, len(turns))
	for i := range turns {
		reversed[len(turns)-1-i] = turns[i]
	}
	return reversed
}

func selectedToolNames(turns []memory.SessionTurn) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, turn := range turns {
		for _, name := range turn.ToolNames {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}

func uniqueToolNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
