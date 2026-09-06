package agent

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

type foregroundCompactorCall struct {
	previous *usermemory.SessionSummary
	turns    []usermemory.SessionTurn
	limit    int
}

type fakeForegroundCompactor struct {
	artifact usermemory.SummaryArtifact
	err      error
	calls    []foregroundCompactorCall
	cancel   context.CancelFunc
}

func (f *fakeForegroundCompactor) CompactForeground(_ context.Context, previous *usermemory.SessionSummary, turns []usermemory.SessionTurn, limit int) (usermemory.SummaryArtifact, error) {
	call := foregroundCompactorCall{previous: previous, turns: append([]usermemory.SessionTurn(nil), turns...), limit: limit}
	f.calls = append(f.calls, call)
	if f.cancel != nil {
		f.cancel()
	}
	return f.artifact, f.err
}

func TestForegroundCompactionStateInstallsCheckpointAtomically(t *testing.T) {
	compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "Work completed so far."}}
	image := llm.InputImage{MimeType: "image/png", Data: "encoded", Source: "fixture"}
	status := make([]StreamChunk, 0, 1)
	state := newForegroundCompactionState(compactor, 100, "policy", "profile", "current request", []llm.InputImage{image}, nil, []usermemory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, func(chunk StreamChunk) {
		status = append(status, chunk)
	})
	original := []llm.ChatMessage{
		{Role: "system", Content: "policy"},
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "current request", Images: []llm.InputImage{image}},
	}

	rebuilt, stats, err := state.prepare(context.Background(), original, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Compacted || stats.DebtCount != 1 || len(compactor.calls) != 1 {
		t.Fatalf("stats=%+v calls=%+v", stats, compactor.calls)
	}
	if len(status) != 1 || status[0].Type != ChunkStatus || status[0].Text != foregroundCompactionStatus {
		t.Fatalf("status=%+v", status)
	}
	if len(rebuilt) != 4 || rebuilt[0].Content != "policy" || rebuilt[1].Content != "profile" || !strings.Contains(rebuilt[2].Content, "active_turn_summary") || rebuilt[3].Content != "current request" {
		t.Fatalf("rebuilt=%+v", rebuilt)
	}
	if len(rebuilt[3].Images) != 1 || rebuilt[3].Images[0].Data != image.Data || messagesContain(rebuilt, "old") {
		t.Fatalf("current image or replaced history is wrong: %+v", rebuilt)
	}
	if state.hasDebt() {
		t.Fatal("compacted debt was retained")
	}

	failing := &fakeForegroundCompactor{err: errors.New("provider unavailable")}
	state = newForegroundCompactionState(failing, 100, "policy", "", "current request", nil, nil, []usermemory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	got, _, err := state.prepare(context.Background(), original, nil, true)
	if err == nil || len(got) != len(original) || !state.hasDebt() || state.hasCheckpoint {
		t.Fatalf("failed compaction mutated state: messages=%+v debt=%t checkpoint=%t err=%v", got, state.hasDebt(), state.hasCheckpoint, err)
	}
}

func TestForegroundCompactionStateTriggersAtSeventyPercent(t *testing.T) {
	messages := []llm.ChatMessage{{Role: "system", Content: "policy"}, {Role: "user", Content: strings.Repeat("request ", 40)}}
	estimated := promptbudget.EstimateRequest(messages, nil)
	belowLimit := estimated*100/foregroundCompactionPercent + 1
	below := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "checkpoint"}}
	state := newForegroundCompactionState(below, belowLimit, "policy", "", messages[1].Content, nil, nil, []usermemory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	if _, stats, err := state.prepare(context.Background(), messages, nil, false); err != nil || stats.Compacted || len(below.calls) != 0 {
		t.Fatalf("below threshold compacted: stats=%+v calls=%d err=%v", stats, len(below.calls), err)
	}

	atLimit := estimated * 100 / foregroundCompactionPercent
	at := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "checkpoint"}}
	state = newForegroundCompactionState(at, atLimit, "policy", "", messages[1].Content, nil, nil, []usermemory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	if _, stats, err := state.prepare(context.Background(), messages, nil, false); err != nil || !stats.Compacted || len(at.calls) != 1 {
		t.Fatalf("threshold did not compact: stats=%+v calls=%d err=%v", stats, len(at.calls), err)
	}
}

func TestProcessCompactsCompletedToolRoundAndContinues(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("large-call", "test.large", map[string]interface{}{"query": "details"}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "finished after compaction"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterTool(registry.Spec{Name: "test.large", Description: "Return a large result", Schema: &llm.ToolParameters{Type: "object"}}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return productiveResult(strings.Repeat("result ", 1000)), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	agent.budget.PromptLimit = 1000
	compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "The large lookup completed."}}
	agent.SetForegroundCompactor(compactor)
	var chunks []StreamChunk

	response, err := processAgent(agent, "compact-tool", "homeassistant", "session", "user-1", "User", "research this", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Response != "finished after compaction" || len(compactor.calls) != 1 || len(compactor.calls[0].turns) != 1 {
		t.Fatalf("response=%+v compactor_calls=%+v", response, compactor.calls)
	}
	batch := compactor.calls[0].turns[0].ToolHistory.Batches
	if len(batch) != 1 || len(batch[0].Calls) != 1 || batch[0].Calls[0].Name != "test.large" || batch[0].Calls[0].Result == "" {
		t.Fatalf("foreground debt did not contain the complete tool round: %+v", compactor.calls[0].turns)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 2 || !requestHasTool(requests[1], "test.large") || !messagesContain(requests[1].Messages, "active_turn_summary") || messagesContain(requests[1].Messages, strings.Repeat("result ", 20)) {
		t.Fatalf("second request did not use the compacted epoch: %+v", requests)
	}
	if !hasCompactionStatus(chunks) {
		t.Fatalf("compaction status was not streamed: %+v", chunks)
	}
}

func TestProcessRecoversProviderContextOverflowWithTransientCheckpoint(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: &llm.ChatHTTPError{StatusCode: 400, Body: "maximum context length exceeded"}},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "recovered"}}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session", "user-1", profile.Generation, "historical question", "historical answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "Historical work summarized."}}
	agent.SetForegroundCompactor(compactor)
	var chunks []StreamChunk

	response, err := processAgent(agent, "compact-overflow", "homeassistant", "session", "user-1", "User", "continue exactly", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Response != "recovered" || len(compactor.calls) != 1 || len(compactor.calls[0].turns) != 1 {
		t.Fatalf("response=%+v compactor_calls=%+v", response, compactor.calls)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 2 || !messagesContain(requests[0].Messages, "historical question") || messagesContain(requests[1].Messages, "historical question") || !messagesContain(requests[1].Messages, "active_turn_summary") {
		t.Fatalf("provider recovery requests=%+v", requests)
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != "user" || last.Content != "continue exactly" || !hasCompactionStatus(chunks) {
		t.Fatalf("current request or status changed: last=%+v chunks=%+v", last, chunks)
	}
	if summary, err := store.LatestSessionSummary(context.Background(), "user-1", "session", profile.Generation); !errors.Is(err, sql.ErrNoRows) || summary.ID != 0 {
		t.Fatalf("foreground checkpoint was persisted: summary=%+v err=%v", summary, err)
	}
}

func TestProcessPropagatesCancellationDuringForegroundCompaction(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: &llm.ChatHTTPError{StatusCode: 400, Body: "context length exceeded"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session", "user-1", profile.Generation, strings.Repeat("historical context ", 100), "answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	agent.SetForegroundCompactor(&fakeForegroundCompactor{err: context.Canceled, cancel: cancel})

	response, err := agent.Process(ctx, Request{
		RequestID: "compact-canceled", SessionKey: "session", Prompt: "continue",
		Principal: identity.Principal{CanonicalUserID: "user-1", Gateway: "homeassistant", ExternalID: "user-1", Assurance: identity.AssuranceHomeAssistantToken},
	})
	if !errors.Is(err, context.Canceled) || response != nil || len(chat.requests) != 1 {
		t.Fatalf("response=%+v err=%v provider_calls=%d", response, err, len(chat.requests))
	}
}

func TestProcessRecoversProviderOverflowOnGovernanceFinalCall(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{response: toolCallResponse("lookup", "test.lookup", nil)},
		{err: &llm.ChatHTTPError{StatusCode: 400, Body: "context window exceeded"}},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "final after recovery"}}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := reg.RegisterTool(registry.Spec{Name: "test.lookup", Description: "Look up data", Schema: &llm.ToolParameters{Type: "object"}}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return productiveResult("lookup complete"), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	agent.toolPolicy.MaxExecutions = 1
	compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "The lookup completed."}}
	agent.SetForegroundCompactor(compactor)

	response, err := processAgent(agent, "compact-final", "homeassistant", "session", "user-1", "User", "look this up", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Response != "final after recovery" || len(compactor.calls) != 1 || len(primaryRequests(chat.requests)) != 3 {
		t.Fatalf("response=%+v compactor_calls=%+v provider_calls=%d", response, compactor.calls, len(chat.requests))
	}
}

func TestProcessRecoversProviderOverflowOnEmptyResponseRetry(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant"}}},
		{err: &llm.ChatHTTPError{StatusCode: 400, Body: "too many tokens"}},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "answer after recovery"}}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session", "user-1", profile.Generation, "historical question", "historical answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "Prior conversation summarized."}}
	agent.SetForegroundCompactor(compactor)

	response, err := processAgent(agent, "compact-empty", "homeassistant", "session", "user-1", "User", "answer this", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if response.Response != "answer after recovery" || len(compactor.calls) != 1 || len(requests) != 3 {
		t.Fatalf("response=%+v compactor_calls=%+v provider_calls=%d", response, compactor.calls, len(requests))
	}
	last := requests[2].Messages
	if len(last) < 2 || last[len(last)-1].Content != emptyResponseRetryPrompt || last[len(last)-2].Content != "answer this" || !messagesContain(last, "active_turn_summary") {
		t.Fatalf("recovered empty-response request=%+v", last)
	}
}

func hasCompactionStatus(chunks []StreamChunk) bool {
	for _, chunk := range chunks {
		if chunk.Type == ChunkStatus && chunk.Text == foregroundCompactionStatus {
			return true
		}
	}
	return false
}
