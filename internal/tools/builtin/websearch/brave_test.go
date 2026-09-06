package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func testBraveClient(t *testing.T, server *httptest.Server, key string) *BraveClient {
	t.Helper()
	client, err := newBraveClient(server.URL, key, server.Client(), noopLimiter{}, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestBraveClientBuildsPinnedContextRequest(t *testing.T) {
	var request braveContextRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("X-Subscription-Token") != "secret-key" || r.Header.Get("Api-Version") != braveAPIVersion {
			t.Errorf("authentication/version headers = %v", r.Header)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "oswald-ai/web.search" {
			t.Errorf("request headers = %v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"grounding":{"generic":[]},"sources":{}}`)
	}))
	defer server.Close()

	if _, err := testBraveClient(t, server, "secret-key").Search(context.Background(), "structured data query"); err != nil {
		t.Fatal(err)
	}
	if request.Query != "structured data query" || request.Country != "US" || request.SearchLanguage != "en" || request.Count != 20 || !request.Spellcheck || request.SafeSearch != "off" {
		t.Fatalf("request profile = %+v", request)
	}
	if request.MaximumURLs != 8 || request.MaximumTokens != 3072 || request.MaximumSnippets != 24 || request.MaximumTokensPerURL != 1024 || request.MaximumSnippetsPerURL != 6 || request.ContextThresholdMode != "balanced" || request.EnableLocal || request.EnableSourceMetadata {
		t.Fatalf("context profile = %+v", request)
	}
}

func TestBraveClientMapsAndNormalizesGrounding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"grounding":{"generic":[
				{"url":"HTTPS://Example.COM:443/page?utm_source=x&keep=1#part","title":"Primary &amp; title","snippets":["line one\n{\"value\": 2}"]},
				{"url":"http://127.0.0.1/private","title":"Private","snippets":["bad"]}
			]},
			"sources":{"HTTPS://Example.COM:443/page?utm_source=x&keep=1#part":{"age":["full","2026-08-01","relative","2026-08-01T12:30:00Z"]}}
		}`)
	}))
	defer server.Close()

	response, err := testBraveClient(t, server, "secret").Search(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Stats.CandidateCount != 2 || response.Stats.FilteredCount != 1 {
		t.Fatalf("response = %+v", response)
	}
	result := response.Results[0]
	if result.URL != "https://example.com/page?keep=1" || result.Title != "Primary & title" || result.PublishedAt != "2026-08-01T12:30:00Z" || strings.Join(result.Engines, ",") != "brave" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Snippet, "\n") || !strings.Contains(result.Snippet, `{"value": 2}`) {
		t.Fatalf("structured snippet was flattened: %q", result.Snippet)
	}
}

func TestBraveClientRetriesExplicitTransientResponsesOnly(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "private error", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"grounding":{"generic":[]}}`)
	}))
	defer server.Close()
	if _, err := testBraveClient(t, server, "secret").Search(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestBraveClientRateLimitRetriesOnlyShortWindow(t *testing.T) {
	for _, test := range []struct {
		name         string
		policy       string
		reset        string
		wantAttempts int32
	}{
		{name: "burst", policy: "50;w=1", reset: "0", wantAttempts: 2},
		{name: "quota", policy: "1000;w=2592000", reset: "100", wantAttempts: 1},
		{name: "unknown", wantAttempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempt := attempts.Add(1)
				if attempt == 1 {
					if test.policy != "" {
						w.Header().Set("X-RateLimit-Limit", "50")
						w.Header().Set("X-RateLimit-Policy", test.policy)
						w.Header().Set("X-RateLimit-Remaining", "0")
						w.Header().Set("X-RateLimit-Reset", test.reset)
					}
					http.Error(w, "limited", http.StatusTooManyRequests)
					return
				}
				_, _ = io.WriteString(w, `{"grounding":{"generic":[]}}`)
			}))
			defer server.Close()
			_, _ = testBraveClient(t, server, "secret").Search(context.Background(), "test")
			if attempts.Load() != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts.Load(), test.wantAttempts)
			}
		})
	}
}

func TestBraveClientRetriesBurstLimitWithUnlimitedQuotaWindow(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Limit", "50, 0")
			w.Header().Set("X-RateLimit-Policy", "50;w=1, 0;w=2592000")
			w.Header().Set("X-RateLimit-Remaining", "0, 0")
			w.Header().Set("X-RateLimit-Reset", "0, 100000")
			http.Error(w, "limited", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"grounding":{"generic":[]}}`)
	}))
	defer server.Close()
	if _, err := testBraveClient(t, server, "secret").Search(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

type failingRoundTripper struct{ calls atomic.Int32 }

func (r *failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, errors.New("transport secret")
}

func TestBraveClientDoesNotRetryOrLeakTransportFailure(t *testing.T) {
	transport := &failingRoundTripper{}
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	client, err := newBraveClient("https://api.search.brave.com/res/v1/llm/context", "secret-key", httpClient, noopLimiter{}, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Search(context.Background(), "private query")
	if err == nil || transport.calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", transport.calls.Load(), err)
	}
	if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "private query") || strings.Contains(err.Error(), "transport secret") {
		t.Fatalf("error leaked request data: %q", err)
	}
}

func TestBraveClientRejectsRedirectWithoutForwardingKey(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
		if r.Header.Get("X-Subscription-Token") != "" {
			t.Error("redirect destination received API key")
		}
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	httpClient := source.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client, err := newBraveClient(source.URL, "secret", httpClient, noopLimiter{}, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), "test"); err == nil || destinationCalls.Load() != 0 {
		t.Fatalf("destination calls=%d err=%v", destinationCalls.Load(), err)
	}
}

func TestBraveClientRejectsOversizedAndMalformedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request braveContextRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Query == "large" {
			_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
			return
		}
		_, _ = io.WriteString(w, "not-json")
	}))
	defer server.Close()
	client := testBraveClient(t, server, "secret")
	if _, err := client.Search(context.Background(), "large"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("large error = %v", err)
	}
	if _, err := client.Search(context.Background(), "malformed"); err == nil || !strings.Contains(err.Error(), "parse Brave") {
		t.Fatalf("malformed error = %v", err)
	}
}

func TestBraveClientRejectsStructurallyIncompleteSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	if _, err := testBraveClient(t, server, "secret").Search(context.Background(), "test"); err == nil || !strings.Contains(err.Error(), "parse Brave") {
		t.Fatalf("incomplete response error = %v", err)
	}
}

func TestBraveClientLogsNoSensitiveRequestOrResultData(t *testing.T) {
	const key = "secret-api-key"
	const query = "private search query"
	const resultMarker = "private-result-marker.example"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"grounding":{"generic":[{"url":"https://private-result-marker.example/","title":"private title","snippets":["private snippet"]}]},"sources":{}}`)
	}))
	defer server.Close()
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	client, err := newBraveClient(server.URL, key, server.Client(), noopLimiter{}, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	logs := output.String()
	if !strings.Contains(logs, `"event":"provider.web.search.complete"`) {
		t.Fatalf("missing provider completion: %s", logs)
	}
	for _, forbidden := range []string{key, query, resultMarker, "private title", "private snippet"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("logs contained sensitive value %q: %s", forbidden, logs)
		}
	}
}
