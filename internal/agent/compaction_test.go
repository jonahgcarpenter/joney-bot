package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/testutil"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
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
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.large", Description: "Return a large result", Schema: &llm.ToolParameters{Type: "object"}}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
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

func TestProcessCompactsDeliveredHistoryAcrossPendingGapAndPages(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: &llm.ChatHTTPError{StatusCode: 400, Body: "context length exceeded"}},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "continued"}}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	profile, err := store.ResolveSessionProfile(ctx, "user-1", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= foregroundDebtPageSize; i++ {
		if i == 1 {
			if _, err := testutil.AppendPendingTurn(ctx, store.Store, "session", "user-1", profile.Generation, "pending", "pending answer", nil, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.AppendSessionTurnForGeneration(ctx, "session", "user-1", profile.Generation, "delivered", "answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "Delivered conversation summarized."}}
	agent.SetForegroundCompactor(compactor)
	response, err := processAgent(agent, "gap-pages", "homeassistant", "session", "user-1", "User", "continue", nil, nil)
	if err != nil || response.Response != "continued" || len(compactor.calls) != 1 {
		t.Fatalf("response=%+v err=%v calls=%d", response, err, len(compactor.calls))
	}
	turns := compactor.calls[0].turns
	if len(turns) != foregroundDebtPageSize+1 {
		t.Fatalf("compacted %d turns, want %d", len(turns), foregroundDebtPageSize+1)
	}
	for i, turn := range turns {
		if turn.UserText != "delivered" || (i > 0 && turn.ID <= turns[i-1].ID) {
			t.Fatalf("unexpected compacted turn %d: %+v", i, turn)
		}
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
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Look up data", Schema: &llm.ToolParameters{Type: "object"}}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
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

func TestProcessInitialCompactionUsesPressureBeforeHistoryOmission(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "continued"}}}}
			a, store := newTestAgent(t, chat, nil, nil)
			a.budget.PromptLimit = 1000
			profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AppendSessionTurnForGeneration(context.Background(), "session", "user-1", profile.Generation, strings.Repeat("oversized history ", 2000), "answer", nil, time.Hour); err != nil {
				t.Fatal(err)
			}
			compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "Prior context."}}
			if fail {
				compactor.err = errors.New("compaction failed")
			}
			a.SetForegroundCompactor(compactor)
			response, err := processAgent(a, "initial-pressure", "discord", "session", "user-1", "User", "continue", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(compactor.calls) != 1 || len(compactor.calls[0].turns) != 1 {
				t.Fatalf("calls=%+v", compactor.calls)
			}
			if fail {
				if response.Response != contextCompactionFallback || response.SourceTurnID == 0 || len(chat.requests) != 0 {
					t.Fatalf("response=%+v requests=%d", response, len(chat.requests))
				}
			} else if len(chat.requests) != 1 || !messagesContain(chat.requests[0].Messages, "active_turn_summary") {
				t.Fatalf("requests=%+v", chat.requests)
			}
		})
	}
}

func TestProcessForegroundEvidenceAndFallbackPersistence(t *testing.T) {
	for _, mode := range []string{"success", "preflight", "governance", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			liveResult := strings.Repeat("private fetched evidence ", 1000)
			liveURL := "https://example.com/" + strings.Repeat("private-path", 1000)
			first := toolCallResponse("fetch", "test.fetch", map[string]interface{}{"url": liveURL})
			first.Message.Thinking = "private reasoning"
			first.Message.ToolCalls = append(first.Message.ToolCalls, llm.ToolCall{ID: "stage", Function: llm.ToolFunction{Name: toolnames.UserMemorySave, Arguments: map[string]interface{}{}}})
			chat := &fakeChatter{outcomes: []fakeChatOutcome{{response: first}, {response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "continued"}}}}}
			if mode == "overflow" {
				chat.outcomes[1] = fakeChatOutcome{err: &llm.ChatHTTPError{StatusCode: 400, Body: "context length exceeded"}}
			}
			reg := registry.New(config.NewLogger(config.LevelError))
			policy := testToolPolicy()
			policy.History = governance.HistoryPolicy{Mode: governance.HistoryMetadata}
			if err := registerTestTool(t, reg, registry.Spec{Name: "test.fetch", Description: "Fetch-like evidence"}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
				return governance.Result{Content: liveResult, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "result.png", MIMEType: "image/png", Data: []byte("private attachment bytes")}}}, nil
			}); err != nil {
				t.Fatal(err)
			}
			a, store := newTestAgent(t, chat, nil, reg)
			registerStagingTool(t, reg, store.Store, false)
			a.budget.PromptLimit = 1000
			if mode == "overflow" {
				a.budget.PromptLimit = 100000
			}
			if mode == "governance" {
				a.toolPolicy.MaxExecutions = 2
			}
			compactor := &fakeForegroundCompactor{artifact: usermemory.SummaryArtifact{Narrative: "Transient checkpoint."}}
			if mode != "success" {
				compactor.err = errors.New("compaction failed")
			}
			a.SetForegroundCompactor(compactor)
			response, err := processAgent(a, "evidence-"+mode, "discord", "session", "user-1", "User", "I prefer dark mode.", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if response.SourceTurnID == 0 || response.SessionGeneration == 0 || len(response.Attachments) != 1 {
				t.Fatalf("response=%+v", response)
			}
			if mode != "success" && response.Response != contextCompactionFallback {
				t.Fatalf("response=%+v", response)
			}
			if len(compactor.calls) != 1 {
				t.Fatalf("compactor calls=%d", len(compactor.calls))
			}
			call := compactor.calls[0].turns[0].ToolHistory.Batches[0].Calls[0]
			if call.Arguments["url"] != liveURL || call.Result != liveResult || call.ArgumentsTruncated || call.ResultTruncated {
				t.Fatal("live evidence was redacted or truncated")
			}
			encoded, _ := json.Marshal(compactor.calls[0].turns)
			if strings.Contains(string(encoded), "private reasoning") || strings.Contains(string(encoded), "private attachment bytes") {
				t.Fatal("compactor received reasoning or attachment bytes")
			}
			artifact, err := store.SessionTurnForegroundMemory(context.Background(), "user-1", response.SourceTurnID)
			if err != nil || len(artifact.Candidates) != 1 {
				t.Fatalf("artifact=%+v err=%v", artifact, err)
			}
			turns, err := store.RecentSessionTurns("user-1", "session", 1, 1)
			if err != nil || len(turns) != 1 || turns[0].AssistantText != response.Response || len(turns[0].ToolHistory.Batches) != 1 || len(turns[0].ToolHistory.Batches[0].Calls) != 2 {
				t.Fatalf("turns=%+v err=%v", turns, err)
			}
			encoded, _ = json.Marshal(turns)
			for _, private := range []string{"private fetched evidence", "private-path", "private reasoning", "private attachment bytes", "Transient checkpoint."} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("persisted private evidence %q", private)
				}
			}
		})
	}
}

func TestProcessRetriesWhitespaceOnlyVisibleResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			chat := &fakeChatter{responses: []*llm.ChatResponse{
				{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: " \n\t "}},
				{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "visible answer"}},
			}}
			a, _ := newTestAgent(t, chat, nil, nil)
			var callback func(StreamChunk)
			if stream {
				callback = func(StreamChunk) {}
			}
			response, err := processAgent(a, "whitespace", "discord", "session", "user-1", "User", "answer", nil, callback)
			if err != nil || response.Response != "visible answer" || len(chat.requests) != 2 {
				t.Fatalf("response=%+v err=%v requests=%d", response, err, len(chat.requests))
			}
			last := chat.requests[1]
			if len(last.Tools) != 0 || last.Messages[len(last.Messages)-1].Content != emptyResponseRetryPrompt {
				t.Fatalf("retry=%+v", last)
			}
		})
	}
}

func TestForegroundToolEvidenceDoesNotUseHistoryBounds(t *testing.T) {
	tc := llm.ToolCall{Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"query": strings.Repeat("argument", 100)}}}
	content := strings.Repeat("result", 100)
	decision := governance.Decision{Allowed: true}
	result := productiveResult(content)
	live := foregroundToolCall(tc, decision, result, nil, content, time.Now())
	policy := governance.HistoryPolicy{Mode: governance.HistoryFull, MaxArgumentBytes: 32, MaxResultRunes: 32}
	stored := persistedToolCall(tc, policy, decision, result, nil, content, time.Now())
	if !stored.ArgumentsTruncated || !stored.ResultTruncated {
		t.Fatalf("stored history did not enforce bounds: %+v", stored)
	}
	if live.Arguments["query"] != tc.Function.Arguments["query"] || live.Result != content || live.ArgumentsTruncated || live.ResultTruncated {
		t.Fatalf("live evidence inherited storage bounds: %+v", live)
	}
}
