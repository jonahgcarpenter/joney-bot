package agent

import (
	"reflect"
	"strings"
	"testing"

	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func assembleTestPromptContext(policy, profile, prompt string, images []llm.InputImage, turns []memory.SessionTurn, tools []llm.Tool, limit int) PromptContext {
	return AssemblePromptContext(policy, profile, prompt, images, memory.SessionSummary{}, 0, nil, 0, turns, tools, limit)
}

func TestAssemblePromptContextPreservesRolesAndOrder(t *testing.T) {
	turns := []memory.SessionTurn{
		{ID: 3, UserText: "new user", AssistantText: "new assistant", ToolNames: []string{"web.search", "time.current"}},
		{ID: 2, UserText: "middle user", AssistantText: "middle assistant", ToolNames: []string{"web.search"}},
		{ID: 1, UserText: "old user", AssistantText: "old assistant"},
	}

	got := assembleTestPromptContext("deployment policy", "tenant profile", "current", nil, turns, nil, 100000)
	wantRoles := []string{"system", "user", "user", "assistant", "user", "assistant", "user", "assistant", "user"}
	wantContents := []string{
		"deployment policy", "tenant profile",
		"old user", "old assistant",
		"middle user", "middle assistant",
		"new user", "new assistant",
		"current",
	}
	if roles(got.Messages) != strings.Join(wantRoles, ",") {
		t.Fatalf("roles = %s, want %s", roles(got.Messages), strings.Join(wantRoles, ","))
	}
	for i, message := range got.Messages {
		if message.Content != wantContents[i] {
			t.Fatalf("message %d content = %q, want %q", i, message.Content, wantContents[i])
		}
	}
	if ids(got.SelectedTurns) != "1,2,3" {
		t.Fatalf("selected turn order = %s, want 1,2,3", ids(got.SelectedTurns))
	}
	if !reflect.DeepEqual(got.SelectedToolNames, []string{"web.search", "time.current"}) {
		t.Fatalf("selected tools = %#v", got.SelectedToolNames)
	}
	if got.SelectedTurnCount != 3 || got.OmittedTurnCount != 0 {
		t.Fatalf("unexpected counts: selected=%d omitted=%d", got.SelectedTurnCount, got.OmittedTurnCount)
	}
}

func TestAssemblePromptContextReplaysNativeToolHistoryAndFallsBackWhenUnavailable(t *testing.T) {
	history := memory.ToolHistory{Version: memory.ToolHistoryVersion, Batches: []memory.ToolHistoryBatch{{Calls: []memory.ToolHistoryCall{{
		Name: "weather.current", Arguments: map[string]interface{}{"city": "Louisville"}, Status: "succeeded", Outcome: "productive", Result: `{"temperature":72}`, ExecutedAt: "2026-08-28T12:00:00Z",
	}}}}}
	turn := memory.SessionTurn{ID: 41, UserText: "weather?", AssistantText: "72 degrees", ToolNames: []string{"weather.current"}, ToolHistory: history}
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "weather.current"}}}

	full := assembleTestPromptContext("policy", "", "follow up", nil, []memory.SessionTurn{turn}, tools, 100000)
	if roles(full.Messages) != "system,user,assistant,tool,assistant,user" {
		t.Fatalf("native history roles=%s messages=%+v", roles(full.Messages), full.Messages)
	}
	if full.Messages[2].ToolCalls[0].ID != "hist_41_1_1" || full.Messages[3].ToolCallID != "hist_41_1_1" || !strings.Contains(full.Messages[3].Content, "potentially stale") {
		t.Fatalf("native history correlation=%+v", full.Messages)
	}

	compact := assembleTestPromptContext("policy", "", "follow up", nil, []memory.SessionTurn{turn}, nil, 100000)
	if roles(compact.Messages) != "system,user,assistant,user" || compact.Messages[2].Content != "72 degrees" {
		t.Fatalf("unavailable tool did not use compact replay: %+v", compact.Messages)
	}
	compactWithTool := assembleTestPromptContext("policy", "", "follow up", nil, []memory.SessionTurn{withoutToolHistory(turn)}, tools, 100000)
	budgetFallback := assembleTestPromptContext("policy", "", "follow up", nil, []memory.SessionTurn{turn}, tools, compactWithTool.EstimatedAfter)
	if budgetFallback.SelectedTurnCount != 1 || roles(budgetFallback.Messages) != "system,user,assistant,user" {
		t.Fatalf("oversized native history did not compact before omission: %+v", budgetFallback)
	}
}

func TestCompletedPromptPressureIncludesStoredAssistantReplay(t *testing.T) {
	prompt := PromptContext{EstimatedBefore: 1000}
	turn := memory.SessionTurn{UserText: "current user", AssistantText: strings.Repeat("answer ", 40), ToolNames: []string{"web.search"}}
	got := tokenbudget.EstimateCompletedRequest(prompt.EstimatedBefore, turn.UserText, memory.SessionTurnMessages(turn))
	userOnly := tokenbudget.EstimateRequest([]llm.ChatMessage{{Role: "user", Content: turn.UserText}}, nil)
	want := prompt.EstimatedBefore + tokenbudget.EstimateRequest(memory.SessionTurnMessages(turn), nil) - userOnly
	if got != want || got <= prompt.EstimatedBefore {
		t.Fatalf("completed pressure=%d want=%d before=%d", got, want, prompt.EstimatedBefore)
	}
}

func TestAssemblePromptContextExactFitAndOneTokenOver(t *testing.T) {
	turn := memory.SessionTurn{UserText: "historical question", AssistantText: "historical answer"}
	all := assembleTestPromptContext("policy", "", "now", nil, []memory.SessionTurn{turn}, nil, 100000)

	exact := assembleTestPromptContext("policy", "", "now", nil, []memory.SessionTurn{turn}, nil, all.EstimatedAfter)
	if exact.SelectedTurnCount != 1 || exact.EstimatedAfter != all.EstimatedAfter {
		t.Fatalf("exact fit rejected: %+v", exact)
	}

	over := assembleTestPromptContext("policy", "", "now", nil, []memory.SessionTurn{turn}, nil, all.EstimatedAfter-1)
	if over.SelectedTurnCount != 0 || len(over.Messages) != 2 {
		t.Fatalf("one-token over included history: %+v", over)
	}
}

func TestAssemblePromptContextStopsAtOversizedNewestTurn(t *testing.T) {
	turns := []memory.SessionTurn{
		{ID: 3, UserText: strings.Repeat("界", 4000), AssistantText: "too large"},
		{ID: 2, UserText: "small", AssistantText: "would fit"},
	}
	required := assembleTestPromptContext("policy", "profile", "current", nil, nil, nil, 100000)
	smallOnly := assembleTestPromptContext("policy", "profile", "current", nil, turns[1:], nil, 100000)
	limit := smallOnly.EstimatedAfter
	if limit <= required.EstimatedAfter {
		t.Fatal("test setup did not make the older turn fit")
	}

	got := assembleTestPromptContext("policy", "profile", "current", nil, turns, nil, limit)
	if got.SelectedTurnCount != 0 || got.OmittedTurnCount != 2 {
		t.Fatalf("assembler skipped past non-fitting newest turn: %+v", got)
	}
	if !strings.Contains(turns[0].UserText, "界") {
		t.Fatal("UTF-8 test fixture was unexpectedly modified")
	}
}

func TestAssemblePromptContextRequiredOverBudgetPreservesRequiredMessages(t *testing.T) {
	images := []llm.InputImage{{MimeType: "image/png", Data: "one"}, {MimeType: "image/jpeg", Data: "two"}}
	turns := []memory.SessionTurn{{UserText: "old user", AssistantText: "old assistant"}}
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "web.search", Description: strings.Repeat("schema", 100)}}}

	got := assembleTestPromptContext("policy", "profile", "current", images, turns, tools, 1)
	if !got.RequiredOverBudget || got.SelectedTurnCount != 0 || got.OmittedTurnCount != 1 {
		t.Fatalf("unexpected over-budget result: %+v", got)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" || got.Messages[2].Role != "user" {
		t.Fatalf("required messages not preserved: %#v", got.Messages)
	}
	if len(got.Messages[2].Images) != 2 || len(got.Messages[0].Images) != 0 || len(got.Messages[1].Images) != 0 {
		t.Fatalf("images must remain current-turn-only: %#v", got.Messages)
	}
	if got.EstimatedAfter != got.RequiredEstimate || got.EstimatedBefore <= got.EstimatedAfter {
		t.Fatalf("unexpected estimates: before=%d required=%d after=%d", got.EstimatedBefore, got.RequiredEstimate, got.EstimatedAfter)
	}
}

func TestAssemblePromptContextToolsAffectSelectionBudget(t *testing.T) {
	turn := memory.SessionTurn{UserText: strings.Repeat("u", 100), AssistantText: strings.Repeat("a", 100)}
	withoutTools := assembleTestPromptContext("policy", "", "current", nil, []memory.SessionTurn{turn}, nil, 100000)
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "large.tool", Description: strings.Repeat("description", 200)}}}

	got := assembleTestPromptContext("policy", "", "current", nil, []memory.SessionTurn{turn}, tools, withoutTools.EstimatedAfter)
	if got.SelectedTurnCount != 0 {
		t.Fatalf("tool schema was not included in the selection estimate: %+v", got)
	}
	if got.RequiredEstimate != tokenbudget.EstimateRequest(got.Messages, tools) {
		t.Fatalf("required estimate does not include tools: got %d", got.RequiredEstimate)
	}
}

func TestAssemblePromptContextAddsBoundedRecallToCurrentUser(t *testing.T) {
	recall := []memory.RecallResult{{
		Entry:      memory.MemoryEntry{ID: 1, Scope: "long_term", Category: "projects", Statement: "Project codename is Atlas", Confidence: 0.9, Importance: 4},
		Score:      0.9,
		Provenance: []memory.RecallProvenance{{Source: memory.RecallSourceLexical, Relevance: 1, Authority: memory.RecallAuthorityUserStated}},
	}}
	got := AssemblePromptContext("policy", "profile", "What is the codename?", nil, memory.SessionSummary{}, 0, recall, 2000, nil, nil, 100000)
	if got.SelectedRecallCount != 1 || got.OmittedRecallCount != 0 || got.RecallChars == 0 {
		t.Fatalf("unexpected recall selection: %+v", got)
	}
	current := got.Messages[len(got.Messages)-1]
	if current.Role != "user" || !strings.Contains(current.Content, "What is the codename?") || !strings.Contains(current.Content, "UNTRUSTED LOWER-AUTHORITY REFERENCE") || !strings.Contains(current.Content, "Atlas") {
		t.Fatalf("recall not attached to current user turn: %+v", current)
	}
	if strings.Contains(got.Messages[0].Content, "Atlas") || strings.Contains(got.Messages[1].Content, "Atlas") {
		t.Fatalf("recall gained policy/profile authority: %+v", got.Messages)
	}
}

func TestAssemblePromptContextOmitsRecallBeforeRequiredContent(t *testing.T) {
	recall := []memory.RecallResult{{Entry: memory.MemoryEntry{ID: 1, Scope: "long_term", Category: "notes", Statement: strings.Repeat("memory ", 100), Confidence: 1, Importance: 5}, Score: 1}}
	required := assembleTestPromptContext("policy", "profile", "current", nil, nil, nil, 100000)
	got := AssemblePromptContext("policy", "profile", "current", nil, memory.SessionSummary{}, 0, recall, 2000, nil, nil, required.EstimatedAfter)
	if got.SelectedRecallCount != 0 || got.OmittedRecallCount != 1 || got.Messages[len(got.Messages)-1].Content != "current" {
		t.Fatalf("optional recall displaced required content: %+v", got)
	}
}

func TestAssemblePromptContextPlacesSummaryBeforeRoleCorrectTail(t *testing.T) {
	summary := memory.SessionSummary{ID: 7, CoveredFromTurnID: 1, CoveredThroughTurnID: 10, Narrative: "Atlas was selected.", OpenTasks: []string{"Ship Atlas"}}
	turns := []memory.SessionTurn{{ID: 12, UserText: "new user", AssistantText: "new assistant"}, {ID: 11, UserText: "older user", AssistantText: "older assistant"}}
	got := AssemblePromptContext("policy", "profile", "current", nil, summary, 1, nil, 0, turns, nil, 100000)
	if !got.SummaryIncluded || got.SummaryChars == 0 || got.MinimumTailCount != 1 || got.SelectedTurnCount != 2 {
		t.Fatalf("unexpected summary selection: %+v", got)
	}
	if roles(got.Messages) != "system,user,user,user,assistant,user,assistant,user" {
		t.Fatalf("summary/tail roles=%s messages=%+v", roles(got.Messages), got.Messages)
	}
	if !strings.Contains(got.Messages[2].Content, "session_history_summary") || !strings.Contains(got.Messages[2].Content, "untrusted_historical_reference") || !strings.Contains(got.Messages[2].Content, "Atlas was selected") {
		t.Fatalf("summary not safely rendered: %+v", got.Messages[2])
	}
	if strings.Contains(got.Messages[0].Content, "Atlas was selected") || strings.Contains(got.Messages[1].Content, "Atlas was selected") {
		t.Fatalf("summary gained policy/profile authority: %+v", got.Messages)
	}
}

func TestAssemblePromptContextReservesSummaryAndMinimumTailBeforeRecall(t *testing.T) {
	summary := memory.SessionSummary{ID: 3, CoveredFromTurnID: 1, CoveredThroughTurnID: 20, Narrative: strings.Repeat("summary ", 30)}
	turns := make([]memory.SessionTurn, 0, 10)
	for i := 0; i < 8; i++ {
		turns = append(turns, memory.SessionTurn{ID: int64(30 - i), UserText: "recent", AssistantText: "answer"})
	}
	turns = append(turns,
		memory.SessionTurn{ID: 22, UserText: strings.Repeat("older ", 2000), AssistantText: "large"},
		memory.SessionTurn{ID: 21, UserText: "oldest", AssistantText: "oldest answer"},
	)
	base := AssemblePromptContext("policy", "profile", "current", nil, summary, 8, nil, 0, turns[:8], nil, 100000)
	recall := []memory.RecallResult{{Entry: memory.MemoryEntry{ID: 1, Scope: "long_term", Category: "projects", Statement: strings.Repeat("memory ", 300), Confidence: 1, Importance: 5}, Score: 1}}
	got := AssemblePromptContext("policy", "profile", "current", nil, summary, 8, recall, 4000, turns, nil, base.EstimatedAfter)
	if !got.SummaryIncluded || got.MinimumTailCount != 8 || got.SelectedTurnCount != 8 || got.SelectedRecallCount != 0 {
		t.Fatalf("summary/tail reservation failed: %+v", got)
	}
}

func TestAssemblePromptContextNeverLetsSummaryDisplaceMinimumTail(t *testing.T) {
	turns := []memory.SessionTurn{{ID: 2, UserText: "recent user", AssistantText: "recent assistant"}, {ID: 1, UserText: "older user", AssistantText: "older assistant"}}
	withoutSummary := AssemblePromptContext("policy", "profile", "current", nil, memory.SessionSummary{}, 2, nil, 0, turns, nil, 100000)
	hugeSummary := memory.SessionSummary{ID: 1, CoveredFromTurnID: 1, CoveredThroughTurnID: 20, Narrative: strings.Repeat("large summary ", 200)}
	got := AssemblePromptContext("policy", "profile", "current", nil, hugeSummary, 2, nil, 0, turns, nil, withoutSummary.EstimatedAfter)
	if got.SummaryIncluded || got.MinimumTailCount != 2 || got.SelectedTurnCount != 2 {
		t.Fatalf("summary displaced required tail: %+v", got)
	}
}

func roles(messages []llm.ChatMessage) string {
	values := make([]string, len(messages))
	for i, message := range messages {
		values[i] = message.Role
	}
	return strings.Join(values, ",")
}

func ids(turns []memory.SessionTurn) string {
	values := make([]string, len(turns))
	for i, turn := range turns {
		values[i] = string(rune('0' + turn.ID))
	}
	return strings.Join(values, ",")
}
