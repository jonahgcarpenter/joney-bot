package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const reflectedSoulCanary = "SOUL_CANARY_DO_NOT_LOG_OR_RETURN"

func TestGatewayClientChatPostsRequestAndParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/async/chat/completions" && r.URL.Path != "/v1/async/chat/completions/job-1" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-bf-vk"); got != "virtual-key" {
			t.Fatalf("x-bf-vk = %q", got)
		}
		if r.Method == http.MethodPost {
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if body["model"] != "test-model" || body["stream"] != false {
				t.Fatalf("unexpected request body: %+v", body)
			}
			for _, field := range []string{"tool_choice", "parallel_tool_calls", "temperature", "max_tokens"} {
				if _, ok := body[field]; ok {
					t.Fatalf("ordinary request included %s: %+v", field, body)
				}
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job-1","status":"pending","created_at":"2026-01-01T00:00:00Z"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"job-1","status":"completed","status_code":200,"result":{
			"model":"served-model",
			"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}}`))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL+"/", "api-key", "virtual-key", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "test-model", Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Model != "served-model" || resp.Message.Content != "hello" || resp.PromptTokens != 3 || resp.TotalTokens != 5 || resp.DoneReason != "stop" {
		t.Fatalf("unexpected Chat response: %+v", resp)
	}
}

func TestGatewayClientChatSerializesForcedRecursiveToolSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"job-1","status":"completed","status_code":200,"result":{"choices":[{"message":{"role":"assistant","content":"ok"}}]}}`))
			return
		}
		var body struct {
			Tools             []Tool     `json:"tools"`
			ToolChoice        ToolChoice `json:"tool_choice"`
			ParallelToolCalls *bool      `json:"parallel_tool_calls"`
			Temperature       *float64   `json:"temperature"`
			MaxTokens         int        `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 1 || body.Tools[0].Function.Name != "user_memory_save" || body.ToolChoice != ToolChoiceRequired || body.ParallelToolCalls == nil || *body.ParallelToolCalls {
			t.Fatalf("unexpected forced tool request: %+v", body)
		}
		if body.Temperature == nil || *body.Temperature != 0 || body.MaxTokens != 2048 {
			t.Fatalf("unexpected deterministic controls: %+v", body)
		}
		memories := body.Tools[0].Function.Parameters.Properties["memories"]
		if memories.Items == nil || memories.MaxItems == nil || *memories.MaxItems != 5 || memories.AdditionalProperties != nil {
			t.Fatalf("unexpected recursive schema: %+v", memories)
		}
		if memories.Items.AdditionalProperties == nil || *memories.Items.AdditionalProperties {
			t.Fatalf("item additionalProperties was not false: %+v", memories.Items)
		}
		importance := memories.Items.Properties["importance"]
		if importance.Minimum == nil || importance.Maximum == nil || *importance.Minimum != 1 || *importance.Maximum != 5 {
			t.Fatalf("importance range was not serialized: %+v", importance)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"job-1","status":"pending"}`))
	}))
	defer server.Close()

	maximum := 5
	minimumImportance, maximumImportance := 1.0, 5.0
	additional := false
	tool := Tool{Type: "function", Function: ToolDefinition{Name: "user_memory_save", Parameters: ToolParameters{
		Type: "object", Properties: map[string]ToolParameterProperty{"memories": {
			Type: "array", MaxItems: &maximum, Items: &ToolParameterProperty{Type: "object", AdditionalProperties: &additional, Properties: map[string]ToolParameterProperty{
				"importance": {Type: "integer", Minimum: &minimumImportance, Maximum: &maximumImportance},
			}},
		}},
	}}}
	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	parallelToolCalls := false
	temperature := 0.0
	_, err := client.Chat(context.Background(), ChatRequest{Model: "model", Tools: []Tool{tool}, ToolChoice: ToolChoiceRequired, ParallelToolCalls: &parallelToolCalls, Temperature: &temperature, MaxTokens: 2048}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestGatewayClientChatHTTPAndDecodeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "") {
		}
		switch r.Header.Get("x-test-case") {
		case "bad-json":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`not-json`))
		default:
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("Chat HTTP error = %v, want HTTP 502", err)
	}

	client.HTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.Header.Set("x-test-case", "bad-json")
		return http.DefaultTransport.RoundTrip(req)
	})
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err == nil || !strings.Contains(err.Error(), "decode LLM gateway async response") {
		t.Fatalf("Chat decode error = %v, want async decode error", err)
	}
}

func TestGatewayClientAsyncChatPollsUntilCompleted(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job","status":"pending"}`))
			return
		}
		polls++
		if polls == 1 {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job","status":"processing"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"job","status":"completed","status_code":200,"result":{"choices":[{"message":{"role":"assistant","content":"complete"}}]}}`))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "model"}, nil)
	if err != nil || resp.Message.Content != "complete" || polls != 2 {
		t.Fatalf("response=%+v polls=%d err=%v", resp, polls, err)
	}
}

func TestGatewayClientAsyncChatMapsTerminalFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job","status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"job","status":"failed","status_code":429,"error":{"error":{"message":"busy"}}}`))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	_, err := client.Chat(context.Background(), ChatRequest{Model: "model"}, nil)
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error=%T %v, want typed 429", err, err)
	}
}

func TestGatewayClientAsyncChatHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			cancel()
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"job","status":"pending"}`))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	_, err := client.Chat(ctx, ChatRequest{Model: "model"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context cancellation", err)
	}
}

func TestGatewayClientChatReturnsTypedHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"XML syntax error on line 7: unexpected EOF %s"}}`, reflectedSoulCanary)))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	_, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
	var httpErr *ChatHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("Chat error = %T %v, want *ChatHTTPError", err, err)
	}
	if httpErr.StatusCode != http.StatusInternalServerError || !strings.Contains(httpErr.Body, "XML syntax error") || !strings.Contains(httpErr.Body, reflectedSoulCanary) {
		t.Fatalf("unexpected typed error: %+v", httpErr)
	}
	for label, text := range map[string]string{
		"error":      err.Error(),
		"safe error": config.SafeErrorText(err),
		"wrapped":    fmt.Errorf("model failed: %w", err).Error(),
	} {
		if strings.Contains(text, reflectedSoulCanary) {
			t.Fatalf("%s exposed reflected provider content: %q", label, text)
		}
	}
	if !IsTemporaryOllamaToolParserError(err) {
		t.Fatalf("expected temporary parser error classification: %v", err)
	}
}

func TestPermanentChatProviderErrorClassification(t *testing.T) {
	for _, test := range []struct {
		status int
		want   bool
	}{{http.StatusBadRequest, true}, {http.StatusUnauthorized, true}, {http.StatusNotFound, true}, {http.StatusRequestTimeout, false}, {http.StatusTooEarly, false}, {http.StatusTooManyRequests, false}, {http.StatusInternalServerError, false}} {
		err := &ChatHTTPError{StatusCode: test.status, Body: "provider content"}
		if got := IsPermanentChatProviderError(err); got != test.want {
			t.Fatalf("status=%d permanent=%v want=%v", test.status, got, test.want)
		}
	}
}

func TestGatewayClientProviderErrorsDoNotExposeResponseText(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		call    func(*GatewayClient) error
	}{
		{
			name: "chat response error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s"}}`, reflectedSoulCanary)))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
				return err
			},
		},
		{
			name: "chat stream response error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(fmt.Sprintf("data: {\"error\":{\"message\":%q}}\n\n", reflectedSoulCanary)))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Chat(context.Background(), ChatRequest{Model: "m", Stream: true}, func(ChatMessage) {})
				return err
			},
		},
		{
			name: "embed HTTP error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(reflectedSoulCanary))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Embed(context.Background(), EmbedRequest{Model: "m", Input: "input"})
				return err
			},
		},
		{
			name: "embed response error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s"}}`, reflectedSoulCanary)))
			},
			call: func(client *GatewayClient) error {
				_, err := client.Embed(context.Background(), EmbedRequest{Model: "m", Input: "input"})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			var err error
			logs := captureLogs(t, func(log *config.Logger) {
				client := newTestGatewayClient(server.URL, "", "", log)
				err = tt.call(client)
			})
			if err == nil {
				t.Fatal("expected provider error")
			}
			if strings.Contains(err.Error(), reflectedSoulCanary) || strings.Contains(config.SafeErrorText(err), reflectedSoulCanary) {
				t.Fatalf("provider error exposed reflected content: %q", err)
			}
			if strings.Contains(logs, reflectedSoulCanary) {
				t.Fatalf("provider logs exposed reflected content: %s", logs)
			}
			if !strings.Contains(logs, `"record_kind":"measurement"`) || !strings.Contains(logs, `.complete"`) {
				t.Fatalf("missing provider completion: %s", logs)
			}
		})
	}
}

func captureLogs(t *testing.T, run func(*config.Logger)) string {
	t.Helper()
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	run(log)
	return output.String()
}

func TestTemporaryOllamaToolParserErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "element type", err: &ChatHTTPError{StatusCode: 500, Body: `expected element type <function> but have <parameter>`}, want: true},
		{name: "xml syntax", err: &ChatHTTPError{StatusCode: 500, Body: `XML syntax error on line 7: unexpected EOF`}, want: true},
		{name: "unrelated 500", err: &ChatHTTPError{StatusCode: 500, Body: `out of memory`}, want: false},
		{name: "wrong status", err: &ChatHTTPError{StatusCode: 400, Body: `XML syntax error: unexpected EOF`}, want: false},
		{name: "ordinary error", err: errors.New("XML syntax error: unexpected EOF"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTemporaryOllamaToolParserError(tt.err); got != tt.want {
				t.Fatalf("IsTemporaryOllamaToolParserError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOllamaModelRunnerStoppedErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "runner stopped", err: &ChatHTTPError{StatusCode: 500, Body: `model runner has unexpectedly stopped, this may be due to resource limitations`}, want: true},
		{name: "unrelated 500", err: &ChatHTTPError{StatusCode: 500, Body: `out of memory`}, want: false},
		{name: "wrong status", err: &ChatHTTPError{StatusCode: 502, Body: `model runner has unexpectedly stopped`}, want: false},
		{name: "ordinary error", err: errors.New("model runner has unexpectedly stopped"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOllamaModelRunnerStoppedError(tt.err); got != tt.want {
				t.Fatalf("IsOllamaModelRunnerStoppedError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContextLengthExceededErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "maximum context length", err: &ChatHTTPError{StatusCode: 400, Body: "maximum context length exceeded"}, want: true},
		{name: "context window", err: fmt.Errorf("wrapped: %w", &ChatHTTPError{StatusCode: 413, Body: "request exceeds the context window"}), want: true},
		{name: "too many tokens", err: &ChatHTTPError{StatusCode: 400, Body: "too many tokens in prompt"}, want: true},
		{name: "unrelated rejection", err: &ChatHTTPError{StatusCode: 400, Body: "invalid tool schema"}, want: false},
		{name: "ordinary error", err: errors.New("context length exceeded"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsContextLengthExceededError(tt.err); got != tt.want {
				t.Fatalf("IsContextLengthExceededError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGatewayClientEmbedParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.URL.Path != "/v1/async/embeddings" {
				t.Fatalf("path = %q, want /v1/async/embeddings", r.URL.Path)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"embed-job","status":"pending"}`))
			return
		}
		if r.URL.Path != "/v1/async/embeddings/embed-job" {
			t.Fatalf("unexpected poll path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"embed-job","status":"completed","status_code":200,"result":{"model":"embed-model","data":[{"embedding":[1.5,2.5]}]}}`))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Embed(context.Background(), EmbedRequest{Model: "embed-model", Input: "text"})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if resp.Model != "embed-model" || len(resp.Embeddings) != 1 || len(resp.Embeddings[0]) != 2 || resp.Embeddings[0][1] != 2.5 {
		t.Fatalf("unexpected Embed response: %+v", resp)
	}
}

func TestGatewayClientChatStreamAccumulatesContentThinkingAndTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("stream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"stream-model\",\"choices\":[{\"delta\":{\"reasoning\":\"think\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"tool.name\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":8,\"total_tokens\":15}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	var chunks []ChatMessage
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "fallback-model", Stream: true}, func(chunk ChatMessage) {
		chunks = append(chunks, chunk)
	})
	if err != nil {
		t.Fatalf("stream Chat returned error: %v", err)
	}
	if resp.Model != "stream-model" || resp.Message.Thinking != "think" || resp.Message.Content != "hello" || resp.DoneReason != "stop" {
		t.Fatalf("unexpected stream response: %+v", resp)
	}
	if resp.PromptTokens != 7 || resp.CompletionTokens != 8 || resp.TotalTokens != 15 {
		t.Fatalf("unexpected stream usage: %+v", resp)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "call_1" || resp.Message.ToolCalls[0].Function.Arguments["x"] != float64(1) || resp.Message.ToolCalls[0].Function.RawArguments != `{"x":1}` {
		t.Fatalf("unexpected stream tool call: %+v", resp.Message.ToolCalls)
	}
	if len(chunks) != 2 || chunks[0].Thinking != "think" || chunks[1].Content != "hello" {
		t.Fatalf("unexpected stream chunks: %+v", chunks)
	}
}

func TestGatewayClientChatStreamSupportsNilCallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("stream path = %q", r.URL.Path)
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !request.Stream {
			t.Fatal("streaming request did not set stream=true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"memory.save\",\"arguments\":\"{\\\"ok\\\":\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"true}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "model", Stream: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Arguments["ok"] != true {
		t.Fatalf("tool calls=%+v", resp.Message.ToolCalls)
	}
}

func TestGatewayClientChatStreamCancellationStopsAcceptedRequest(t *testing.T) {
	established := make(chan struct{}, 1)
	disconnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning\":\"started\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(disconnected)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
	result := make(chan error, 1)
	go func() {
		_, err := client.Chat(ctx, ChatRequest{Model: "model", Stream: true}, func(ChatMessage) {
			select {
			case established <- struct{}{}:
			default:
			}
		})
		result <- err
	}()

	select {
	case <-established:
	case <-time.After(time.Second):
		t.Fatal("stream was not established")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream cancellation did not return promptly")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("server did not observe stream cancellation")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func newTestGatewayClient(baseURL, apiKey, virtualKey string, log *config.Logger) *GatewayClient {
	client := NewGatewayClient(baseURL, apiKey, virtualKey, log)
	client.pollInterval = time.Millisecond
	return client
}

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
