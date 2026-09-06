package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestAsyncPollFailureBudgetAndValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		statuses  []int
		body      string
		transport bool
		wantError string
	}{
		{name: "three HTTP failures", statuses: []int{503, 503, 503}, wantError: "HTTP 503"},
		{name: "three transport failures", statuses: []int{503, 503, 503}, transport: true, wantError: "poll LLM gateway async job"},
		{name: "successful poll resets HTTP budget", statuses: []int{503, 503, 202, 503, 503, 200}},
		{name: "successful poll resets transport budget", statuses: []int{503, 503, 202, 503, 503, 200}, transport: true},
		{name: "mismatched ID", statuses: []int{200}, body: `{"id":"other","status":"completed","result":{"choices":[{"message":{"content":"wrong job"}}]}}`, wantError: "mismatched job ID"},
		{name: "null completed result", statuses: []int{200}, body: `{"id":"job","status":"completed","status_code":200,"result":null}`, wantError: "completed without a result"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var submissions, polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					submissions.Add(1)
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"id":"job","status":"pending"}`))
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/v1/async/chat/completions/job" {
					t.Errorf("unexpected poll: %s %s", r.Method, r.URL.Path)
				}
				index := int(polls.Load()) - 1
				if index >= len(test.statuses) {
					http.Error(w, "unexpected extra poll", http.StatusBadRequest)
					return
				}
				status := test.statuses[index]
				w.WriteHeader(status)
				if status == http.StatusAccepted {
					_, _ = w.Write([]byte(`{"id":"job","status":"processing"}`))
				} else if test.body != "" {
					_, _ = w.Write([]byte(test.body))
				} else {
					_, _ = w.Write([]byte(`{"id":"job","status":"completed","status_code":200,"result":{"choices":[{"message":{"content":"complete"}}]}}`))
				}
			}))
			defer server.Close()
			client := newTestGatewayClient(server.URL, "", "", config.NewLogger(config.LevelError))
			base := server.Client().Transport
			client.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					index := int(polls.Add(1)) - 1
					if test.transport && index < len(test.statuses) && test.statuses[index] == 503 {
						return nil, errors.New("synthetic poll transport failure")
					}
				}
				return base.RoundTrip(r)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			response, err := client.Chat(ctx, ChatRequest{Model: "test"}, nil)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || response != nil {
					t.Fatalf("response=%+v err=%v, want nil response and %q", response, err, test.wantError)
				}
			} else if err != nil || response == nil || response.Message.Content != "complete" {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			if submissions.Load() != 1 || polls.Load() != int32(len(test.statuses)) {
				t.Fatalf("submissions=%d polls=%d, want 1 and %d", submissions.Load(), polls.Load(), len(test.statuses))
			}
		})
	}
}
