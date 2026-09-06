package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func measurementRecords(t *testing.T, output string, event string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid log: %v: %s", err, line)
		}
		if record["event"] == event {
			records = append(records, record)
		}
	}
	return records
}

func TestChatCompletionMeasurement(t *testing.T) {
	const canary = "private_unrecognized_provider_label"
	for _, test := range []struct {
		name, body, status      string
		reported, failed, first bool
		malformed               int
		tokens                  int
	}{
		{name: "zero", body: `data: {"choices":[{"delta":{"content":"private prose"},"finish_reason":"` + canary + `"}],"model":"` + canary + `","usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"unknown":"private prose"}` + "\n\ndata: [DONE]\n", status: "ok", reported: true, first: true},
		{name: "absent", body: "data: [DONE]\n", status: "ok"},
		{name: "null", body: "data: {\"usage\":null}\n\ndata: [DONE]\n", status: "ok"},
		{name: "empty", body: "data: {\"usage\":{}}\n\ndata: [DONE]\n", status: "ok"},
		{name: "malformed", body: "data: {private prose\n\ndata: {private prose\n\ndata: [DONE]\n", status: "degraded", malformed: 2},
		{name: "partial_failure", body: "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: {\"error\":{\"message\":\"private prose\"}}\n", status: "error", reported: true, failed: true, tokens: 5},
		{name: "split_usage_failure", body: "data: {\"usage\":{\"prompt_tokens\":3}}\n\ndata: {\"usage\":{\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: {\"error\":{\"message\":\"private prose\"}}\n", status: "error", reported: true, failed: true, tokens: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, test.body) }))
			defer server.Close()
			var output bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&output)
			collector := requestctx.NewUsageCollector()
			ctx := requestctx.WithUsageCollector(context.Background(), collector)
			ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req_1", OperationID: "parent_1", Workload: "foreground", SessionID: "private prose"})
			ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "usr_1", Gateway: "discord", ExternalID: "private prose"})
			response, err := NewGatewayClient(server.URL, "", "", log).Chat(ctx, ChatRequest{Model: "configured-model", Stream: true}, nil)
			if (err != nil) != test.failed || (test.failed && response != nil) {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			records := measurementRecords(t, output.String(), "provider.gateway.chat.complete")
			if len(records) != 1 {
				t.Fatalf("completion count=%d logs=%s", len(records), output.String())
			}
			r := records[0]
			if r["is_usage_complete"] != (test.reported && !test.failed) || r["is_usage_invalid"] != false {
				t.Errorf("usage metadata=%+v", r)
			}
			for key, want := range map[string]any{"level": "info", "record_kind": "measurement", "operation": "chat", "transport": "streaming", "status": test.status, "is_submitted": true, "is_usage_reported": test.reported, "model": "configured-model", "parent_operation_id": "parent_1", "request_id": "req_1", "user_id": "usr_1", "gateway": "discord", "workload": "foreground", "malformed_chunk_count": float64(test.malformed)} {
				if r[key] != want {
					t.Errorf("%s=%v want %v", key, r[key], want)
				}
			}
			if r["operation_id"] == "" || r["operation_id"] == "parent_1" {
				t.Errorf("operation id=%v", r["operation_id"])
			}
			_, first := r["time_to_first_output_ms"]
			_, tokens := r["total_tokens"]
			if first != test.first || tokens != test.reported {
				t.Errorf("presence: %+v", r)
			}
			if test.reported && r["total_tokens"] != float64(test.tokens) {
				t.Errorf("tokens: %+v", r)
			}
			warnings := measurementRecords(t, output.String(), "provider.gateway.chat.stream.parse_failed")
			if len(warnings) != min(1, test.malformed) {
				t.Errorf("warnings=%d", len(warnings))
			}
			if strings.Contains(output.String(), "private prose") || strings.Contains(output.String(), canary) {
				t.Fatalf("private data logged: %s", output.String())
			}
			snapshot := collector.Snapshot()
			if test.tokens == 5 && (snapshot.PromptTokens != 3 || snapshot.CompletionTokens != 2) {
				t.Errorf("lost partial usage: %+v", snapshot)
			}
			if snapshot.ModelCallCount != 1 || snapshot.ModelSubmissionCount != 1 || snapshot.TotalTokens != test.tokens || snapshot.UsageReportedCallCount != map[bool]int{true: 1}[test.reported] {
				t.Errorf("meter=%+v", snapshot)
			}
		})
	}
}

func TestGatewayLocalFailureAndCancellationMeasurements(t *testing.T) {
	for _, kind := range []string{"marshal", "url", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var output bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&output)
			client := NewGatewayClient(":invalid private prose", "", "", log)
			ctx := context.Background()
			req := ChatRequest{Model: "configured-model", Stream: true}
			if kind == "marshal" {
				invalid := math.NaN()
				req.Temperature = &invalid
			}
			if kind == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				client.BaseURL = "http://unused.invalid"
			}
			_, err := client.Chat(ctx, req, nil)
			if err == nil {
				t.Fatal("expected failure")
			}
			records := measurementRecords(t, output.String(), "provider.gateway.chat.complete")
			if len(records) != 1 {
				t.Fatalf("logs=%s", output.String())
			}
			r := records[0]
			if r["is_submitted"] != false || r["workload"] != "system" {
				t.Errorf("measurement=%+v", r)
			}
			phase := "submit"
			if kind == "marshal" {
				phase = "marshal"
			}
			if r["phase"] != phase || r["is_usage_complete"] != false {
				t.Errorf("measurement=%+v", r)
			}
			if kind == "cancel" && (r["status"] != "ok" || r["outcome"] != "canceled" || !errors.Is(err, context.Canceled)) {
				t.Errorf("cancel=%+v err=%v", r, err)
			}
			if strings.Contains(output.String(), "private prose") || strings.Contains(output.String(), `"level":"warn"`) {
				t.Fatalf("logs=%s", output.String())
			}
		})
	}
}

func TestAsyncMeasurementsSeparateEmbeddingAndDoNotCountPolls(t *testing.T) {
	for _, embedding := range []bool{false, true} {
		for _, usage := range []string{"", `,"usage":{"prompt_tokens":0,"total_tokens":0}`} {
			t.Run(fmt.Sprintf("embedding_%t_usage_%t", embedding, usage != ""), func(t *testing.T) {
				var posts, polls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						posts.Add(1)
						w.WriteHeader(http.StatusAccepted)
						_, _ = io.WriteString(w, `{"id":"job","status":"pending"}`)
						return
					}
					if polls.Add(1) == 1 {
						w.WriteHeader(http.StatusAccepted)
						_, _ = io.WriteString(w, `{"id":"job","status":"processing"}`)
						return
					}
					result := `"choices":[{"message":{"content":"private prose"}}]`
					if embedding {
						result = `"data":[{"embedding":[1,2]}]`
					}
					_, _ = fmt.Fprintf(w, `{"id":"job","status":"completed","status_code":200,"result":{%s%s}}`, result, usage)
				}))
				defer server.Close()
				var output bytes.Buffer
				log := config.NewLogger(config.LevelDebug)
				log.SetOutput(&output)
				collector := requestctx.NewUsageCollector()
				ctx := requestctx.WithUsageCollector(context.Background(), collector)
				client := newTestGatewayClient(server.URL, "", "", log)
				event, operation := "provider.gateway.chat.complete", "chat"
				if embedding {
					event, operation = "provider.gateway.embed.complete", "embedding"
					if _, err := client.Embed(ctx, EmbedRequest{Model: "configured-model", Input: "private prose"}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := client.Chat(ctx, ChatRequest{Model: "configured-model"}, nil); err != nil {
					t.Fatal(err)
				}
				records := measurementRecords(t, output.String(), event)
				if len(records) != 1 || records[0]["operation"] != operation || records[0]["transport"] != "async" || records[0]["is_usage_reported"] != (usage != "") {
					t.Fatalf("logs=%s", output.String())
				}
				if records[0]["is_usage_complete"] != (embedding && usage != "") || records[0]["phase"] != "decode" {
					t.Errorf("measurement=%+v", records[0])
				}
				s := collector.Snapshot()
				if embedding && (s.EmbeddingCallCount != 1 || s.EmbeddingSubmissionCount != 1 || s.ModelCallCount != 0) {
					t.Errorf("meter=%+v", s)
				}
				if !embedding && (s.ModelCallCount != 1 || s.ModelSubmissionCount != 1 || s.EmbeddingCallCount != 0) {
					t.Errorf("meter=%+v", s)
				}
				server.Close()
				if posts.Load() != 1 || polls.Load() != 2 {
					t.Errorf("posts=%d polls=%d", posts.Load(), polls.Load())
				}
			})
		}
	}
}

func TestInvalidUsageIsOmittedWithoutChangingChatResponse(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming_%t", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				result := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":-2,"total_tokens":-1}}`
				if streaming {
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n", result)
					return
				}
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{"id":"job","status":"pending"}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"id":"job","status":"completed","status_code":200,"result":%s}`, result)
			}))
			defer server.Close()
			var output bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&output)
			collector := requestctx.NewUsageCollector()
			client := newTestGatewayClient(server.URL, "", "", log)
			response, err := client.Chat(requestctx.WithUsageCollector(context.Background(), collector), ChatRequest{Model: "model", Stream: streaming}, nil)
			if err != nil || response.CompletionTokens != -2 || response.TotalTokens != -1 {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			records := measurementRecords(t, output.String(), "provider.gateway.chat.complete")
			if len(records) != 1 {
				t.Fatalf("records=%v", records)
			}
			r := records[0]
			if r["is_usage_complete"] != false || r["is_usage_invalid"] != true || r["is_usage_reported"] != true || r["status"] != "degraded" || r["prompt_tokens"] != float64(5) {
				t.Errorf("measurement=%+v", r)
			}
			for _, key := range []string{"completion_tokens", "total_tokens", "effective_output_tps"} {
				if _, exists := r[key]; exists {
					t.Errorf("invalid metric %s=%v", key, r[key])
				}
			}
			s := collector.Snapshot()
			if s.PromptTokens != 5 || s.CompletionTokens != 0 || s.TotalTokens != 0 {
				t.Errorf("collector=%+v", s)
			}
		})
	}
}

type failingStreamReader struct{ cancel context.CancelFunc }

func TestLaterInvalidStreamUsagePreservesKnownCounts(t *testing.T) {
	for _, ending := range []string{"success", "error", "canceled"} {
		t.Run(ending, func(t *testing.T) {
			var output bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&output)
			collector := requestctx.NewUsageCollector()
			ctx, cancel := context.WithCancel(requestctx.WithUsageCollector(context.Background(), collector))
			defer cancel()
			var tail io.Reader = strings.NewReader("data: [DONE]\n")
			status, outcome := "degraded", "degraded"
			switch ending {
			case "error":
				tail = strings.NewReader("data: {\"error\":{\"message\":\"private provider prose\"}}\n")
				status, outcome = "error", "error"
			case "canceled":
				tail = failingStreamReader{cancel: cancel}
				status, outcome = "ok", "canceled"
			}
			client := NewGatewayClient("https://unused.invalid", "", "", log)
			client.HTTPClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				body := io.MultiReader(strings.NewReader("data: {\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":20,\"total_tokens\":100}}\n\ndata: {\"usage\":{\"prompt_tokens\":81,\"completion_tokens\":-1,\"total_tokens\":-1}}\n\n"), tail)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(body)}, nil
			})
			response, err := client.Chat(ctx, ChatRequest{Model: "configured-model", Stream: true}, nil)
			if ending == "success" {
				if err != nil || response == nil || response.PromptTokens != 81 || response.CompletionTokens != -1 || response.TotalTokens != -1 {
					t.Fatalf("public response changed: response=%+v err=%v", response, err)
				}
			} else if err == nil || response != nil {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			if ending == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation changed: %v", err)
			}
			records := measurementRecords(t, output.String(), "provider.gateway.chat.complete")
			if len(records) != 1 {
				t.Fatalf("completion count=%d logs=%s", len(records), output.String())
			}
			r := records[0]
			for key, want := range map[string]any{"level": "info", "status": status, "outcome": outcome, "is_usage_reported": true, "is_usage_complete": false, "is_usage_invalid": true, "prompt_tokens": float64(81), "completion_tokens": float64(20), "total_tokens": float64(100)} {
				if r[key] != want {
					t.Errorf("%s=%v want %v", key, r[key], want)
				}
			}
			if _, exists := r["effective_output_tps"]; exists {
				t.Errorf("invalid usage produced TPS: %+v", r)
			}
			s := collector.Snapshot()
			if s.ModelCallCount != 1 || s.ModelSubmissionCount != 1 || s.UsageReportedCallCount != 1 || s.UsageCompleteCallCount != 0 || s.PromptTokens != 81 || s.CompletionTokens != 20 || s.TotalTokens != 100 {
				t.Errorf("collector lost known partial usage: %+v", s)
			}
			if strings.Contains(output.String(), "private") {
				t.Fatalf("private data logged: %s", output.String())
			}
		})
	}
}

func (r failingStreamReader) Read([]byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return 0, errors.New("private stream failure prose")
}

func TestStreamReadFailureAndCancellationRetainObservedUsage(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled_%t", canceled), func(t *testing.T) {
			var output bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&output)
			collector := requestctx.NewUsageCollector()
			ctx, cancel := context.WithCancel(requestctx.WithUsageCollector(context.Background(), collector))
			defer cancel()
			reader := failingStreamReader{}
			if canceled {
				reader.cancel = cancel
			}
			client := NewGatewayClient("https://unused.invalid", "", "", log)
			client.HTTPClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				body := io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private thinking prose\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n"), reader)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(body)}, nil
			})
			chunks := 0
			response, err := client.Chat(ctx, ChatRequest{Model: "configured-model", Stream: true}, func(ChatMessage) { chunks++ })
			if err == nil || response != nil || chunks != 1 {
				t.Fatalf("response=%+v err=%v chunks=%d", response, err, chunks)
			}
			records := measurementRecords(t, output.String(), "provider.gateway.chat.complete")
			if len(records) != 1 || records[0]["total_tokens"] != float64(6) || records[0]["is_usage_reported"] != true || records[0]["time_to_first_output_ms"] == nil {
				t.Fatalf("logs=%s", output.String())
			}
			if canceled && (records[0]["status"] != "ok" || records[0]["outcome"] != "canceled") {
				t.Errorf("cancel=%+v", records[0])
			}
			s := collector.Snapshot()
			if s.ModelCallCount != 1 || s.ModelSubmissionCount != 1 || s.TotalTokens != 6 {
				t.Errorf("meter=%+v", s)
			}
			if strings.Contains(output.String(), "private") || strings.Contains(output.String(), `"level":"warn"`) || strings.Contains(output.String(), `"level":"error"`) {
				t.Fatalf("logs=%s", output.String())
			}
		})
	}
}
