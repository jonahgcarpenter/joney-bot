package agent

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// PromptContext is a role-correct model context assembled within an input
// token limit. SelectedTurns are returned in chronological message order.
type PromptContext struct {
	Messages            []llm.ChatMessage
	SelectedTurns       []usermemory.SessionTurn
	SelectedToolNames   []string
	SelectedTurnCount   int
	OmittedTurnCount    int
	SelectedRecallCount int
	OmittedRecallCount  int
	RecallChars         int
	SummaryIncluded     bool
	SummaryChars        int
	MinimumTailCount    int
	SelectedRecall      []usermemory.RecallResult
	RequiredEstimate    int
	EstimatedBefore     int
	EstimatedAfter      int
	InputLimit          int
	RequiredOverBudget  bool
}

// AssemblePromptContextWithSummary reserves a bounded historical summary and a
// caller-selected newest verbatim tail before recall and additional history.
func AssemblePromptContextWithSummary(
	deploymentPolicy string,
	tenantProfile string,
	currentPrompt string,
	currentImages []llm.InputImage,
	summary usermemory.SessionSummary,
	minimumTail int,
	recallResults []usermemory.RecallResult,
	recallCharLimit int,
	recentTurns []usermemory.SessionTurn,
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
		RequiredEstimate:   promptbudget.EstimateRequest(required, tools),
		OmittedRecallCount: len(recallResults),
	}
	summaryBlock := usermemory.RenderSessionSummary(summary)
	allRequired := withRecall(required, usermemory.RenderDurableMemoryRecall(recallResults, recallCharLimit))
	allMessages := messagesWithSummaryAndTurns(allRequired, summaryBlock, recentTurns)
	result.EstimatedBefore = promptbudget.EstimateRequest(allMessages, tools)
	result.RequiredOverBudget = result.RequiredEstimate > inputLimit

	selectedNewestFirst := make([]usermemory.SessionTurn, 0, len(recentTurns))
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
			if promptbudget.EstimateRequest(candidateMessages, tools) > inputLimit {
				compact := compactHistoricalTurn(turn)
				candidate = append(selectedNewestFirst, compact)
				candidateMessages = messagesWithSummaryAndTurns(required, "", candidate)
				if promptbudget.EstimateRequest(candidateMessages, tools) > inputLimit {
					break
				}
			}
			selectedNewestFirst = candidate
		}
		if summaryBlock != "" && promptbudget.EstimateRequest(messagesWithSummaryAndTurns(required, summaryBlock, selectedNewestFirst), tools) <= inputLimit {
			selectedSummary = summaryBlock
		}
	}
	result.MinimumTailCount = len(selectedNewestFirst)
	result.SummaryIncluded = selectedSummary != ""
	result.SummaryChars = len([]rune(selectedSummary))

	selectedRecall := make([]usermemory.RecallResult, 0, len(recallResults))
	if !result.RequiredOverBudget {
		for _, recall := range recallResults {
			candidate := append(selectedRecall, recall)
			block := usermemory.RenderDurableMemoryRecall(candidate, recallCharLimit)
			if block == "" || len(block) == len(usermemory.RenderDurableMemoryRecall(selectedRecall, recallCharLimit)) {
				continue
			}
			candidateRequired := withRecall(required, block)
			if promptbudget.EstimateRequest(messagesWithSummaryAndTurns(candidateRequired, selectedSummary, selectedNewestFirst), tools) > inputLimit {
				continue
			}
			selectedRecall = candidate
		}
	}
	recallBlock := usermemory.RenderDurableMemoryRecall(selectedRecall, recallCharLimit)
	required = withRecall(required, recallBlock)
	result.SelectedRecallCount = len(selectedRecall)
	result.SelectedRecall = append([]usermemory.RecallResult(nil), selectedRecall...)
	result.OmittedRecallCount = len(recallResults) - len(selectedRecall)
	result.RecallChars = len([]rune(recallBlock))

	if !result.RequiredOverBudget {
		for _, turn := range recentTurns[len(selectedNewestFirst):] {
			candidate := append(selectedNewestFirst, turn)
			if promptbudget.EstimateRequest(messagesWithSummaryAndTurns(required, selectedSummary, candidate), tools) > inputLimit {
				candidate = append(selectedNewestFirst, compactHistoricalTurn(turn))
				if promptbudget.EstimateRequest(messagesWithSummaryAndTurns(required, selectedSummary, candidate), tools) > inputLimit {
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
	result.EstimatedAfter = promptbudget.EstimateRequest(result.Messages, tools)
	return result
}

func prepareHistoricalTurns(turns []usermemory.SessionTurn, tools []llm.Tool) []usermemory.SessionTurn {
	available := make(map[string]bool, len(tools))
	for _, tool := range tools {
		available[tool.Function.Name] = true
	}
	prepared := append([]usermemory.SessionTurn(nil), turns...)
	for i := range prepared {
		for _, batch := range prepared[i].ToolHistory.Batches {
			for _, call := range batch.Calls {
				if !available[call.Name] || call.ArgumentsTruncated || (call.HistoryMode != "" && call.HistoryMode != string(governance.HistoryFull)) {
					prepared[i] = compactHistoricalTurn(prepared[i])
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

func compactHistoricalTurn(turn usermemory.SessionTurn) usermemory.SessionTurn {
	turn.ToolHistory = usermemory.EmptyToolHistory()
	return turn
}

func preservedRecentTailCount(recentTurns []usermemory.SessionTurn, inputLimit int) int {
	budget := inputLimit / 4
	if budget < 2000 {
		budget = 2000
	}
	if budget > 8000 {
		budget = 8000
	}
	if budget > inputLimit {
		budget = inputLimit
	}
	total := 0
	count := 0
	for _, turn := range recentTurns {
		if count == 2 {
			break
		}
		size := promptbudget.EstimateRequest(usermemory.SessionTurnMessages(turn), nil)
		if total+size > budget {
			break
		}
		total += size
		count++
	}
	return count
}

func completedPromptPressure(prompt PromptContext, storedTurn usermemory.SessionTurn) int {
	userOnly := promptbudget.EstimateRequest([]llm.ChatMessage{{Role: "user", Content: storedTurn.UserText}}, nil)
	pressure := prompt.EstimatedBefore + promptbudget.EstimateRequest(usermemory.SessionTurnMessages(storedTurn), nil) - userOnly
	if pressure < 0 {
		return 0
	}
	return pressure
}

func messagesWithSummaryAndTurns(required []llm.ChatMessage, summary string, newestFirst []usermemory.SessionTurn) []llm.ChatMessage {
	return messagesWithSummaryAndChronologicalTurns(required, summary, reverseTurns(newestFirst))
}

func messagesWithSummaryAndChronologicalTurns(required []llm.ChatMessage, summary string, chronological []usermemory.SessionTurn) []llm.ChatMessage {
	messages := make([]llm.ChatMessage, 0, len(required)+len(chronological)*2+1)
	last := len(required) - 1
	messages = append(messages, required[:last]...)
	if summary != "" {
		messages = append(messages, llm.ChatMessage{Role: "user", Content: summary})
	}
	for _, turn := range chronological {
		messages = append(messages, usermemory.SessionTurnMessages(turn)...)
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

func reverseTurns(turns []usermemory.SessionTurn) []usermemory.SessionTurn {
	reversed := make([]usermemory.SessionTurn, len(turns))
	for i := range turns {
		reversed[len(turns)-1-i] = turns[i]
	}
	return reversed
}

func selectedToolNames(turns []usermemory.SessionTurn) []string {
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
