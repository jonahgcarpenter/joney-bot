package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/indexing"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestProcessRejectsUnauthenticatedPrincipal(t *testing.T) {
	principal := identity.Principal{
		CanonicalUserID: "user-1",
		Gateway:         "homeassistant",
		ExternalID:      "user-1",
		Assurance:       identity.AssuranceSelfAsserted,
	}
	response, err := (&Agent{}).Process(context.Background(), Request{Principal: principal})
	if err == nil || response != nil || !strings.Contains(err.Error(), "authenticated principal") {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestProcessReturnsBeforeProviderCallWhenCanceled(t *testing.T) {
	chat := &fakeChatter{}
	agent, _ := newTestAgent(t, chat, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response, err := agent.Process(ctx, Request{
		RequestID: "canceled", SessionKey: "session", Prompt: "hello",
		Principal: identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken},
	})
	if !errors.Is(err, context.Canceled) || response != nil {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if len(chat.requests) != 0 {
		t.Fatalf("provider received %d requests", len(chat.requests))
	}
}

func TestProcessPropagatesCancellationDuringProviderCallWithoutPersistence(t *testing.T) {
	chat := &cancelingChatter{started: make(chan struct{})}
	agent, store := newTestAgent(t, chat, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		response *Response
		err      error
	}, 1)
	go func() {
		response, err := agent.Process(ctx, Request{
			RequestID: "canceled", SessionKey: "session", Prompt: "hello",
			Principal: identity.Principal{CanonicalUserID: "user-1", Gateway: "homeassistant", ExternalID: "user-1", Assurance: identity.AssuranceHomeAssistantToken},
		})
		done <- struct {
			response *Response
			err      error
		}{response: response, err: err}
	}()
	<-chat.started
	cancel()
	result := <-done
	if !errors.Is(result.err, context.Canceled) || result.response != nil {
		t.Fatalf("response=%+v err=%v", result.response, result.err)
	}
	turns, err := store.RecentSessionTurns("user-1", "session", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("persisted canceled turns: %+v", turns)
	}
}

type cancelingChatter struct{ started chan struct{} }

func (c *cancelingChatter) Chat(ctx context.Context, _ llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	close(c.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestProcessFinalAnswerPersistsCleanedSessionMemory(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "final answer"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "[Replying to Alice: \"old\"]\n\nnew prompt", []llm.InputImage{testInputImage(t, 800, 600)}, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "final answer" {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one primary chat call, got %d", len(primary))
	}
	lastMessage := primary[0].Messages[len(primary[0].Messages)-1]
	if len(lastMessage.Images) != 1 {
		t.Fatalf("expected current-turn image in prompt, got %+v", lastMessage.Images)
	}

	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected one persisted turn, got %d", len(turns))
	}
	wantUser := "new prompt\n\n[Attached 1 image(s)]"
	if turns[0].UserText != wantUser || turns[0].AssistantText != "final answer" {
		t.Fatalf("unexpected stored turn: %+v", turns[0])
	}
}

func TestProcessPersistsStagedForegroundMemoryWithFinalTurn(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "private dark mode candidate", ToolCalls: []llm.ToolCall{{ID: "stage", Function: llm.ToolFunction{Name: toolnames.UserMemorySave, Arguments: map[string]interface{}{}}}}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "I will remember that."}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	agent, store := newTestAgent(t, chat, nil, reg)
	registerStagingTool(t, reg, store.Store, false)
	response, err := processAgent(agent, "staged", "homeassistant", "session", "user-1", "User", "I prefer dark mode.", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.SessionTurnForegroundMemory(context.Background(), "user-1", response.SourceTurnID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Candidates) != 1 || artifact.Candidates[0].ClaimValue != "dark_mode" {
		t.Fatalf("artifact=%+v", artifact)
	}
	turns, err := store.RecentSessionTurns("user-1", "session", 1, 1)
	if err != nil || len(turns) != 1 || len(turns[0].ToolHistory.Batches) != 1 {
		t.Fatalf("turns=%+v err=%v", turns, err)
	}
	encodedHistory, _ := json.Marshal(turns[0].ToolHistory)
	if bytes.Contains(encodedHistory, []byte("dark mode")) || bytes.Contains(encodedHistory, []byte("private dark mode candidate")) {
		t.Fatalf("metadata-only tool history retained staged candidate: %s", encodedHistory)
	}
}

func TestProcessDoesNotSilentlySucceedWhenStagedTurnIsNotPersisted(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "stage", Function: llm.ToolFunction{Name: toolnames.UserMemorySave, Arguments: map[string]interface{}{}}}}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "I will remember that."}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	agent, store := newTestAgent(t, chat, nil, reg)
	registerStagingTool(t, reg, store.Store, true)
	usage := requestctx.NewUsageCollector()
	response, err := agent.Process(requestctx.WithUsageCollector(context.Background(), usage), Request{RequestID: "staged-failure", SessionKey: "session", Prompt: "I prefer dark mode.", Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "user-1", Gateway: "homeassistant", Assurance: identity.AssuranceHomeAssistantToken}})
	if err == nil || response != nil || !strings.Contains(err.Error(), "session turn was not stored") {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	e := usage.ExecutionSnapshot()
	if !e.IsComplete || e.PersistenceStatus != "failed" || e.ToolExecutionCount != 1 || e.ResponseKind != "error" {
		t.Fatalf("lost failure execution state: %+v", e)
	}
}

func TestMemoryToolStreamPayloadsUseScopeExplicitKeys(t *testing.T) {
	userPayload := toolStreamPayload(toolnames.UserMemorySearch, map[string]interface{}{"query": "reply style"}, "No memories found.", time.Millisecond, false)
	if userPayload.UserMemory == nil || userPayload.UserMemory.Action != "search" || userPayload.GlobalMemory != nil {
		t.Fatalf("unexpected user memory payload: %+v", userPayload)
	}
	globalPayload := toolStreamPayload(toolnames.GlobalMemorySearch, map[string]interface{}{"query": "implementation language"}, "search result", time.Millisecond, false)
	if globalPayload.GlobalMemory == nil || globalPayload.GlobalMemory.Action != "search" || globalPayload.GlobalMemory.Query != "implementation language" || globalPayload.UserMemory != nil {
		t.Fatalf("unexpected global memory payload: %+v", globalPayload)
	}

	for _, payload := range []*ToolStreamPayload{userPayload, globalPayload} {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"memory":`) {
			t.Fatalf("legacy memory stream key was emitted: %s", encoded)
		}
	}
}

func TestUserMemorySaveStreamPayloadOmitsCandidateContent(t *testing.T) {
	payload := toolStreamPayload(toolnames.UserMemorySave, map[string]interface{}{
		"memories": []interface{}{map[string]interface{}{"statement": "private statement", "evidence": "private evidence"}},
	}, `{"status":"staged","message":"private result"}`, time.Millisecond, false)
	if payload.Arguments != nil || payload.ResultText != "" || payload.UserMemory == nil || payload.UserMemory.Action != "save" {
		t.Fatalf("unexpected save stream payload: %+v", payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private statement", "private evidence", "private result"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("save stream exposed %q: %s", private, encoded)
		}
	}
}

func TestWebSearchToolStreamPayloadDecodesStructuredResults(t *testing.T) {
	raw := `{"notice":"untrusted","degraded":true,"unresponsive_engines":["seznam"],"results":[{"title":"Result","url":"https://example.com/page","domain":"example.com","snippet":"Snippet","engines":["yandex"],"published_at":"2026-08-28","score":2}]}`
	payload := toolStreamPayload("web.search", map[string]interface{}{"query": " test query "}, raw, time.Millisecond, false)
	if payload.WebSearch == nil || payload.WebSearch.Query != "test query" || !payload.WebSearch.IsDegraded {
		t.Fatalf("unexpected web search payload: %+v", payload)
	}
	if strings.Join(payload.WebSearch.UnresponsiveEngines, ",") != "seznam" || len(payload.WebSearch.Results) != 1 {
		t.Fatalf("missing web search degradation/results: %+v", payload.WebSearch)
	}
	result := payload.WebSearch.Results[0]
	if result.Title != "Result" || result.Domain != "example.com" || result.Content != "Snippet" || result.PublishedAt != "2026-08-28" || result.Score != 2 || strings.Join(result.Engines, ",") != "yandex" {
		t.Fatalf("unexpected streamed result: %+v", result)
	}

	malformed := toolStreamPayload("web.search", map[string]interface{}{"query": "test"}, "not-json", time.Millisecond, false)
	if malformed.WebSearch == nil || len(malformed.WebSearch.Results) != 0 {
		t.Fatalf("malformed result exposed structured data: %+v", malformed)
	}
}

func TestWebFetchToolStreamPayloadOmitsURLAndContent(t *testing.T) {
	raw := `{"notice":"untrusted","url":"https://example.com/private-path","title":"Example","content_type":"text/html","source":"direct","content":"private fetched content","is_truncated":true,"is_degraded":true}`
	payload := toolStreamPayload("web.fetch", map[string]interface{}{"url": "https://example.com/private-path"}, raw, time.Millisecond, false)
	if payload.WebFetch == nil || payload.WebFetch.Title != "Example" || payload.WebFetch.ContentType != "text/html" || payload.WebFetch.Source != "direct" || !payload.WebFetch.IsTruncated || !payload.WebFetch.IsDegraded {
		t.Fatalf("unexpected web fetch payload: %+v", payload)
	}
	if payload.Arguments != nil || payload.ResultText != "" {
		t.Fatalf("web fetch stream retained private data: %+v", payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-path") || strings.Contains(string(encoded), "private fetched content") {
		t.Fatalf("web fetch stream exposed URL or content: %s", encoded)
	}

	malformed := toolStreamPayload("web.fetch", map[string]interface{}{"url": "https://example.com/secret"}, "not-json", time.Millisecond, false)
	if malformed.WebFetch == nil || malformed.Arguments != nil || malformed.ResultText != "" {
		t.Fatalf("malformed fetch result exposed data: %+v", malformed)
	}
}

func TestPersistedMetadataToolCallOmitsArgumentsAndResult(t *testing.T) {
	tc := llm.ToolCall{Function: llm.ToolFunction{Name: "web.fetch", Arguments: map[string]interface{}{"url": "https://example.com/private"}}}
	policy := governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false}.Effective()
	call := persistedToolCall(tc, policy, governance.Decision{Allowed: true}, governance.Result{Outcome: governance.OutcomeProductive}, nil, "private page content", time.Now())
	if call.HistoryMode != string(governance.HistoryMetadata) || len(call.Arguments) != 0 || call.Result != "Historical tool result omitted by policy." || !call.ArgumentsTruncated || !call.ResultTruncated || call.SearchResult {
		t.Fatalf("metadata history retained tool data: %+v", call)
	}
}

func TestComfyUIToolStreamPayloadOmitsPromptsAndResults(t *testing.T) {
	payload := toolStreamPayload(toolnames.ComfyUITextToImage, map[string]interface{}{"prompt": "private prompt", "negative_prompt": "private negative"}, `{"status":"generated"}`, time.Millisecond, false)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Arguments != nil || payload.ResultText != "" || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "generated") {
		t.Fatalf("ComfyUI stream exposed private data: %s", encoded)
	}
}

func TestProcessPropagatesToolAttachmentsWithoutPersistingBytes(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "image-call", Function: llm.ToolFunction{Name: "test.image", Arguments: map[string]interface{}{"prompt": "private prompt"}}}}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "attached"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	privateBytes := []byte("private-image-bytes")
	policy := testToolPolicy()
	policy.History = governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false}
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.image", Description: "Image"}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: `{"attachment_count":1}`, Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "generated.png", MIMEType: "image/png", Data: privateBytes}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	a, store := newTestAgent(t, chat, nil, reg)
	var chunks []StreamChunk
	response, err := processAgent(a, "attachment", "discord", "session", "user-1", "Display", "draw", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Attachments) != 1 || !bytes.Equal(response.Attachments[0].Data, privateBytes) {
		t.Fatalf("attachments were not propagated: %+v", response.Attachments)
	}
	foundStreamAttachment := false
	for _, chunk := range chunks {
		if chunk.Type != ChunkToolResult || len(chunk.Attachments) == 0 {
			continue
		}
		foundStreamAttachment = bytes.Equal(chunk.Attachments[0].Data, privateBytes)
		encodedChunk, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encodedChunk, privateBytes) || bytes.Contains(encodedChunk, []byte(base64.StdEncoding.EncodeToString(privateBytes))) {
			t.Fatalf("serialized stream chunk retained attachment bytes: %s", encodedChunk)
		}
	}
	if !foundStreamAttachment {
		t.Fatalf("tool result stream did not carry the generated attachment: %+v", chunks)
	}
	turns, err := store.RecentSessionTurns("user-1", "session", 1, 1)
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns=%+v err=%v", turns, err)
	}
	encoded, err := json.Marshal(turns[0].ToolHistory)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, privateBytes) || bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(privateBytes))) || bytes.Contains(encoded, []byte("private prompt")) {
		t.Fatalf("tool history retained private attachment data: %s", encoded)
	}
}

func TestProcessExecutesToolThenFinalAnswerAndStreamsEvents(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "thinking", ToolCalls: []llm.ToolCall{{ID: "call-1", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "oswald"}}}}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "tool-backed answer"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Lookup", Parameters: []registry.ParamSpec{{Name: "q", Type: "string", Required: true}}}, testToolPolicy(), func(_ context.Context, args map[string]interface{}) (governance.Result, error) {
		if args["q"] != "oswald" {
			t.Fatalf("unexpected tool args: %+v", args)
		}
		return productiveResult("lookup result"), nil
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, store := newTestAgent(t, chat, nil, reg)

	var chunks []StreamChunk
	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "tool-backed answer" || resp.Thinking != "thinking" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected two primary chat calls, got %d", len(primary))
	}
	secondMessages := primary[1].Messages
	toolMsg := secondMessages[len(secondMessages)-1]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call-1" || toolMsg.Content != "lookup result" {
		t.Fatalf("unexpected tool message: %+v", toolMsg)
	}
	toolCallIndex := -1
	toolResultIndex := -1
	for i, chunk := range chunks {
		if chunk.Type == ChunkToolCall {
			toolCallIndex = i
		}
		if chunk.Type == ChunkToolResult {
			toolResultIndex = i
		}
	}
	if toolCallIndex < 0 || toolResultIndex < 0 || toolResultIndex <= toolCallIndex || chunks[toolResultIndex].Tool.ResultText != "lookup result" {
		t.Fatalf("unexpected stream chunks: %+v", chunks)
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || strings.Join(turns[0].ToolNames, ",") != "test.lookup" {
		t.Fatalf("successful tool annotation was not persisted: %+v", turns)
	}
	if len(turns[0].ToolHistory.Batches) != 1 || len(turns[0].ToolHistory.Batches[0].Calls) != 1 {
		t.Fatalf("native tool history was not persisted: %+v", turns[0].ToolHistory)
	}
	storedCall := turns[0].ToolHistory.Batches[0].Calls[0]
	if storedCall.Name != "test.lookup" || storedCall.Result != "lookup result" || storedCall.Arguments["q"] != "oswald" || storedCall.Status != "succeeded" {
		t.Fatalf("stored tool call = %+v", storedCall)
	}
}

func TestProcessOffersRetrievalOnlyMemoryTools(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := builtin.Register(reg, &config.Config{SearxngURL: "http://localhost:8080"}, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	if _, err := processAgent(agent, "retrieval-only", "homeassistant", "session", "user-1", "Display", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}

	request := primaryRequests(chat.requests)[0]
	for _, name := range []string{toolnames.UserMemorySearch, toolnames.UserMemoryList, toolnames.SessionTranscriptSearch, toolnames.GlobalMemorySearch} {
		if !requestHasTool(request, name) {
			t.Fatalf("expected tool missing from primary request: %s", name)
		}
	}
}

func TestProcessHidesComfyUIToolsOnlyForHomeAssistant(t *testing.T) {
	for _, test := range []struct {
		name      string
		gateway   string
		images    []llm.InputImage
		wantText  bool
		wantImage bool
	}{
		{name: "discord text only", gateway: "discord", wantText: true, wantImage: true},
		{name: "discord with image", gateway: "discord", images: []llm.InputImage{testInputImage(t, 2, 2)}, wantText: true, wantImage: true},
		{name: "home assistant", gateway: "homeassistant", images: []llm.InputImage{testInputImage(t, 2, 2)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
			reg := registry.New(config.NewLogger(config.LevelError))
			for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
				if err := registerTestTool(t, reg, registry.Spec{Name: name, Description: name}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
					return productiveResult("unused"), nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			a, _ := newTestAgent(t, chat, nil, reg)
			if _, err := processAgent(a, "visibility", test.gateway, "session", "user-1", "Display", "hello", test.images, nil); err != nil {
				t.Fatal(err)
			}
			request := primaryRequests(chat.requests)[0]
			if requestHasTool(request, toolnames.ComfyUITextToImage) != test.wantText || requestHasTool(request, toolnames.ComfyUIImageToImage) != test.wantImage {
				t.Fatalf("tools=%+v want text=%t image=%t", request.Tools, test.wantText, test.wantImage)
			}
		})
	}
}

func TestProcessDisablesToolsAfterIterationBudget(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call-1", Function: llm.ToolFunction{Name: "test.fail"}},
			{ID: "call-2", Function: llm.ToolFunction{Name: "test.fail"}},
		}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "finished without tools"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.fail", Description: "Fail"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{}, errors.New("boom")
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	agent.toolPolicy.MaxToolIterations = 1

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "finished without tools" || resp.Kind != "tool_limit" {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected final no-tools call, got %d calls", len(primary))
	}
	if len(primary[1].Tools) != 0 {
		t.Fatalf("expected tools disabled, got %+v", primary[1].Tools)
	}
	for _, id := range []string{"call-1", "call-2"} {
		if result := toolResultByID(primary[1].Messages, id); result == nil || !strings.Contains(result.Content, "boom") {
			t.Fatalf("failed call %s missing correlated result: %+v", id, primary[1].Messages)
		}
	}
}

func TestProcessBlocksExactDuplicateButAllowsDistinctCall(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("first", "test.lookup", map[string]interface{}{"q": "same"}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "duplicate", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "same"}}},
			{ID: "distinct", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "different"}}},
		}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	var invocations []string
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(_ context.Context, args map[string]interface{}) (governance.Result, error) {
		invocations = append(invocations, args["q"].(string))
		return productiveResult("result " + args["q"].(string)), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)

	if _, err := processAgent(agent, "duplicate", "homeassistant", "session", "user-1", "User", "lookup", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(invocations, ","); got != "same,different" {
		t.Fatalf("handler invocations = %q, want same,different", got)
	}
	requests := primaryRequests(chat.requests)
	duplicateResult := toolResultByID(requests[2].Messages, "duplicate")
	if duplicateResult == nil || !strings.Contains(duplicateResult.Content, "same tool and arguments") {
		t.Fatalf("duplicate call was not blocked with a matching result: %+v", requests[2].Messages)
	}
	if distinctResult := toolResultByID(requests[2].Messages, "distinct"); distinctResult == nil || distinctResult.Content != "result different" {
		t.Fatalf("distinct call did not execute: %+v", requests[2].Messages)
	}
}

func TestProcessAllowsExactRetryAfterToolFailure(t *testing.T) {
	args := map[string]interface{}{"q": "same"}
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("first", "test.lookup", args),
		toolCallResponse("retry", "test.lookup", args),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	invocations := 0
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		invocations++
		if invocations == 1 {
			return governance.Result{}, errors.New("temporary failure")
		}
		return productiveResult("recovered"), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)

	if _, err := processAgent(agent, "retry", "homeassistant", "session", "user-1", "User", "lookup", nil, nil); err != nil {
		t.Fatal(err)
	}
	if invocations != 2 {
		t.Fatalf("handler invocation count = %d, want 2", invocations)
	}
	requests := primaryRequests(chat.requests)
	if result := toolResultByID(requests[2].Messages, "retry"); result == nil || result.Content != "recovered" {
		t.Fatalf("retry result missing: %+v", requests[2].Messages)
	}
}

func TestProcessRetiresOnlyUnproductiveTool(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("stale", "test.stale", nil),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	policy := testToolPolicy()
	policy.MaxUnproductive = 1
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.stale", Description: "Stale"}, policy, func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: "nothing useful", Outcome: governance.OutcomeUnproductive}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.useful", Description: "Useful"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return productiveResult("useful"), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)

	if _, err := processAgent(agent, "retire", "homeassistant", "session", "user-1", "User", "search", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if requestHasTool(requests[1], "test.stale") || !requestHasTool(requests[1], "test.useful") {
		t.Fatalf("per-tool retirement changed the wrong catalog: %+v", toolNames(requests[1]))
	}
}

func TestProcessGlobalCapMidBatchEmitsResultForEveryCall(t *testing.T) {
	declared := []llm.ToolCall{
		{ID: "one", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "one"}}},
		{ID: "two", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "two"}}},
		{ID: "three", Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "three"}}},
	}
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: declared}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "finished"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	invocations := 0
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		invocations++
		return productiveResult("first result"), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)
	agent.toolPolicy.MaxExecutions = 1

	response, err := processAgent(agent, "cap", "homeassistant", "session", "user-1", "User", "lookup", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Kind != "tool_limit" || response.Response != "finished" {
		t.Fatalf("ceiling response = %+v", response)
	}
	requests := primaryRequests(chat.requests)
	if invocations != 1 {
		t.Fatalf("handler invocation count = %d, want 1", invocations)
	}
	if len(requests) != 2 || len(requests[1].Tools) != 0 {
		t.Fatalf("final request did not disable tools: %+v", requests)
	}
	for _, call := range declared {
		if result := toolResultByID(requests[1].Messages, call.ID); result == nil {
			t.Fatalf("missing tool result for declared call %q: %+v", call.ID, requests[1].Messages)
		}
	}
}

func TestProcessNormalizesMissingToolCallIDsConsistently(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "one"}}},
			{Function: llm.ToolFunction{Name: "test.lookup", Arguments: map[string]interface{}{"q": "two"}}},
		}}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(_ context.Context, args map[string]interface{}) (governance.Result, error) {
		return productiveResult(args["q"].(string)), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)

	if _, err := processAgent(agent, "ids", "homeassistant", "session", "user-1", "User", "lookup", nil, nil); err != nil {
		t.Fatal(err)
	}
	messages := primaryRequests(chat.requests)[1].Messages
	assistant := messages[len(messages)-3]
	wants := []string{"call_1_1", "call_1_2"}
	for i, want := range wants {
		if assistant.ToolCalls[i].ID != want {
			t.Fatalf("assistant call %d ID = %q, want %q", i, assistant.ToolCalls[i].ID, want)
		}
		if result := toolResultByID(messages, want); result == nil {
			t.Fatalf("missing tool result with normalized ID %q: %+v", want, messages)
		}
	}
}

func TestNormalizeToolCallIDsAvoidsProvidedAndDuplicateCollisions(t *testing.T) {
	message := llm.ChatMessage{ToolCalls: []llm.ToolCall{
		{ID: "call_1_2"},
		{},
		{ID: "call_1_2"},
	}}
	normalizeToolCallIDs(&message, 1)
	seen := map[string]bool{}
	for _, call := range message.ToolCalls {
		if call.ID == "" || seen[call.ID] {
			t.Fatalf("tool call IDs are not unique: %+v", message.ToolCalls)
		}
		seen[call.ID] = true
	}
	if message.ToolCalls[0].ID != "call_1_2" {
		t.Fatalf("first provider ID was not preserved: %+v", message.ToolCalls)
	}
}

func TestProcessRetriesEmptyVisibleResponse(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "reasoning only"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "visible answer"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return productiveResult("lookup result"), nil
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, store := newTestAgent(t, chat, nil, reg)

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "visible answer" {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected retry chat call, got %d calls", len(primary))
	}
	if len(primary[1].Tools) != 0 {
		t.Fatalf("expected retry with tools disabled, got %+v", primary[1].Tools)
	}
	lastMessage := primary[1].Messages[len(primary[1].Messages)-1]
	if lastMessage.Role != "user" || lastMessage.Content != emptyResponseRetryPrompt {
		t.Fatalf("unexpected retry prompt: %+v", lastMessage)
	}

	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].AssistantText != "visible answer" {
		t.Fatalf("unexpected stored turn: %+v", turns)
	}
}

func TestProcessFallsBackAfterEmptyRetry(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "reasoning only"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Thinking: "still reasoning"}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)

	var chunks []StreamChunk
	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != emptyResponseFallback {
		t.Fatalf("unexpected response %q", resp.Response)
	}
	foundFallbackChunk := false
	for _, chunk := range chunks {
		if chunk.Type == ChunkContent && chunk.Text == emptyResponseFallback {
			foundFallbackChunk = true
		}
	}
	if !foundFallbackChunk {
		t.Fatalf("expected fallback content chunk, got %+v", chunks)
	}

	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].AssistantText != emptyResponseFallback {
		t.Fatalf("unexpected stored turn: %+v", turns)
	}
}

func TestProcessRetriesTemporaryOllamaParserErrorWithTools(t *testing.T) {
	parserErr := &llm.ChatHTTPError{StatusCode: 500, Body: `expected element type <function> but have <parameter>`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: parserErr},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "recovered"}}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, registry.Spec{Name: "test.lookup", Description: "Lookup"}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return productiveResult("lookup result"), nil
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	agent, _ := newTestAgent(t, chat, nil, reg)

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "recovered" || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	primary := primaryRequests(chat.requests)
	if len(primary) != 2 {
		t.Fatalf("expected two calls, got %d", len(primary))
	}
	if len(primary[0].Tools) == 0 || len(primary[1].Tools) != len(primary[0].Tools) {
		t.Fatalf("retry did not preserve tools: first=%+v retry=%+v", primary[0].Tools, primary[1].Tools)
	}
	if len(primary[1].Messages) != len(primary[0].Messages) || primary[1].Messages[len(primary[1].Messages)-1].Content != primary[0].Messages[len(primary[0].Messages)-1].Content {
		t.Fatalf("retry changed messages: first=%+v retry=%+v", primary[0].Messages, primary[1].Messages)
	}
}

func TestProcessUsesFriendlyFallbackAfterRepeatedOllamaParserError(t *testing.T) {
	parserErr := &llm.ChatHTTPError{StatusCode: 500, Body: `XML syntax error on line 7: unexpected EOF`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: parserErr}, {err: parserErr}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	var chunks []StreamChunk

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != emptyResponseFallback || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	contentChunks := 0
	for _, chunk := range chunks {
		if chunk.Type == ChunkContent && chunk.Text == emptyResponseFallback {
			contentChunks++
		}
	}
	if contentChunks != 1 {
		t.Fatalf("fallback chunks = %d, want 1: %+v", contentChunks, chunks)
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].AssistantText != emptyResponseFallback {
		t.Fatalf("fallback turn was not persisted: %+v", turns)
	}
}

func TestProcessDoesNotRetryUnrelatedModelError(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: &llm.ChatHTTPError{StatusCode: 500, Body: "out of memory"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Error == "" || len(primaryRequests(chat.requests)) != 1 {
		t.Fatalf("unexpected response or retry: response=%+v calls=%d", resp, len(chat.requests))
	}
}

func TestProcessRetriesStoppedModelRunnerWithExponentiallySmallerImages(t *testing.T) {
	runnerErr := &llm.ChatHTTPError{StatusCode: 500, Body: `{"error":{"message":"model runner has unexpectedly stopped, this may be due to resource limitations"}}`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{
		{err: runnerErr},
		{err: runnerErr},
		{response: &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "recovered"}}},
	}}
	agent, _ := newTestAgent(t, chat, nil, nil)
	input := testInputImage(t, 800, 600)

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", []llm.InputImage{input}, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "recovered" || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 3 {
		t.Fatalf("calls = %d, want 3", len(requests))
	}
	wants := []image.Point{{X: 800, Y: 600}, {X: 600, Y: 450}, {X: 450, Y: 338}}
	for i, req := range requests {
		got := inputImageDimensions(t, req.Messages[len(req.Messages)-1].Images[0])
		if got != wants[i] {
			t.Fatalf("attempt %d dimensions = %v, want %v", i+1, got, wants[i])
		}
	}
}

func TestProcessUsesImageSizeFallbackAfterFiveStoppedRunnerAttempts(t *testing.T) {
	runnerErr := &llm.ChatHTTPError{StatusCode: 500, Body: `model runner has unexpectedly stopped`}
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: runnerErr}, {err: runnerErr}, {err: runnerErr}, {err: runnerErr}, {err: runnerErr}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	var chunks []StreamChunk

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", []llm.InputImage{testInputImage(t, 800, 600)}, func(chunk StreamChunk) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != imageSizeFallback || resp.Error != "" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != maxImageModelAttempts {
		t.Fatalf("calls = %d, want %d", len(chat.requests), maxImageModelAttempts)
	}
	if got := inputImageDimensions(t, requests[0].Messages[len(requests[0].Messages)-1].Images[0]); got != (image.Point{X: 800, Y: 600}) {
		t.Fatalf("first attempt dimensions = %v, want 800x600", got)
	}
	if got := inputImageDimensions(t, requests[4].Messages[len(requests[4].Messages)-1].Images[0]); got != (image.Point{X: 253, Y: 190}) {
		t.Fatalf("fifth attempt dimensions = %v, want 253x190", got)
	}
	contentChunks := 0
	for _, chunk := range chunks {
		if chunk.Type == ChunkContent && chunk.Text == imageSizeFallback {
			contentChunks++
		}
	}
	if contentChunks != 1 {
		t.Fatalf("fallback chunks = %d, want 1", contentChunks)
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil || len(turns) != 1 || turns[0].AssistantText != imageSizeFallback {
		t.Fatalf("fallback turn was not persisted: turns=%+v err=%v", turns, err)
	}
}

func TestProcessDoesNotRetryStoppedModelRunnerWithoutImages(t *testing.T) {
	chat := &fakeChatter{outcomes: []fakeChatOutcome{{err: &llm.ChatHTTPError{StatusCode: 500, Body: `model runner has unexpectedly stopped`}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	resp, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Error == "" || len(primaryRequests(chat.requests)) != 1 {
		t.Fatalf("unexpected response or retry: response=%+v calls=%d", resp, len(chat.requests))
	}
}

func TestProcessFailsWhenTenantProfileCannotBeResolved(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "must not run"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil); err == nil {
		t.Fatal("expected profile resolution failure")
	}
	if len(chat.requests) != 0 {
		t.Fatalf("model called after profile resolution failed: %+v", chat.requests)
	}
}

func TestProcessIncludesRoleCorrectSessionContextWithAutomaticRecallLookup(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "new answer"}}}}
	embedder := &fakeEmbedder{vectors: [][]float64{{0, 1}, {1, 0}, {0, 1}, {0, 1}, {0, 1}, {0, 1}}}
	agent, store := newTestAgent(t, chat, embedder, nil)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, "older unrelated", "old a", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, "older relevant", "old b", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"recent one", "recent two", "recent three", "recent four"} {
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, text, "recent answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	_, err = processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "follow up", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(embedder.inputs) != 0 {
		t.Fatalf("semantic recall embedded without a live vector revision: %+v", embedder.inputs)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	if len(messages) != 15 || messages[2].Role != "user" || messages[2].Content != "older unrelated" || messages[3].Role != "assistant" || messages[3].Content != "old a" {
		t.Fatalf("history roles or chronology are wrong: %+v", messages)
	}
	if messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Content != "follow up" {
		t.Fatalf("current request is not the final user message: %+v", messages)
	}
}

func TestProcessUsesCommittedSummaryWithRecentVerbatimTail(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "continued"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-1", "user-1", profile.Generation, fmt.Sprintf("turn %d user", i), fmt.Sprintf("turn %d assistant", i), nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := store.CompactionWindowAfter(context.Background(), "user-1", "session-1", profile.Generation, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user-1", "session-1", profile.Generation, turns.Turns[0].ID, turns.Turns[1].ID, turns.Turns[1].ID, "test-model", "test-v1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "test", time.Minute, "test-model", "test-v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, memory.SummaryArtifact{Narrative: "The first two turns established Atlas.", OpenTasks: []string{"Continue"}, GenerationModel: "test-model", GeneratorVersion: "test-v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-summary", "homeassistant", "session-1", "user-1", "Display", "continue", nil, nil); err != nil {
		t.Fatal(err)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	if len(messages) != 20 || !strings.Contains(messages[2].Content, "session_history_summary") || !strings.Contains(messages[2].Content, "first two turns") {
		t.Fatalf("summary context missing or malformed: %+v", messages)
	}
	if messages[3].Content != "turn 3 user" || messages[len(messages)-2].Content != "turn 10 assistant" || messages[len(messages)-1].Content != "continue" {
		t.Fatalf("recent verbatim tail is wrong: %+v", messages)
	}
}

func TestProcessInjectsTenantScopedRecallWithoutPersistingIt(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "Atlas."}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	_, err := memorytest.PublishMemory(context.Background(), store.Store, "user-1", memorytest.MemoryFixture{
		Scope: memory.ScopeLongTerm, Category: "projects", Statement: "The project codename is Atlas.", Evidence: "The user named it.", Confidence: 0.95, Importance: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = memorytest.PublishMemory(context.Background(), store.Store, "user-2", memorytest.MemoryFixture{
		Scope: memory.ScopeLongTerm, Category: "projects", Statement: "The private project codename is Borealis.", Evidence: "Another user's project.", Confidence: 1, Importance: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := indexing.NewService(store.Store, nil, nil, "", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err = processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "What is the project codename?", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	current := messages[len(messages)-1]
	if !strings.Contains(current.Content, "UNTRUSTED LOWER-AUTHORITY REFERENCE") || !strings.Contains(current.Content, "Atlas") {
		t.Fatalf("automatic recall missing from current user turn: %+v", current)
	}
	if strings.Contains(current.Content, "Borealis") || strings.Contains(messages[0].Content, "Atlas") {
		t.Fatalf("recall crossed tenant or authority boundary: %+v", messages)
	}
	turns, err := store.RecentSessionTurns("user-1", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].UserText != "What is the project codename?" || strings.Contains(turns[0].UserText, "Atlas") {
		t.Fatalf("injected recall was persisted: %+v", turns)
	}
}

func TestProcessDoesNotConversationallyConfirmPendingMemory(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "model response"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	output, err := policy.Evaluate(policy.CandidateInput{
		SourceUserText: "My phone is 555-0100", Statement: "The user's phone is 555-0100.", Evidence: "My phone is 555-0100",
		Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect,
		Sensitivity: policy.SensitivityIdentityOrContact, Mode: policy.ModeAutomaticExtraction,
		Scope: policy.ScopeLongTerm, Category: policy.CategoryIdentity,
		Context: policy.ContextDirectAssertion, Confidence: 0.95, Importance: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", memory.CandidateProposal{Output: output, IdempotencyKey: "pending-phone"})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user-1", Gateway: "homeassistant", ExternalID: "user-1", Assurance: identity.AssuranceHomeAssistantToken}
	response, err := agent.Process(context.Background(), Request{RequestID: "req-1", Principal: principal, DisplayName: "Display", SessionKey: "session-1", IsDirect: true, Prompt: "yes remember it"})
	if err != nil || response.Response != "model response" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	messages := primaryRequests(chat.requests)[0].Messages
	if strings.Contains(messages[0].Content, "memory confirmation") || strings.Contains(messages[len(messages)-1].Content, "pending_memory_confirmation") || strings.Contains(messages[len(messages)-1].Content, "555-0100") {
		t.Fatalf("pending confirmation was injected: %+v", messages)
	}
	var publishedID int64
	err = store.sql.QueryRow(`SELECT COALESCE(published_memory_id, 0) FROM memory_candidates WHERE canonical_user_id = 'user-1' AND id = ?`, candidate.ID).Scan(&publishedID)
	if err != nil || publishedID != candidate.PublishedMemoryID {
		t.Fatalf("conversational phrase changed candidate publication: id=%d err=%v", publishedID, err)
	}
}

func TestProcessAddsIMessagePlainTextSystemInstruction(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := processAgent(agent, "req-1", "imessage", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if system.Role != "system" || !strings.Contains(system.Content, "iMessage") || !strings.Contains(system.Content, "does not render Markdown") {
		t.Fatalf("missing imessage system instruction: %+v", system)
	}
}

func TestProcessUsesFreshOperatorManagedSoulAsSystemPrompt(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "first"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "second"}},
	}}
	agent, _, soulPath := newTestAgentWithSoulPath(t, chat, nil, nil)

	if _, err := processAgent(agent, "req-1", "imessage", "session-1", "user-1", "Display", "first question", nil, nil); err != nil {
		t.Fatalf("first process: %v", err)
	}
	firstSystem := primaryRequests(chat.requests)[0].Messages[0]
	if firstSystem.Role != "system" || !strings.HasPrefix(firstSystem.Content, "You are Oswald.\n\n# Gateway Instructions") {
		t.Fatalf("soul and gateway instructions have incorrect authority or order: %+v", firstSystem)
	}

	if err := os.WriteFile(soulPath, []byte("You are Oswald after a manual edit."), 0o600); err != nil {
		t.Fatalf("manually edit soul fixture: %v", err)
	}
	if _, err := processAgent(agent, "req-2", "homeassistant", "session-2", "user-1", "Display", "second question", nil, nil); err != nil {
		t.Fatalf("second process: %v", err)
	}
	secondSystem := primaryRequests(chat.requests)[1].Messages[0]
	if secondSystem.Role != "system" || secondSystem.Content != "You are Oswald after a manual edit." {
		t.Fatalf("manual soul edit was not reloaded as the system prompt: %+v", secondSystem)
	}
}

func TestProcessDoesNotAddIMessageSystemInstructionForOtherGateways(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if strings.Contains(system.Content, "does not render Markdown") {
		t.Fatalf("unexpected imessage system instruction: %+v", system)
	}
}

func TestProcessDoesNotInjectCurrentTimeIntoSystemPrompt(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, _ := newTestAgent(t, chat, nil, nil)

	_, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	system := primaryRequests(chat.requests)[0].Messages[0]
	if strings.Contains(system.Content, "# Current Date and Time") {
		t.Fatalf("system prompt contains injected current time: %q", system.Content)
	}
}

func TestProcessSendsStrippedSpeakerIntroAsProviderUser(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "ok"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	intro := "You are speaking with Example User aka examplehandle."
	if err := store.SyncSpeakerIntro("user-1", intro); err != nil {
		t.Fatalf("sync speaker intro: %v", err)
	}

	_, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Display", "question", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	req := primaryRequests(chat.requests)[0]
	if req.User != "Example User aka examplehandle" {
		t.Fatalf("provider user = %q, want stripped speaker name", req.User)
	}
	if !messagesContain(req.Messages, intro) {
		t.Fatalf("system messages no longer contain full speaker intro: %+v", req.Messages)
	}
}

func TestSessionMemoryUserContentReplyOnly(t *testing.T) {
	got := sessionMemoryUserContent("[Replying to Alice: \"old\"]", 0)
	if got != "[User replied to a prior message]" {
		t.Fatalf("unexpected content %q", got)
	}
}

func TestProviderUserValueStripsStaticSpeakerPrefix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "speaker intro",
			input: "You are speaking with Example User aka examplehandle.",
			want:  "Example User aka examplehandle",
		},
		{
			name:  "display name",
			input: "Example User",
			want:  "Example User",
		},
		{
			name:  "canonical id",
			input: "usr_123",
			want:  "usr_123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerUserValue(tt.input); got != tt.want {
				t.Fatalf("providerUserValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestProcessUsesDynamicMCPDiscoveryTools(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("call-discover", "home.tools", map[string]interface{}{"query": "light"}),
		toolCallResponse("call-tool", "home.turn_on", map[string]interface{}{"entity": "light.office"}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	agent, _ := newTestAgent(t, chat, nil, nil)
	agent.mcpProvider = &fakeMCPProvider{}

	resp, err := processAgent(agent, "req-mcp", "homeassistant", "session-mcp", "user-1", "User", "turn on office light", nil, nil)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if resp.Response != "done" {
		t.Fatalf("response = %q", resp.Response)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) < 2 {
		t.Fatalf("expected multiple requests, got %d", len(requests))
	}
	if !requestHasTool(requests[0], "home.tools") {
		t.Fatalf("first request did not include home.tools: %+v", toolNames(requests[0]))
	}
	if requestHasTool(requests[0], "home.turn_on") {
		t.Fatalf("first request exposed actual MCP tool before discovery")
	}
	if !requestHasTool(requests[1], "home.turn_on") {
		t.Fatalf("second request did not expose actual MCP tool: %+v", toolNames(requests[1]))
	}
	if description := requestToolDescription(requests[1], "home.turn_on"); description != "Turn on a light" {
		t.Fatalf("discovered tool description = %q", description)
	}
}

func TestProcessMCPDiscoveryDoesNotAuthorizeSameBatchCall(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			batch := toolCallResponse("discover", "home.tools", map[string]interface{}{"query": "light"})
			batch.Message.ToolCalls = append(batch.Message.ToolCalls, toolCallResponse("premature", "home.turn_on", map[string]interface{}{"entity": "light.office"}).Message.ToolCalls...)
			chat := &fakeChatter{responses: []*llm.ChatResponse{
				batch,
				toolCallResponse("authorized", "home.turn_on", map[string]interface{}{"entity": "light.office"}),
				{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
			}}
			a, _ := newTestAgent(t, chat, nil, nil)
			provider := &fakeMCPProvider{}
			a.mcpProvider = provider
			var callback func(StreamChunk)
			if streaming {
				callback = func(StreamChunk) {}
			}
			resp, err := processAgent(a, "req-mcp-batch", "homeassistant", "session-mcp-batch", "user-1", "User", "turn on office light", nil, callback)
			if err != nil || resp == nil || resp.Response != "done" {
				t.Fatalf("process = %+v, %v", resp, err)
			}
			if got := provider.executed; !slices.Equal(got, []string{"home.tools", "home.turn_on"}) {
				t.Fatalf("provider executions = %v, want discovery and one authorized execution", got)
			}
			if len(chat.requests) != 3 {
				t.Fatalf("model requests = %d, want 3", len(chat.requests))
			}
			for i, req := range chat.requests {
				if req.Stream != streaming || !requestHasTool(req, "home.tools") || requestHasTool(req, "home.turn_on") != (i > 0) {
					t.Fatalf("request %d: stream=%t tools=%v", i, req.Stream, toolNames(req))
				}
			}
			wantResults := []llm.ChatMessage{
				{Role: "tool", ToolCallID: "discover", Content: "Available MCP tools from home:\n1. home.turn_on"},
				{Role: "tool", ToolCallID: "premature", Content: "Tool call blocked: this tool was not available for this model step. Use only currently available tools."},
				{Role: "tool", ToolCallID: "authorized", Content: "light turned on"},
			}
			for i, count := range []int{0, 2, 3} {
				var results []llm.ChatMessage
				var callIDs []string
				for _, message := range chat.requests[i].Messages {
					for _, call := range message.ToolCalls {
						callIDs = append(callIDs, call.ID)
					}
					if message.Role == "tool" {
						results = append(results, message)
					}
				}
				if len(results) != count || len(callIDs) != count {
					t.Fatalf("request %d: calls=%v results=%+v, want %d correlated pairs", i, callIDs, results, count)
				}
				for j, result := range results {
					want := wantResults[j]
					if callIDs[j] != want.ToolCallID || result.ToolCallID != want.ToolCallID || result.Content != want.Content {
						t.Fatalf("request %d result %d: call=%q result=%+v, want %+v", i, j, callIDs[j], result, want)
					}
				}
			}
		})
	}
}

func TestProcessPreExposesMCPToolsFromRecentSessionTurns(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	agent.mcpProvider = &fakeMCPProvider{}
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-mcp", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session-mcp", "user-1", profile.Generation, "prior question", "prior answer", []string{"home.turn_on", "home.tools", "web.search"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := processAgent(agent, "req-mcp", "homeassistant", "session-mcp", "user-1", "User", "again", nil, nil); err != nil {
		t.Fatalf("process: %v", err)
	}
	request := primaryRequests(chat.requests)[0]
	if !requestHasTool(request, "home.turn_on") {
		t.Fatalf("first request did not pre-expose recent MCP tool: %+v", toolNames(request))
	}
	if description := requestToolDescription(request, "home.turn_on"); description != "Turn on a light" {
		t.Fatalf("pre-exposed tool description = %q", description)
	}
	for _, message := range request.Messages {
		if strings.Contains(message.Content, "Tools used:") {
			t.Fatalf("internal tool annotation leaked into history: %+v", request.Messages)
		}
	}
}

func TestProcessPreExposesLatestFourMCPToolsAcrossSummaryBoundary(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	agent.mcpProvider = &fakeMCPProvider{}
	profile, err := store.ResolveSessionProfile(context.Background(), "user-1", "session-mcp", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		var tools []string
		if i == 1 {
			tools = []string{"home.turn_on"}
		}
		if err := store.AppendSessionTurnForGeneration(context.Background(), "session-mcp", "user-1", profile.Generation, fmt.Sprintf("prior %d", i), "answer", tools, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := store.CompactionWindowAfter(context.Background(), "user-1", "session-mcp", profile.Generation, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user-1", "session-mcp", profile.Generation, turns.Turns[0].ID, turns.Turns[1].ID, turns.Turns[1].ID, "test-model", "test-v1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "test", time.Minute, "test-model", "test-v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, memory.SummaryArtifact{Narrative: "Earlier context.", GenerationModel: "test-model", GeneratorVersion: "test-v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-mcp", "homeassistant", "session-mcp", "user-1", "User", "again", nil, nil); err != nil {
		t.Fatal(err)
	}
	request := primaryRequests(chat.requests)[0]
	if !requestHasTool(request, "home.turn_on") {
		t.Fatalf("summarized fourth-newest MCP tool was not pre-exposed: %+v", toolNames(request))
	}
	for _, message := range request.Messages {
		if strings.Contains(message.Content, "Tools used: home.turn_on") {
			t.Fatalf("summarized turn was unexpectedly replayed verbatim: %+v", request.Messages)
		}
	}
}

func TestProcessFreezesTenantProfileUntilNewSession(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "one"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "two"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "three"}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)
	if _, err := memorytest.PublishMemory(context.Background(), store.Store, "user-1", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Category: "identity", Statement: "The user is Ada.", Confidence: 1, Importance: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-1", "homeassistant", "session-1", "user-1", "Ada", "first", nil, nil); err != nil {
		t.Fatal(err)
	}
	firstProfile := tenantProfileMessage(primaryRequests(chat.requests)[0].Messages)
	if _, err := memorytest.PublishMemory(context.Background(), store.Store, "user-1", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Category: "communication_preferences", Statement: "The user prefers concise replies.", Confidence: 1, Importance: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-2", "homeassistant", "session-1", "user-1", "Ada", "second", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-3", "homeassistant", "session-2", "user-1", "Ada", "third", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	frozenProfile := tenantProfileMessage(requests[1].Messages)
	latestProfile := tenantProfileMessage(requests[2].Messages)
	if firstProfile == "" || frozenProfile != firstProfile {
		t.Fatalf("profile changed in active session: first=%q frozen=%q", firstProfile, frozenProfile)
	}
	if !strings.Contains(latestProfile, "concise replies") || latestProfile == firstProfile {
		t.Fatalf("new session did not receive latest profile: %q", latestProfile)
	}
	if len(requests[0].Messages) != 3 || requests[0].Messages[1].Role != "user" || !strings.Contains(requests[1].Messages[1].Content, "authority=\"lower\"") {
		t.Fatalf("tenant profile is not lower-authority user context: %+v", requests[0].Messages)
	}
}

func TestProcessNeverIncludesAnotherUsersTenantProfile(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "one"}},
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "two"}},
	}}
	agent, store := newTestAgent(t, chat, nil, nil)
	for _, tc := range []struct{ user, statement string }{{"user-1", "The user is Alice."}, {"user-2", "The user is Bob."}} {
		if _, err := memorytest.PublishMemory(context.Background(), store.Store, tc.user, memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Category: "identity", Statement: tc.statement, Confidence: 1, Importance: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := processAgent(agent, "req-a", "homeassistant", "shared-session", "user-1", "Alice", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(agent, "req-b", "homeassistant", "shared-session", "user-2", "Bob", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if messagesContain(requests[0].Messages, "Bob") || messagesContain(requests[1].Messages, "Alice") {
		t.Fatalf("cross-user profile leak: a=%+v b=%+v", requests[0].Messages, requests[1].Messages)
	}
}

func TestProcessDoesNotPreExposeMCPToolOutsideRecentFourTurns(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	agent.mcpProvider = &fakeMCPProvider{}
	if err := store.AppendSessionTurn(context.Background(), "session-mcp", "user-1", "old question", "old answer", []string{"home.turn_on"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := store.AppendSessionTurn(context.Background(), "session-mcp", "user-1", "recent question", "recent answer", nil, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := processAgent(agent, "req-mcp", "homeassistant", "session-mcp", "user-1", "User", "again", nil, nil); err != nil {
		t.Fatalf("process: %v", err)
	}
	request := primaryRequests(chat.requests)[0]
	if requestHasTool(request, "home.turn_on") {
		t.Fatalf("first request exposed tool from fifth-oldest turn: %+v", toolNames(request))
	}
	if strings.Contains(request.Messages[0].Content, "old question") {
		t.Fatalf("system prompt included fifth-oldest turn:\n%s", request.Messages[0].Content)
	}
}

func TestProcessDoesNotAutomaticallyInjectGlobalMemory(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	agent, store := newTestAgent(t, chat, nil, nil)
	defer store.Close()
	if _, err := processAgent(agent, "request", "homeassistant", "session", "user-1", "User", "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 1 {
		t.Fatalf("request count=%d", len(requests))
	}
	for _, message := range requests[0].Messages {
		if strings.Contains(strings.ToLower(message.Content), "<global_memory") {
			t.Fatalf("automatic global memory block in prompt: %q", message.Content)
		}
	}
}

func TestAgentKeepsDefaultVisibleGlobalMemorySearchAfterGlobalMCPResult(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		toolCallResponse("discover", "home.tools", nil),
		toolCallResponse("global-call", "home.turn_on", map[string]interface{}{"entity": "light"}),
		{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}},
	}}
	reg := registry.New(config.NewLogger(config.LevelError))
	if err := registerTestTool(t, reg, registry.Spec{Name: toolnames.GlobalMemorySearch}, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return productiveResult("global memory"), nil
	}); err != nil {
		t.Fatal(err)
	}
	agent, store := newTestAgent(t, chat, nil, reg)
	defer store.Close()
	agent.mcpProvider = &fakeMCPProvider{scope: mcp.ScopeGlobal}
	if _, err := processAgent(agent, "request", "homeassistant", "session", "user-1", "User", "check the deployment", nil, nil); err != nil {
		t.Fatal(err)
	}
	requests := primaryRequests(chat.requests)
	if len(requests) != 3 {
		t.Fatalf("request count=%d", len(requests))
	}
	for i, request := range requests {
		if !requestHasTool(request, toolnames.GlobalMemorySearch) {
			t.Fatalf("global memory search missing from request %d: %+v", i, toolNames(request))
		}
	}
}

func toolCallResponse(id, name string, args map[string]interface{}) *llm.ChatResponse {
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Function: llm.ToolFunction{Name: name, Arguments: args}}}}}
}

func testToolPolicy() governance.ToolPolicy {
	return governance.ToolPolicy{MaxExecutions: 4, MaxFailures: 2, MaxUnproductive: 2, BlockDuplicates: true}
}

func testGlobalPolicy() governance.GlobalPolicy {
	return governance.GlobalPolicy{MaxExecutions: 10, MaxToolIterations: 10}
}

func productiveResult(content string) governance.Result {
	return governance.Result{Content: content, Outcome: governance.OutcomeProductive}
}

func registerStagingTool(t *testing.T, reg *registry.Registry, store *memory.Store, resetSession bool) {
	t.Helper()
	toolPolicy := testToolPolicy()
	toolPolicy.History = governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false}
	err := registerTestTool(t, reg, registry.Spec{Name: toolnames.UserMemorySave, Description: "Stage a memory", Schema: &llm.ToolParameters{Type: "object"}}, toolPolicy, func(ctx context.Context, _ map[string]interface{}) (governance.Result, error) {
		collector := requestctx.MemoryStageCollectorFromContext(ctx)
		if collector == nil {
			return governance.Result{}, errors.New("memory collector is missing")
		}
		candidate := policy.CandidateOutput{
			Statement: "The user prefers dark mode.", Evidence: "I prefer dark mode.",
			Category: policy.CategoryDurablePreferences, ClaimSlot: "durable_preferences.fact", ClaimValue: "dark mode",
			Mode: policy.ModeAgentSave, Approval: policy.ApprovalApproved,
		}
		if err := collector.Stage([]requestctx.StagedMemoryCandidate{{CanonicalUserID: "user-1", Candidate: candidate}}); err != nil {
			return governance.Result{}, err
		}
		if resetSession {
			meta := requestctx.MetadataFromContext(ctx)
			if _, err := store.ResetSession(ctx, "user-1", meta.SessionID, time.Hour); err != nil {
				return governance.Result{}, err
			}
		}
		return productiveResult(`{"status":"staged"}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func toolResultByID(messages []llm.ChatMessage, id string) *llm.ChatMessage {
	for i := range messages {
		if messages[i].Role == "tool" && messages[i].ToolCallID == id {
			return &messages[i]
		}
	}
	return nil
}

type fakeChatter struct {
	responses []*llm.ChatResponse
	outcomes  []fakeChatOutcome
	requests  []llm.ChatRequest
}

type fakeChatOutcome struct {
	response *llm.ChatResponse
	err      error
}

func (f *fakeChatter) Chat(_ context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.requests = append(f.requests, req)
	if len(f.outcomes) > 0 {
		outcome := f.outcomes[0]
		f.outcomes = f.outcomes[1:]
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.response == nil {
			return nil, errors.New("empty fake outcome")
		}
		if cb != nil {
			if outcome.response.Message.Thinking != "" {
				cb(llm.ChatMessage{Thinking: outcome.response.Message.Thinking})
			}
			if outcome.response.Message.Content != "" {
				cb(llm.ChatMessage{Content: outcome.response.Message.Content})
			}
		}
		return outcome.response, nil
	}
	if len(f.responses) == 0 {
		return nil, errors.New("no fake response")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	if cb != nil {
		if resp.Message.Thinking != "" {
			cb(llm.ChatMessage{Thinking: resp.Message.Thinking})
		}
		if resp.Message.Content != "" {
			cb(llm.ChatMessage{Content: resp.Message.Content})
		}
	}
	return resp, nil
}

type fakeEmbedder struct {
	vectors [][]float64
	inputs  []string
}

type fakeMCPProvider struct {
	scope    string
	executed []string
}

func (p *fakeMCPProvider) DiscoveryTools(context.Context, identity.Principal) []llm.Tool {
	return []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "home.tools", Description: "Search Home Assistant tools", Parameters: llm.ToolParameters{Type: "object"}}}}
}

func (p *fakeMCPProvider) ResolveTools(_ context.Context, _ identity.Principal, names []string) []string {
	for _, name := range names {
		if name == "home.turn_on" {
			return []string{name}
		}
	}
	return nil
}

func (p *fakeMCPProvider) LLMTools(_ context.Context, _ identity.Principal, exposed map[string]bool) []llm.Tool {
	if !exposed["home.turn_on"] {
		return nil
	}
	return []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "home.turn_on", Description: "Turn on a light", Parameters: llm.ToolParameters{Type: "object"}}}}
}

func (p *fakeMCPProvider) ToolPolicy(string) governance.ToolPolicy {
	return testToolPolicy()
}

func (p *fakeMCPProvider) Execute(ctx context.Context, _ identity.Principal, name string, _ map[string]interface{}, exposed map[string]bool) (mcp.ExecutionResult, bool, error) {
	p.executed = append(p.executed, name)
	if name == "home.tools" {
		if exposer := requestctx.ToolExposerFromContext(ctx); exposer != nil {
			exposer.ExposeTools([]string{"home.turn_on"})
		}
		return mcp.ExecutionResult{Result: productiveResult("Available MCP tools from home:\n1. home.turn_on"), IsDiscovery: true}, true, nil
	}
	if name == "home.turn_on" && exposed[name] {
		scope := p.scope
		if scope == "" {
			scope = mcp.ScopeUser
		}
		return mcp.ExecutionResult{Result: productiveResult("light turned on"), Scope: scope, ServerID: "server-1", ServerName: "home", ToolName: name, RemoteToolName: "turn_on"}, true, nil
	}
	return mcp.ExecutionResult{}, false, nil
}

func processAgent(agent *Agent, requestID, gateway, sessionKey, userID, displayName, prompt string, images []llm.InputImage, streamFunc func(StreamChunk)) (*Response, error) {
	assurance := identity.AssuranceHomeAssistantToken
	switch gateway {
	case "discord":
		assurance = identity.AssuranceDiscordGateway
	case "imessage":
		assurance = identity.AssuranceBlueBubblesWebhook
	}
	response, err := agent.Process(context.Background(), Request{
		RequestID: requestID,
		Principal: identity.Principal{
			CanonicalUserID: userID,
			Gateway:         gateway,
			ExternalID:      userID,
			Assurance:       assurance,
		},
		DisplayName: displayName,
		SessionKey:  sessionKey,
		Prompt:      prompt,
		Images:      images,
		StreamFunc:  streamFunc,
	})
	if err == nil && response != nil && response.SourceTurnID > 0 && agent.userMemory != nil {
		_ = agent.userMemory.MarkSessionTurnDelivered(context.Background(), userID, response.SourceTurnID)
	}
	return response, err
}

func (f *fakeEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.inputs = append(f.inputs, req.Input)
	if len(f.vectors) == 0 {
		return nil, errors.New("no fake embedding")
	}
	vec := f.vectors[0]
	f.vectors = f.vectors[1:]
	return &llm.EmbedResponse{Model: req.Model, Embeddings: [][]float64{vec}}, nil
}

func newTestAgent(t *testing.T, chat llm.Chatter, embedder llm.Embedder, reg *registry.Registry) (*Agent, *agentMemoryFixture) {
	agent, store, _ := newTestAgentWithSoulPath(t, chat, embedder, reg)
	return agent, store
}

func newTestAgentWithSoulPath(t *testing.T, chat llm.Chatter, embedder llm.Embedder, reg *registry.Registry) (*Agent, *agentMemoryFixture, string) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	if reg == nil {
		reg = registry.New(log)
	}
	dir := t.TempDir()
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0o600); err != nil {
		t.Fatalf("write soul fixture: %v", err)
	}
	soulStore := soul.NewStore(soulPath)
	dbPath := filepath.Join(dir, "oswald.db")
	db, err := database.Open(dbPath, log)
	if err != nil {
		t.Fatalf("open account database: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?), (?)`, "user-1", "user-2"); err != nil {
		t.Fatalf("seed account user: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, verified) VALUES ('homeassistant', 'user-1', 'user-1', 'User 1', 1), ('homeassistant', 'user-2', 'user-2', 'User 2', 1)`); err != nil {
		t.Fatalf("seed linked accounts: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	embeddingModel := ""
	if embedder != nil {
		embeddingModel = "embed-model"
	}
	userStore, err := memory.NewSQLiteStore(dbPath, embedder, embeddingModel, log)
	if err != nil {
		t.Fatalf("user store: %v", err)
	}
	agent := NewAgent(chat, reg, "test-model", soulStore, userStore, budget.ContextBudget{PromptLimit: 100000}, testGlobalPolicy(), log)
	t.Cleanup(func() { _ = userStore.Close() })
	return agent, &agentMemoryFixture{Store: userStore, sql: db.SQL()}, soulPath
}

func primaryRequests(requests []llm.ChatRequest) []llm.ChatRequest {
	out := make([]llm.ChatRequest, 0, len(requests))
	for _, req := range requests {
		if req.Format == "json_object" {
			continue
		}
		out = append(out, req)
	}
	return out
}

func messagesContain(messages []llm.ChatMessage, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

func tenantProfileMessage(messages []llm.ChatMessage) string {
	for _, message := range messages {
		start := strings.Index(message.Content, "<tenant_profile")
		end := strings.Index(message.Content, "</tenant_profile>")
		if start >= 0 && end >= start {
			return message.Content[start : end+len("</tenant_profile>")]
		}
	}
	return ""
}

func requestHasTool(req llm.ChatRequest, name string) bool {
	for _, tool := range req.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func requestToolDescription(req llm.ChatRequest, name string) string {
	for _, tool := range req.Tools {
		if tool.Function.Name == name {
			return tool.Function.Description
		}
	}
	return ""
}

func toolNames(req llm.ChatRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func testInputImage(t *testing.T, width, height int) llm.InputImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 127, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	input, err := media.NormalizeInputImageFromBytes(nil, "image/jpeg", buf.Bytes(), "test.jpg")
	if err != nil {
		t.Fatal(err)
	}
	return input.Image
}

func inputImageDimensions(t *testing.T, input llm.InputImage) image.Point {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return decoded.Bounds().Size()
}
