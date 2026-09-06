package discord

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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/runtimeinvalidation"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/testutil"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestDiscordWebSearchStatusReportsDegradation(t *testing.T) {
	status := discordToolStatusFor(&agent.ToolStreamPayload{
		Name:      "web.search",
		WebSearch: &agent.ToolStreamSearchPayload{Query: "current pricing", IsDegraded: true},
	})
	if status.completed != "Searched the web for \"current pricing\" with limited sources." {
		t.Fatalf("completed status = %q", status.completed)
	}
}

func TestDiscordWebFetchStatusDoesNotExposeURLOrContent(t *testing.T) {
	status := discordToolStatusFor(&agent.ToolStreamPayload{
		Name:       "web.fetch",
		Arguments:  map[string]interface{}{"url": "https://example.com/private-path"},
		ResultText: "private fetched content",
		WebFetch:   &agent.ToolStreamFetchPayload{Title: "Untrusted title", IsDegraded: true},
	})
	combined := status.running + status.completed + status.failed
	if status.completed != "Fetched the requested public page with limited extraction." {
		t.Fatalf("completed status = %q", status.completed)
	}
	for _, secret := range []string{"private-path", "private fetched content", "Untrusted title"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("web fetch status exposed %q: %s", secret, combined)
		}
	}
}

func TestDiscordUserMemorySaveStatusDoesNotExposeCandidateContent(t *testing.T) {
	status := discordToolStatusFor(&agent.ToolStreamPayload{
		Name:       "user_memory_save",
		Arguments:  map[string]interface{}{"evidence": "private evidence"},
		ResultText: "private result",
	})
	combined := status.running + status.completed + status.failed
	for _, private := range []string{"private evidence", "private result"} {
		if strings.Contains(combined, private) {
			t.Fatalf("memory save status exposed %q: %s", private, combined)
		}
	}
}

func TestDiscordHandleDirectMessageSendsReply(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	dg.handleMessage(discordMessage("msg-1", "channel-1", "", "123", "Alice", "hello discord"))

	primary := primaryDiscordRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	last := primary[0].Messages[len(primary[0].Messages)-1]
	if last.Content != "hello discord" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
		t.Fatalf("unexpected prompt %q", last.Content)
	}
	if rest.lastMessageContent() != "discord response" || len(rest.editedMessages()) != 0 {
		t.Fatalf("unexpected lifecycle delivery: sent=%+v edited=%+v", rest.sentMessages(), rest.editedMessages())
	}
	if _, ok := dg.lookupReply("sent-1"); !ok {
		t.Fatal("expected sent bot reply remembered")
	}
	principal := chat.lastPrincipal()
	if principal.CanonicalUserID == "" || principal.Gateway != "discord" || principal.ExternalID != "123" || principal.Assurance != identity.AssuranceDiscordGateway {
		t.Fatalf("unexpected principal: %+v", principal)
	}
}

func TestDiscordHandleDirectMessageStreamsAndFinalizesReply(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()
	const responseText = "A streamed Discord response that is long enough to display."
	chat.streamChunks = []llm.ChatMessage{{Content: responseText}}
	chat.response = responseText

	dg.handleMessage(discordMessage("msg-1", "channel-1", "", "123", "Alice", "hello discord"))

	primary := primaryDiscordRequests(chat.requests)
	if len(primary) != 1 || !primary[0].Stream {
		t.Fatalf("expected one streaming LLM request, got %+v", primary)
	}
	sent := rest.sentMessages()
	edited := rest.editedMessages()
	if len(sent) != 1 || sent[0]["content"] != strings.TrimSpace(discordStreamCursor) {
		t.Fatalf("unexpected progressive sends: %+v", sent)
	}
	if len(edited) != 1 || edited[0]["content"] != responseText {
		t.Fatalf("unexpected final edits: %+v", edited)
	}
	ctx, ok := dg.lookupReply("sent-1")
	if !ok || ctx.Text != responseText {
		t.Fatalf("final reply context not recorded: %+v, %t", ctx, ok)
	}
}

func TestDiscordStreamShowsCompactToolProgress(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")

	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "private reasoning"})
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolCall, Tool: &agent.ToolStreamPayload{Name: "web.search", Arguments: map[string]interface{}{"query": "secret query", "authorization": "Bearer private-token"}}})
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Tool: &agent.ToolStreamPayload{Name: "web.search", ResultText: "private result", DurationMS: 420}})
	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "post tool reasoning"})
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "The final streamed response is now arriving."})
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: "The final answer."}); err != nil {
		t.Fatal(err)
	}

	sent := rest.sentMessages()
	edited := rest.editedMessages()
	if len(sent) != 1 || sent[0]["content"] != "-# private reasoning"+discordStreamCursor {
		t.Fatalf("expected one lifecycle message, got %+v", sent)
	}
	if len(edited) != 5 ||
		edited[0]["content"] != "-# private reasoning\n\nSearching the web for \"secret query\"..." ||
		edited[1]["content"] != "Searched the web for \"secret query\"." ||
		edited[2]["content"] != "-# post tool reasoning"+discordStreamCursor ||
		edited[3]["content"] != strings.TrimSpace(discordStreamCursor) ||
		edited[4]["content"] != "The final answer." {
		t.Fatalf("unexpected stream edits: %+v", edited)
	}
	serialized, _ := json.Marshal(struct {
		Sent   []map[string]interface{}
		Edited []map[string]interface{}
	}{sent, edited})
	if !strings.Contains(string(serialized), "private reasoning") || !strings.Contains(string(serialized), "secret query") || strings.Contains(string(serialized), "private result") || strings.Contains(string(serialized), "private-token") {
		t.Fatalf("unexpected lifecycle data exposure: %s", serialized)
	}
}

func TestDiscordStreamShowsAndReplacesCompactionStatus(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")

	r.Stream(agent.StreamChunk{Type: agent.ChunkStatus, Text: "Compacting context..."})
	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "continuing with the compacted context"})
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "The answer is arriving."})
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: "The final answer."}); err != nil {
		t.Fatal(err)
	}

	sent := rest.sentMessages()
	edited := rest.editedMessages()
	if len(sent) != 1 || sent[0]["content"] != "Compacting context..." {
		t.Fatalf("compaction status was not sent: %+v", sent)
	}
	if len(edited) != 3 ||
		edited[0]["content"] != "-# continuing with the compacted context"+discordStreamCursor ||
		edited[1]["content"] != strings.TrimSpace(discordStreamCursor) ||
		edited[2]["content"] != "The final answer." {
		t.Fatalf("compaction status was not replaced by lifecycle output: %+v", edited)
	}
}

func TestDiscordCancellationRemovesPartialLifecycleOutput(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.stream.editInterval = time.Millisecond
	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "partial reasoning"})
	deadline := time.Now().Add(time.Second)
	for len(rest.sentMessages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(rest.sentMessages()) != 1 {
		t.Fatalf("partial lifecycle message was not sent: %+v", rest.sentMessages())
	}
	if err := r.CancelAgentResponse(); err != nil {
		t.Fatal(err)
	}
	deleted := rest.deletedMessageIDs()
	if len(deleted) != 1 || deleted[0] != "sent-1" {
		t.Fatalf("deleted lifecycle messages = %v", deleted)
	}
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "late content"})
}

func TestDiscordStreamFinalizesLongResponseAcrossMessages(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	responseText := strings.Repeat("long response text ", 260)
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: responseText})
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: responseText}); err != nil {
		t.Fatal(err)
	}

	chunks := splitMessage(responseText, 2000)
	sent := rest.sentMessages()
	edited := rest.editedMessages()
	if len(chunks) < 2 || len(sent) != len(chunks) || len(edited) != 1 {
		t.Fatalf("chunks=%d sent=%d edits=%d", len(chunks), len(sent), len(edited))
	}
	if edited[0]["content"] != chunks[0] {
		t.Fatalf("first final chunk mismatch: got %q want %q", edited[0]["content"], chunks[0])
	}
	for i := range chunks {
		ctx, ok := dg.lookupReply(fmt.Sprintf("sent-%d", i+1))
		if !ok || ctx.Text != chunks[i] {
			t.Fatalf("chunk %d reply context=%+v found=%t", i, ctx, ok)
		}
	}
}

func TestDiscordStreamStartsMutableContinuationAfterFirstChunkFills(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.stream.editInterval = 5 * time.Millisecond

	firstChunk := strings.Repeat("a", 2000)
	streamed := firstChunk + "second"
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: streamed})
	waitForDiscordMessages(t, rest, 2)

	sent := rest.sentMessages()
	if sent[0]["content"] != strings.TrimSpace(discordStreamCursor) {
		t.Fatalf("first lifecycle message=%q, want initial cursor", sent[0]["content"])
	}
	if sent[1]["content"] != "second"+discordStreamCursor {
		t.Fatalf("continuation preview=%q, want streamed second chunk", sent[1]["content"])
	}
	if _, ok := sent[0]["message_reference"]; !ok {
		t.Fatal("first lifecycle message did not reference the inbound message")
	}
	if _, ok := sent[1]["message_reference"]; ok {
		t.Fatal("continuation lifecycle message unexpectedly included a reply reference")
	}

	updated := streamed + " continuation"
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: " continuation"})
	waitForDiscordEdit(t, rest, "sent-2", "second continuation"+discordStreamCursor)
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: updated}); err != nil {
		t.Fatal(err)
	}

	chunks := splitMessage(updated, 2000)
	if len(chunks) != 2 {
		t.Fatalf("chunk count=%d, want 2", len(chunks))
	}
	for i, chunk := range chunks {
		if discordContentLength(chunk) > 2000 {
			t.Fatalf("chunk %d exceeds Discord limit: %d", i, discordContentLength(chunk))
		}
		ctx, ok := dg.lookupReply(fmt.Sprintf("sent-%d", i+1))
		if !ok || ctx.Text != chunk {
			t.Fatalf("chunk %d reply context=%+v found=%t", i, ctx, ok)
		}
	}
}

func TestDiscordStreamDeletesSurplusContinuationWhenFinalResponseShrinks(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.stream.editInterval = 5 * time.Millisecond

	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: strings.Repeat("a", 2000) + "temporary continuation"})
	waitForDiscordMessages(t, rest, 2)

	const finalText = "Short authoritative response."
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: finalText}); err != nil {
		t.Fatal(err)
	}
	deleted := rest.deletedMessageIDs()
	if len(deleted) != 1 || deleted[0] != "sent-2" {
		t.Fatalf("deleted lifecycle messages=%v, want [sent-2]", deleted)
	}
	ctx, ok := dg.lookupReply("sent-1")
	if !ok || ctx.Text != finalText {
		t.Fatalf("final reply context=%+v found=%t", ctx, ok)
	}
	if _, ok := dg.lookupReply("sent-2"); ok {
		t.Fatal("surplus continuation was recorded as authoritative reply context")
	}
}

func TestDiscordStreamRemovesAbandonedContinuationBeforeToolProgress(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.stream.editInterval = 5 * time.Millisecond

	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: strings.Repeat("a", 2000) + "abandoned continuation"})
	waitForDiscordMessages(t, rest, 2)
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolCall, Tool: &agent.ToolStreamPayload{Name: "web.search"}})
	waitForDiscordDeletion(t, rest, "sent-2")
	waitForDiscordEdit(t, rest, "sent-1", "Searching the web for \"the requested information\"...")

	const finalText = "Final answer after the tool call."
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Tool: &agent.ToolStreamPayload{Name: "web.search"}})
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: finalText})
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: finalText}); err != nil {
		t.Fatal(err)
	}
	deleted := rest.deletedMessageIDs()
	if len(deleted) != 1 || deleted[0] != "sent-2" {
		t.Fatalf("deleted lifecycle messages=%v, want [sent-2]", deleted)
	}
}

func TestDiscordEditMessageRetriesRateLimit(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after":0.001}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sent-1"}`))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError)}
	if err := dg.editMessage("channel-1", "sent-1", "final"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2", attempts)
	}
}

func TestDiscordStreamFallsBackWhenFinalEditFails(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		mu.Lock()
		posts = append(posts, payload["content"].(string))
		id := len(posts)
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":"sent-%d"}`, id)
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "A response preview that is long enough to send."})

	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: "The authoritative final answer."}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || posts[1] != "The authoritative final answer." {
		t.Fatalf("unexpected fallback posts: %q", posts)
	}
}

func TestDiscordStreamReportsStaleLifecycleCleanupFailure(t *testing.T) {
	postCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postCount++
			_, _ = fmt.Fprintf(w, `{"id":"sent-%d"}`, postCount)
		case http.MethodPatch, http.MethodDelete:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "temporary thinking"})

	err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: "The authoritative final answer."})
	if err == nil || postCount != 2 {
		t.Fatalf("error=%v post_count=%d, want cleanup error and fallback answer", err, postCount)
	}
}

func TestDiscordStreamReportsStaleLifecycleErrorCleanupFailure(t *testing.T) {
	postCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postCount++
			_, _ = fmt.Fprintf(w, `{"id":"sent-%d"}`, postCount)
		case http.MethodPatch, http.MethodDelete:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "temporary thinking"})

	err := r.SendAgentError("Safe error response.")
	if err == nil || postCount != 2 {
		t.Fatalf("error=%v post_count=%d, want cleanup error and fallback error response", err, postCount)
	}
}

func TestDiscordStreamEditsActualContentThroughToolPhases(t *testing.T) {
	rest := newFakeDiscordREST(t)
	defer rest.server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: rest.server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.stream.editInterval = 5 * time.Millisecond

	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "First thinking paragraph."})
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolCall, Tool: &agent.ToolStreamPayload{Name: "web.search"}})
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Tool: &agent.ToolStreamPayload{Name: "web.search", DurationMS: 25}})
	r.Stream(agent.StreamChunk{Type: agent.ChunkThinking, Text: "Second thinking paragraph."})
	time.Sleep(20 * time.Millisecond)
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "Authoritative content"})
	time.Sleep(20 * time.Millisecond)
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: "Authoritative content complete."}); err != nil {
		t.Fatal(err)
	}

	if len(rest.sentMessages()) != 1 {
		t.Fatalf("expected one lifecycle message, got %+v", rest.sentMessages())
	}
	if rest.sentMessages()[0]["content"] != "-# First thinking paragraph."+discordStreamCursor {
		t.Fatalf("unexpected first thinking state: %+v", rest.sentMessages())
	}
	edited := rest.editedMessages()
	expectedEdits := []string{
		"-# First thinking paragraph.\n\nSearching the web for \"the requested information\"...",
		"Searched the web for \"the requested information\".",
		"-# Second thinking paragraph." + discordStreamCursor,
		strings.TrimSpace(discordStreamCursor),
		"Authoritative content" + discordStreamCursor,
		"Authoritative content complete.",
	}
	if len(edited) != len(expectedEdits) {
		t.Fatalf("edit count=%d, want %d: %+v", len(edited), len(expectedEdits), edited)
	}
	for i, expected := range expectedEdits {
		if edited[i]["content"] != expected {
			t.Fatalf("edit %d=%q, want %q", i, edited[i]["content"], expected)
		}
	}
	for _, messageID := range rest.editedMessageIDs() {
		if messageID != "sent-1" {
			t.Fatalf("edit targeted %q, want sent-1", messageID)
		}
	}
}

func TestDiscordStreamReportsFinalContinuationFailure(t *testing.T) {
	postCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			_, _ = w.Write([]byte(`{"id":"sent-1"}`))
			return
		}
		postCount++
		if postCount > 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"id":"sent-1"}`))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	responseText := strings.Repeat("long response text ", 260)
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: responseText})

	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: responseText}); err == nil {
		t.Fatal("expected continuation delivery failure")
	}
	first, ok := dg.lookupReply("sent-1")
	if !ok || first.Text != splitMessage(responseText, 2000)[0] {
		t.Fatalf("successful first chunk was not remembered: %+v found=%t", first, ok)
	}
}

func TestDiscordStreamRecoversFinalAnswerAfterStatusEditFailure(t *testing.T) {
	postCount := 0
	patchCount := 0
	deleteCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postCount++
			_, _ = fmt.Fprintf(w, `{"id":"sent-%d"}`, postCount)
		case http.MethodPatch:
			patchCount++
			if patchCount <= 2 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
		case http.MethodDelete:
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	r := newRuntimeResponder(dg, "req-1", "channel-1", "message-1", "discord:dm:123", "123")
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "A commentary preview that is long enough to send."})
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolCall, Tool: &agent.ToolStreamPayload{Name: "web.search"}})
	r.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Tool: &agent.ToolStreamPayload{Name: "web.search", DurationMS: 10}})
	r.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "The final response is long enough to preview."})
	if err := r.SendAgentResponse(&agent.AgentResponse{Model: "test-model", Response: "The final response is long enough to preview."}); err != nil {
		t.Fatal(err)
	}
	if deleteCount != 0 || postCount != 1 || patchCount != 3 {
		t.Fatalf("delete_count=%d post_count=%d patch_count=%d, want 0, 1, and 3", deleteCount, postCount, patchCount)
	}
}

func TestRuntimeInvalidationPurgesOnlyMatchingDiscordReplyContext(t *testing.T) {
	dg := &Gateway{replyIndex: map[string]replyContext{
		"session": {SessionKey: "discord:channel:one", SenderID: "one"},
		"sender":  {SessionKey: "discord:other:one", SenderID: "one"},
		"foreign": {SessionKey: "discord:channel:two", SenderID: "two"},
	}}
	dg.HandleRuntimeInvalidation(runtimeinvalidation.Event{SessionIDs: []string{"discord:channel:one"}, ExternalIdentities: []string{"discord:one", "imessage:one"}})
	if _, ok := dg.replyIndex["session"]; ok {
		t.Fatal("matching session reply context remained")
	}
	if _, ok := dg.replyIndex["sender"]; ok {
		t.Fatal("matching sender reply context remained")
	}
	if _, ok := dg.replyIndex["foreign"]; !ok || len(dg.replyIndex) != 1 {
		t.Fatalf("foreign reply context was purged: %+v", dg.replyIndex)
	}
}

func TestDiscordCommandAttachmentMultipart(t *testing.T) {
	var payloads []map[string]any
	var filenames, mimeTypes []string
	var attachmentData [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/channel-1/messages" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		var payload map[string]any
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(commands.MaxAttachmentBytes + 4096); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
				t.Fatalf("decode payload_json: %v", err)
			}
			file, header, err := r.FormFile("files[0]")
			if err != nil {
				t.Fatalf("read file part: %v", err)
			}
			data, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			filenames = append(filenames, header.Filename)
			mimeTypes = append(mimeTypes, header.Header.Get("Content-Type"))
			attachmentData = append(attachmentData, data)
		} else {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode message payload: %v", err)
			}
		}
		payloads = append(payloads, payload)
		_, _ = w.Write([]byte(`{"id":"sent-attachment"}`))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError)}
	responder := runtimeResponder{gateway: dg, channelID: "channel-1", replyToID: "message-1"}
	err := responder.SendCommandResponse(commands.Result{Text: "export ready", Attachments: []commands.Attachment{
		{Filename: "export.json.part001", MIMEType: "application/octet-stream", Data: []byte("first")},
		{Filename: "export.json.part002", MIMEType: "application/octet-stream", Data: []byte("second")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 3 {
		t.Fatalf("request payload count=%d, want 3", len(payloads))
	}
	reference, _ := payloads[0]["message_reference"].(map[string]any)
	firstMetadata, _ := payloads[0]["attachments"].([]any)
	secondMetadata, _ := payloads[1]["attachments"].([]any)
	if reference["message_id"] != "message-1" || len(firstMetadata) != 1 || len(secondMetadata) != 1 || payloads[2]["content"] != "export ready" || len(filenames) != 2 || filenames[0] != "export.json.part001" || filenames[1] != "export.json.part002" || mimeTypes[0] != "application/octet-stream" || string(attachmentData[0]) != "first" || string(attachmentData[1]) != "second" {
		t.Fatalf("unexpected payloads=%+v filenames=%q mime=%q data=%q", payloads, filenames, mimeTypes, attachmentData)
	}
}

func TestDiscordCommandAttachmentProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private-content", http.StatusBadGateway)
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError)}
	_, err := dg.sendCommandAttachment("channel-1", commands.Result{Attachments: []commands.Attachment{{
		Filename: "export.json", MIMEType: "application/json", Data: []byte("private-content"),
	}}}, "")
	if err == nil || strings.Contains(err.Error(), "private-content") {
		t.Fatalf("unexpected provider error: %v", err)
	}
}

func TestDiscordAgentResponseDeliversAttachmentBeforeFinalText(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(commands.MaxAttachmentBytes + 4096); err != nil {
				t.Fatal(err)
			}
			file, header, err := r.FormFile("files[0]")
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(file)
			_ = file.Close()
			calls = append(calls, "attachment:"+header.Filename+":"+string(data))
		} else {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			calls = append(calls, "text:"+payload["content"].(string))
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"message-%d"}`, len(calls))))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	responder := newRuntimeResponder(dg, "request", "channel-1", "source-1", "session", "user")
	err := responder.SendAgentResponse(&agent.AgentResponse{Response: "generated", Attachments: []media.OutputAttachment{{Filename: "generated.png", MIMEType: "image/png", Data: []byte("image-data")}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"attachment:generated.png:image-data", "text:generated"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestDiscordStreamsAttachmentBeforeRequestingFinalText(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	attachmentStarted := make(chan struct{})
	releaseAttachment := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(commands.MaxAttachmentBytes + 4096); err != nil {
				t.Error(err)
			}
			file, header, err := r.FormFile("files[0]")
			if err != nil {
				t.Error(err)
			} else {
				_ = file.Close()
			}
			mu.Lock()
			calls = append(calls, "attachment:"+header.Filename)
			mu.Unlock()
			close(attachmentStarted)
			<-releaseAttachment
		} else {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			mu.Lock()
			calls = append(calls, strings.ToLower(r.Method)+":"+fmt.Sprint(payload["content"]))
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"id":"message-1"}`))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	responder := newRuntimeResponder(dg, "request", "channel-1", "source-1", "session", "user")
	attachment := media.OutputAttachment{Filename: "generated.png", MIMEType: "image/png", Data: []byte("image-data")}
	streamReturned := make(chan struct{})
	go func() {
		responder.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Tool: &agent.ToolStreamPayload{Name: "comfyui.text_to_image"}, Attachments: []media.OutputAttachment{attachment}})
		close(streamReturned)
	}()
	<-attachmentStarted
	select {
	case <-streamReturned:
		t.Fatal("stream callback returned before Discord completed the attachment request")
	default:
	}
	close(releaseAttachment)
	<-streamReturned

	responder.Stream(agent.StreamChunk{Type: agent.ChunkContent, Text: "generated"})
	if err := responder.SendAgentResponse(&agent.AgentResponse{Response: "generated", Attachments: []media.OutputAttachment{attachment}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	attachmentCount := 0
	finalTextIndex := -1
	for i, call := range calls {
		if strings.HasPrefix(call, "attachment:") {
			attachmentCount++
		}
		if strings.HasSuffix(call, ":generated") {
			finalTextIndex = i
		}
	}
	if len(calls) == 0 || calls[0] != "attachment:generated.png" || attachmentCount != 1 || finalTextIndex <= 0 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestDiscordDoesNotRetryFailedStreamAttachmentAtFinalDelivery(t *testing.T) {
	attachmentCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			attachmentCalls++
			http.Error(w, "provider body", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"id":"message-1"}`))
	}))
	defer server.Close()
	dg := &Gateway{Token: "token", APIBaseURL: server.URL, Log: config.NewLogger(config.LevelError), replyIndex: make(map[string]replyContext)}
	responder := newRuntimeResponder(dg, "request", "channel-1", "source-1", "session", "user")
	attachment := media.OutputAttachment{Filename: "generated.png", MIMEType: "image/png", Data: []byte("image-data")}
	responder.Stream(agent.StreamChunk{Type: agent.ChunkToolResult, Attachments: []media.OutputAttachment{attachment}})
	if err := responder.SendAgentResponse(&agent.AgentResponse{Response: "generated", Attachments: []media.OutputAttachment{attachment}}); err == nil {
		t.Fatal("streamed attachment failure was not returned at final delivery")
	}
	if attachmentCalls != 1 {
		t.Fatalf("attachment calls=%d, want 1", attachmentCalls)
	}
}

func TestDiscordIgnoresUnmentionedGuildMessage(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	dg.handleMessage(discordMessage("msg-1", "channel-1", "guild-1", "123", "Alice", "hello guild"))

	if len(primaryDiscordRequests(chat.requests)) != 0 {
		t.Fatalf("expected no LLM request, got %d", len(primaryDiscordRequests(chat.requests)))
	}
	if len(rest.sentMessages()) != 0 {
		t.Fatalf("expected no sent messages, got %+v", rest.sentMessages())
	}
}

func TestDiscordMentionedGuildMessageStripsMentionAndResolvesMentions(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()

	msg := discordMessage("msg-1", "channel-1", "guild-1", "123", "Alice", "<@bot-1> hello <@456>")
	msg.Mentions = []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}{{ID: "456", Username: "Bob"}}
	dg.handleMessage(msg)

	primary := primaryDiscordRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if prompt != "hello @Bob" || !strings.Contains(primary[0].Messages[len(primary[0].Messages)-2].Content, "<tenant_profile") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestDiscordReplyToBotIncludesReplyContext(t *testing.T) {
	rest := newFakeDiscordREST(t)
	dg, b, chat := newDiscordTestGateway(t, rest.server.URL)
	defer b.Shutdown()
	defer rest.server.Close()
	dg.rememberReply("bot-msg", replyContext{SessionKey: "discord:channel-1:123", ChannelID: "channel-1", DisplayName: "Oswald", Text: "prior answer", IsFromBot: true, CreatedAt: time.Now()})

	msg := discordMessage("msg-2", "channel-1", "guild-1", "123", "Alice", "follow up")
	msg.ReferencedMessage = &struct {
		ID          string       `json:"id"`
		Content     string       `json:"content"`
		Attachments []Attachment `json:"attachments,omitempty"`
		Embeds      []Embed      `json:"embeds,omitempty"`
		Author      struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"author"`
	}{ID: "bot-msg"}
	msg.ReferencedMessage.Author.ID = "bot-1"
	msg.ReferencedMessage.Author.Username = "Oswald"
	dg.handleMessage(msg)

	primary := primaryDiscordRequests(chat.requests)
	if len(primary) != 1 {
		t.Fatalf("expected one LLM request, got %d", len(primary))
	}
	prompt := primary[0].Messages[len(primary[0].Messages)-1].Content
	if !strings.Contains(prompt, "[Replying to Oswald: \"prior answer\"]") || !strings.Contains(prompt, "follow up") {
		t.Fatalf("unexpected prompt %q", prompt)
	}
}

func TestDiscordHelpers(t *testing.T) {
	chunks := splitMessage("one. two three\nfour", 10)
	if len(chunks) != 3 || chunks[0] != "one." || chunks[1] != "two three" || chunks[2] != "four" {
		t.Fatalf("unexpected chunks: %+v", chunks)
	}
	mentions := []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}{{ID: "123", Username: "Alice"}}
	if got := resolveMentions("hi <@123> and <@!999>", mentions); got != "hi @Alice and <@!999>" {
		t.Fatalf("unexpected resolved mentions %q", got)
	}
	if resolveGatewayURL("gateway.discord.gg") != "wss://gateway.discord.gg/?v=10&encoding=json" {
		t.Fatal("unexpected resolved gateway URL")
	}
	preview := discordPreviewText("Here is code:\n```go\nfmt.Println(\"hello\")")
	if preview != "Here is code:\n```go\nfmt.Println(\"hello\")\n```"+discordStreamCursor {
		t.Fatalf("unbalanced streaming preview %q", preview)
	}
	emojiChunks := splitMessage(strings.Repeat("😀", 1001), 2000)
	if len(emojiChunks) != 2 || discordContentLength(emojiChunks[0]) > 2000 || discordContentLength(emojiChunks[1]) > 2000 {
		t.Fatalf("emoji response was not split by Discord units: lengths=%d,%d", discordContentLength(emojiChunks[0]), discordContentLength(emojiChunks[1]))
	}
}

func TestDiscordThinkingShowsOnlyLatestParagraphAndBoundsLongThought(t *testing.T) {
	thinking := "First paragraph should disappear.\n\nSecond paragraph stays visible."
	if got := latestThinkingParagraph(thinking); got != "Second paragraph stays visible." {
		t.Fatalf("latest paragraph = %q", got)
	}
	rendered := formatThinkingParagraph(strings.Repeat("reasoning 😀 ", 500), false, true, discordThinkingRenderLimit)
	if discordContentLength(rendered) > discordThinkingRenderLimit || !strings.Contains(rendered, "[earlier part omitted]") {
		t.Fatalf("long thinking was not bounded: units=%d text=%q", discordContentLength(rendered), rendered)
	}
	if discordContentLength(rendered) < 1950 || !strings.HasSuffix(rendered, strings.TrimSpace(discordStreamCursor)) {
		t.Fatalf("thinking did not use the available Discord budget: units=%d text=%q", discordContentLength(rendered), rendered)
	}
	status := "Searching the web for \"a detailed query\"..."
	combined := composeRunningToolStatus(strings.Repeat("prior thinking 😀 ", 500), false, status)
	if discordContentLength(combined) > 2000 || discordContentLength(combined) < 1950 || !strings.HasSuffix(combined, status) || !strings.Contains(combined, "[earlier part omitted]") {
		t.Fatalf("running tool composition did not prioritize status and fill budget: units=%d text=%q", discordContentLength(combined), combined)
	}
}

func TestDiscordMCPToolStatusFormatsSafePrimitiveArguments(t *testing.T) {
	status := discordToolStatusFor(&agent.ToolStreamPayload{
		Name: "github.list_issues",
		Arguments: map[string]interface{}{
			"state":         "open",
			"repo":          "api",
			"owner":         "acme",
			"authorization": "Bearer private-token",
			"auth":          "private-auth",
			"bearer":        "private-bearer",
			"jwt":           "private-jwt",
			"session_id":    "private-session",
			"x-api-key":     "private-key",
			"privateKey":    "private-camel-key",
			"sshKey":        "private-ssh-key",
			"passphrase":    "private-passphrase",
			"nested":        map[string]interface{}{"hidden": true},
		},
	})
	if status.running != "Running `github.list_issues` with owner=\"acme\", repo=\"api\", state=\"open\"..." {
		t.Fatalf("unexpected MCP status %q", status.running)
	}
	if strings.Contains(status.running, "private-") || strings.Contains(status.running, "nested") || strings.Contains(status.completed, " ms") {
		t.Fatalf("unsafe MCP status: %+v", status)
	}
}

func TestDiscordEmbedIntentionalMediaClassification(t *testing.T) {
	tests := []struct {
		embedType string
		want      bool
	}{
		{embedType: "image", want: true},
		{embedType: " IMAGE ", want: true},
		{embedType: "gifv", want: true},
		{embedType: "GIFV", want: true},
		{embedType: ""},
		{embedType: "article"},
		{embedType: "link"},
		{embedType: "rich"},
		{embedType: "video"},
		{embedType: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.embedType, func(t *testing.T) {
			if got := discordEmbedIsIntentionalMedia(Embed{Type: test.embedType}); got != test.want {
				t.Fatalf("discordEmbedIsIntentionalMedia(%q) = %t, want %t", test.embedType, got, test.want)
			}
		})
	}
}

func TestDiscordWebpagePreviewEmbedsAreIgnoredAndURLPreserved(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testDiscordJPEG(t))
	}))
	defer server.Close()
	dg := &Gateway{HTTPClient: server.Client(), Log: config.NewLogger(config.LevelError)}
	const pageURL = "https://docs.example.com/guide"
	for _, embedType := range []string{"article", "link", "rich"} {
		embed := Embed{
			Type: embedType, URL: pageURL,
			Image: EmbedImage{ProxyURL: server.URL + "/image.jpg"}, Thumbnail: EmbedImage{URL: server.URL + "/thumbnail.jpg"},
		}
		images, unsupported := dg.loadEmbedImagesLimit([]Embed{embed}, 1)
		if len(images) != 0 || len(unsupported) != 0 {
			t.Fatalf("%s preview produced images=%d unsupported=%v", embedType, len(images), unsupported)
		}
		if labels := discordEmbedLabels([]Embed{embed}); len(labels) != 0 {
			t.Fatalf("%s preview labels = %v", embedType, labels)
		}
		if got := stripEmbedURLsFromText("Please inspect "+pageURL, []Embed{embed}); got != "Please inspect "+pageURL {
			t.Fatalf("%s preview text = %q", embedType, got)
		}
	}
	if requestCount != 0 {
		t.Fatalf("webpage previews triggered %d media requests", requestCount)
	}
}

func TestDiscordDirectImageEmbedRemainsIntentionalMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testDiscordJPEG(t))
	}))
	defer server.Close()
	dg := &Gateway{HTTPClient: server.Client(), Log: config.NewLogger(config.LevelError)}
	const imageURL = "https://images.example.com/photo.jpg"
	embed := Embed{Type: "image", URL: imageURL, Image: EmbedImage{ProxyURL: server.URL + "/photo.jpg"}}
	images, unsupported := dg.loadEmbedImagesLimit([]Embed{embed}, 1)
	if len(images) != 1 || len(unsupported) != 0 {
		t.Fatalf("images=%d unsupported=%v", len(images), unsupported)
	}
	if got := stripEmbedURLsFromText(imageURL, []Embed{embed}); got != "" {
		t.Fatalf("direct image URL was not stripped: %q", got)
	}
}

func TestDiscordMixedEmbedsStripOnlyDirectMediaURL(t *testing.T) {
	const pageURL = "https://docs.example.com/guide"
	const imageURL = "https://images.example.com/photo.jpg"
	embeds := []Embed{
		{Type: "article", URL: pageURL, Thumbnail: EmbedImage{URL: "https://cdn.example.com/preview.jpg"}},
		{Type: "image", URL: imageURL, Image: EmbedImage{URL: "https://cdn.example.com/photo.jpg"}},
	}
	text := "Compare " + pageURL + " " + imageURL
	if got := stripEmbedURLsFromText(text, embeds); got != "Compare "+pageURL {
		t.Fatalf("mixed embed text = %q", got)
	}
	if labels := discordEmbedLabels(embeds); len(labels) != 1 {
		t.Fatalf("mixed embed labels = %v", labels)
	}
}

func TestDiscordGIFVEmbedPrefersAnimatedVideo(t *testing.T) {
	videoPayload := []byte("mock mp4")
	requestedThumbnail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video.mp4":
			_, _ = w.Write(videoPayload)
		case "/thumbnail.jpg":
			requestedThumbnail = true
			_, _ = w.Write(testDiscordJPEG(t))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	extractor := &fakeVideoFrameExtractor{image: llm.InputImage{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(testDiscordJPEG(t))}}
	dg := &Gateway{HTTPClient: server.Client(), VideoFrames: extractor, Log: config.NewLogger(config.LevelError)}
	embed := Embed{
		Type:      "gifv",
		Video:     EmbedImage{ProxyURL: server.URL + "/video.mp4"},
		Thumbnail: EmbedImage{ProxyURL: server.URL + "/thumbnail.jpg"},
	}

	images, unsupported := dg.loadEmbedImagesLimit([]Embed{embed}, 1)
	if len(images) != 1 || len(unsupported) != 0 {
		t.Fatalf("images=%d unsupported=%v", len(images), unsupported)
	}
	if !bytes.Equal(extractor.data, videoPayload) {
		t.Fatalf("extractor payload = %q, want %q", extractor.data, videoPayload)
	}
	if requestedThumbnail {
		t.Fatal("static thumbnail was requested after successful video extraction")
	}
}

func TestDiscordGIFVEmbedFallsBackToStaticThumbnail(t *testing.T) {
	requestedThumbnail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video.mp4":
			_, _ = w.Write([]byte("mock mp4"))
		case "/thumbnail.jpg":
			requestedThumbnail = true
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(testDiscordJPEG(t))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dg := &Gateway{
		HTTPClient:  server.Client(),
		VideoFrames: &fakeVideoFrameExtractor{err: errors.New("ffmpeg unavailable")},
		Log:         config.NewLogger(config.LevelError),
	}
	embed := Embed{
		Type:      "gifv",
		Video:     EmbedImage{URL: server.URL + "/video.mp4"},
		Thumbnail: EmbedImage{URL: server.URL + "/thumbnail.jpg"},
	}

	images, unsupported := dg.loadEmbedImagesLimit([]Embed{embed}, 1)
	if len(images) != 1 || len(unsupported) != 0 || !requestedThumbnail {
		t.Fatalf("images=%d unsupported=%v thumbnail_requested=%t", len(images), unsupported, requestedThumbnail)
	}
}

func TestDiscordGIFVEmbedJSONIncludesVideo(t *testing.T) {
	var embed Embed
	err := json.Unmarshal([]byte(`{"type":"gifv","video":{"url":"https://cdn.example/video.mp4","proxy_url":"https://proxy.example/video.mp4","width":480,"height":270}}`), &embed)
	if err != nil {
		t.Fatal(err)
	}
	if got := discordEmbedVideoURL(embed); got != "https://proxy.example/video.mp4" {
		t.Fatalf("video URL = %q", got)
	}
}

type fakeVideoFrameExtractor struct {
	data  []byte
	image llm.InputImage
	err   error
}

func (f *fakeVideoFrameExtractor) Extract(_ context.Context, data []byte, _ string) (llm.InputImage, error) {
	f.data = append([]byte(nil), data...)
	return f.image, f.err
}

func testDiscordJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 80, B: 120, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

type discordFakeChatter struct {
	mu           sync.Mutex
	requests     []llm.ChatRequest
	principal    identity.Principal
	streamChunks []llm.ChatMessage
	response     string
}

func (f *discordFakeChatter) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.principal, _ = requestctx.PrincipalFromContext(ctx)
	streamChunks := append([]llm.ChatMessage(nil), f.streamChunks...)
	response := f.response
	f.mu.Unlock()
	if req.Format == "json_object" {
		return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: `{"session_updates":{"summary":"","open_threads":[],"decisions":[],"user_goals":[]},"memory_candidates":[]}`}}, nil
	}
	if req.Stream && cb != nil {
		for _, chunk := range streamChunks {
			cb(chunk)
		}
	}
	if response == "" {
		response = "discord response"
	}
	return &llm.ChatResponse{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: response}}, nil
}

func (f *discordFakeChatter) lastPrincipal() identity.Principal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.principal
}

func primaryDiscordRequests(requests []llm.ChatRequest) []llm.ChatRequest {
	out := make([]llm.ChatRequest, 0, len(requests))
	for _, req := range requests {
		if req.Format != "json_object" {
			out = append(out, req)
		}
	}
	return out
}

type fakeDiscordREST struct {
	server  *httptest.Server
	mu      sync.Mutex
	sent    []map[string]interface{}
	edited  []map[string]interface{}
	editIDs []string
	deleted []string
	nextID  int
}

func newFakeDiscordREST(t *testing.T) *fakeDiscordREST {
	t.Helper()
	rest := &fakeDiscordREST{}
	rest.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode discord message payload: %v", err)
			}
			rest.mu.Lock()
			rest.sent = append(rest.sent, payload)
			rest.nextID++
			id := rest.nextID
			rest.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"id":"sent-%d"}`, id)
			return
		}
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/messages/") {
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode discord edit payload: %v", err)
			}
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			rest.mu.Lock()
			rest.edited = append(rest.edited, payload)
			rest.editIDs = append(rest.editIDs, id)
			rest.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
			return
		}
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/messages/") {
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			rest.mu.Lock()
			rest.deleted = append(rest.deleted, id)
			rest.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/messages/") {
			_, _ = w.Write([]byte(`{"id":"fetched","content":"fetched reply","author":{"id":"bot-1","username":"Oswald"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	return rest
}

func (r *fakeDiscordREST) sentMessages() []map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]interface{}(nil), r.sent...)
}

func (r *fakeDiscordREST) editedMessages() []map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]interface{}(nil), r.edited...)
}

func (r *fakeDiscordREST) editedMessageIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.editIDs...)
}

func (r *fakeDiscordREST) deletedMessageIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

func waitForDiscordMessages(t *testing.T, rest *fakeDiscordREST, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(rest.sentMessages()) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("sent message count=%d, want at least %d", len(rest.sentMessages()), count)
}

func waitForDiscordEdit(t *testing.T, rest *fakeDiscordREST, messageID, content string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		messages := rest.editedMessages()
		ids := rest.editedMessageIDs()
		for i := range messages {
			if ids[i] == messageID && messages[i]["content"] == content {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("edit for %s with content %q was not observed", messageID, content)
}

func waitForDiscordDeletion(t *testing.T, rest *fakeDiscordREST, messageID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, deletedID := range rest.deletedMessageIDs() {
			if deletedID == messageID {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("deletion for %s was not observed", messageID)
}

func (r *fakeDiscordREST) lastMessageContent() string {
	sent := r.sentMessages()
	if len(sent) == 0 {
		return ""
	}
	content, _ := sent[len(sent)-1]["content"].(string)
	return content
}

func newDiscordTestGateway(t *testing.T, apiBaseURL string) (*Gateway, *broker.Broker, *discordFakeChatter) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	memories := testutil.NewMemoryStore(t, dbPath, log)
	links := accountlinking.NewService(dbPath, memories, nil, log)
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0o600); err != nil {
		t.Fatalf("write soul fixture: %v", err)
	}
	soulStore := soul.NewStore(soulPath)
	chat := &discordFakeChatter{}
	ai := agent.NewAgent(chat, registry.New(log), "test-model", soulStore, memories, promptbudget.ContextBudget{PromptLimit: 100000}, governance.GlobalPolicy{MaxExecutions: 12, MaxToolIterations: 8}, log)
	b := broker.NewBroker(ai, 1, log)
	b.Start()
	commandService, err := commands.NewServiceWithCommands()
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	dg := &Gateway{
		Token:      "token",
		BotID:      "bot-1",
		Broker:     b,
		Links:      links,
		Runtime:    gatewayruntime.Dependencies{Commands: commandService, Log: log},
		Log:        log,
		APIBaseURL: apiBaseURL,
		replyIndex: make(map[string]replyContext),
		hbAcked:    true,
	}
	return dg, b, chat
}

func discordMessage(id, channelID, guildID, authorID, username, content string) MessageCreate {
	var msg MessageCreate
	msg.ID = id
	msg.ChannelID = channelID
	msg.GuildID = guildID
	msg.Content = content
	msg.Author.ID = authorID
	msg.Author.Username = username
	return msg
}
