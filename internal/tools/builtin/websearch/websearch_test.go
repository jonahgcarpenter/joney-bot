package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

type fakeSearcher struct {
	response SearchResponse
	err      error
	query    string
}

func (f *fakeSearcher) Search(_ context.Context, query string) (SearchResponse, error) {
	f.query = query
	return f.response, f.err
}

func newTestClient(t *testing.T, baseURL string) *SearxngClient {
	t.Helper()
	client, err := NewSearxngClient(baseURL, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("NewSearxngClient(%q): %v", baseURL, err)
	}
	return client
}

func TestNewClientValidatesBaseURL(t *testing.T) {
	t.Parallel()
	invalid := []string{
		"", "localhost:8080", "ftp://example.com", "https:///missing-host",
		"https://user@example.com", "https://example.com?q=1", "https://example.com/#fragment",
	}
	for _, value := range invalid {
		if _, err := NewSearxngClient(value, nil); err == nil {
			t.Errorf("NewSearxngClient(%q) returned nil error", value)
		}
	}
	if _, err := NewSearxngClient("https://example.com/searx/", nil); err != nil {
		t.Fatalf("valid path-prefix URL rejected: %v", err)
	}
	if _, err := NewSearxngClient("HTTPS://example.com", nil); err != nil {
		t.Fatalf("case-insensitive HTTP scheme rejected: %v", err)
	}
}

func TestClientBuildsPrefixedSearchRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/search" {
			t.Errorf("path = %q, want /prefix/search", r.URL.Path)
		}
		query := r.URL.Query()
		for key, want := range map[string]string{
			"q": "go test", "format": "json", "language": "en-US", "categories": "general", "pageno": "1",
		} {
			if got := query.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		if query.Has("engines") || query.Has("safesearch") {
			t.Errorf("deployment-owned parameters were sent: %v", query)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "oswald-ai/web.search" {
			t.Errorf("unexpected request headers: %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/prefix/")
	if _, err := client.Search(context.Background(), "go test"); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
}

func TestClientValidatesQuery(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, "https://example.com")
	for _, query := range []string{"", "line\nbreak", "\ntrimmed control", strings.Repeat("x", maxQueryRunes+1)} {
		if _, err := client.Search(context.Background(), query); err == nil {
			t.Errorf("Search(%q) returned nil error", query)
		}
	}
	if err := validateQuery(strings.Repeat("界", maxQueryRunes)); err != nil {
		t.Fatalf("400-character query rejected: %v", err)
	}
	if err := validateQuery(strings.Repeat("word ", maxQueryWords+1)); err == nil {
		t.Fatal("51-word query was accepted")
	}
}

func TestClientFiltersCanonicalizesDeduplicatesAndCapsHosts(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"unresponsive_engines":[["slow-engine","timeout"],["backup","reason"]],
			"results":[
				{"title":" First &amp; best\n ","url":"HTTPS://Example.COM:443/a?utm_source=x&keep=1#part","content":"hello\u0000 world","engine":"alpha","engines":["beta"],"score":2.5,"positions":[1,2],"category":"general","publishedDate":"2026-08-01"},
				{"title":"Duplicate","url":"https://example.com/a?keep=1&utm_medium=y","content":"duplicate","engine":"gamma"},
				{"title":"Second","url":"https://example.com/b","content":"second"},
				{"title":"Third host result","url":"https://example.com/c","content":"third"},
				{"title":"Private","url":"http://127.0.0.1/secret","content":"bad"},
				{"title":"Userinfo","url":"https://user@public.example/","content":"bad"},
				{"title":" ","url":"https://other.example/","content":"bad"},
				{"title":"Other","url":"https://other.example/path","content":"ok","engine":"delta"}
			]
		}`))
	}))
	defer server.Close()

	response, err := newTestClient(t, server.URL).Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if !response.Degraded || strings.Join(response.UnresponsiveEngines, ",") != "slow-engine,backup" {
		t.Fatalf("degradation = %+v", response)
	}
	if len(response.Results) != 3 {
		t.Fatalf("result count = %d, want 3: %+v", len(response.Results), response.Results)
	}
	first := response.Results[0]
	if first.Title != "First & best" || first.Snippet != "hello world" {
		t.Fatalf("normalized first result = %+v", first)
	}
	if first.URL != "https://example.com/a?keep=1" || first.Domain != "example.com" {
		t.Fatalf("canonical first URL = %q domain=%q", first.URL, first.Domain)
	}
	if strings.Join(first.Engines, ",") != "alpha,beta,gamma" {
		t.Fatalf("merged engines = %v", first.Engines)
	}
	if first.Score != 2.5 || first.Category != "general" || len(first.Positions) != 2 || first.PublishedAt != "2026-08-01" {
		t.Fatalf("backend metadata not parsed: %+v", first)
	}
	if response.Stats.CandidateCount != 8 || response.Stats.DuplicateCount != 1 || response.Stats.FilteredCount != 4 {
		t.Fatalf("candidate stats = %+v", response.Stats)
	}
}

func TestClientInspectsOnlyFirstFiftyAndSelectsEight(t *testing.T) {
	t.Parallel()
	var results strings.Builder
	results.WriteString(`{"results":[`)
	for i := 0; i < 55; i++ {
		if i > 0 {
			results.WriteByte(',')
		}
		fmt.Fprintf(&results, `{"title":"%d","url":"https://host%d.example/%d"}`, i, i, i)
	}
	results.WriteString(`]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(results.String())) }))
	defer server.Close()

	response, err := newTestClient(t, server.URL).Search(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if response.Stats.CandidateCount != 55 || response.Stats.InspectedCount != 50 || len(response.Results) != 8 {
		t.Fatalf("response stats/results = %+v / %d", response.Stats, len(response.Results))
	}
	if response.Results[7].Title != "7" {
		t.Fatalf("source order not preserved: %+v", response.Results)
	}
}

func TestClientRetriesOnlyRetryableResponses(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "secret backend body", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	if _, err := newTestClient(t, server.URL).Search(context.Background(), "private query"); err != nil {
		t.Fatalf("retryable Search returned error: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempt count = %d, want 2", attempts.Load())
	}

	attempts.Store(0)
	badRequest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "secret backend body", http.StatusBadRequest)
	}))
	defer badRequest.Close()
	_, err := newTestClient(t, badRequest.URL).Search(context.Background(), "private query")
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("non-retryable response: attempts=%d err=%v", attempts.Load(), err)
	}
	if strings.Contains(err.Error(), "private query") || strings.Contains(err.Error(), "secret backend body") || strings.Contains(err.Error(), "?") {
		t.Fatalf("error leaked request/backend data: %q", err)
	}
}

func TestClientRejectsCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationRequests.Add(1)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer source.Close()

	_, err := newTestClient(t, source.URL).Search(context.Background(), "private query")
	if err == nil || destinationRequests.Load() != 0 {
		t.Fatalf("cross-origin redirect: requests=%d err=%v", destinationRequests.Load(), err)
	}
}

func TestClientHonorsCancellationDuringBackoff(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := newTestClient(t, server.URL).Search(ctx, "test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Search error = %v, want deadline exceeded", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("context cancellation did not interrupt backoff")
	}
}

func TestClientRejectsOversizedAndInvalidResponses(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "large" {
			_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
			return
		}
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	if _, err := client.Search(context.Background(), "large"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized response error = %v", err)
	}
	if _, err := client.Search(context.Background(), "invalid"); err == nil || !strings.Contains(err.Error(), "parse SearXNG") {
		t.Fatalf("invalid JSON error = %v", err)
	}
}

func TestHandlerProducesBoundedDecodableJSON(t *testing.T) {
	t.Parallel()
	results := make([]SearchResult, 8)
	for i := range results {
		results[i] = SearchResult{
			Title: strings.Repeat("t", maxTitleRunes), URL: "https://example.com/" + strings.Repeat("u", 1900),
			Domain: "example.com", Snippet: strings.Repeat("s", maxSnippetRunes), Engines: []string{"engine"},
		}
	}
	searcher := &fakeSearcher{response: SearchResponse{Results: results}}
	result, err := NewHandler(searcher, config.NewLogger(config.LevelError))(context.Background(), map[string]interface{}{"query": " test "})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) > maxToolResponseBytes {
		t.Fatalf("tool response size = %d", len(result.Content))
	}
	if !result.IsDegraded || result.ReasonCode != "partial_results" {
		t.Fatalf("size-truncated output classification = %+v", result)
	}
	decoded, err := DecodeToolResponse(result.Content)
	if err != nil {
		t.Fatalf("DecodeToolResponse: %v", err)
	}
	if decoded.Notice != toolNotice || decoded.Results == nil || decoded.UnresponsiveEngines == nil || searcher.query != "test" {
		t.Fatalf("decoded response/query = %+v / %q", decoded, searcher.query)
	}
	if _, err := DecodeToolResponse(result.Content + "{}"); err == nil {
		t.Fatal("DecodeToolResponse accepted trailing JSON")
	}
}

func TestHandlerClassifiesOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response SearchResponse
		outcome  governance.Outcome
		reason   string
		degraded bool
	}{
		{name: "results", response: SearchResponse{Results: []SearchResult{{Title: "a", URL: "https://example.com/", Domain: "example.com"}}}, outcome: governance.OutcomeProductive},
		{name: "partial results", response: SearchResponse{Degraded: true, UnresponsiveEngines: []string{"slow"}, Results: []SearchResult{{Title: "a", URL: "https://example.com/", Domain: "example.com"}}}, outcome: governance.OutcomeProductive, reason: "partial_results", degraded: true},
		{name: "clean empty", response: SearchResponse{}, outcome: governance.OutcomeUnproductive, reason: "no_results"},
		{name: "partial empty", response: SearchResponse{Degraded: true, UnresponsiveEngines: []string{"slow"}}, outcome: governance.OutcomeUnproductive, reason: "partial_no_results", degraded: true},
		{name: "all invalid", response: SearchResponse{Stats: CandidateStats{CandidateCount: 3, FilteredCount: 3}}, outcome: governance.OutcomeUnproductive, reason: "invalid_results", degraded: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			searcher := &fakeSearcher{response: test.response}
			result, err := NewHandler(searcher, config.NewLogger(config.LevelError))(context.Background(), map[string]interface{}{"query": "test"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != test.outcome || result.ReasonCode != test.reason || result.IsDegraded != test.degraded {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestNormalizationBoundsTextAndEngineNames(t *testing.T) {
	t.Parallel()
	engines := make([]string, 12)
	for i := range engines {
		engines[i] = fmt.Sprintf("engine-%d-%s", i, strings.Repeat("x", 100))
	}
	result, ok := normalizeResult(searchCandidate{
		Title: strings.Repeat("界", 300), URL: "https://public.example/", Content: strings.Repeat("界", maxSnippetRunes+100), Engines: engines,
	})
	if !ok {
		t.Fatal("valid result rejected")
	}
	if utf8.RuneCountInString(result.Title) != maxTitleRunes || utf8.RuneCountInString(result.Snippet) != maxSnippetRunes || len(result.Engines) != maxEngineNames {
		t.Fatalf("bounds not applied: title=%d snippet=%d engines=%d", utf8.RuneCountInString(result.Title), utf8.RuneCountInString(result.Snippet), len(result.Engines))
	}
	for _, engine := range result.Engines {
		if utf8.RuneCountInString(engine) > maxEngineNameRunes {
			t.Fatalf("engine name not bounded: %q", engine)
		}
	}
}

func TestNormalizeResultRejectsMalformedQueryAndAcceptsUppercaseScheme(t *testing.T) {
	t.Parallel()
	if _, ok := normalizeResult(searchCandidate{Title: "bad", URL: "https://example.com/?a=1;b=2"}); ok {
		t.Fatal("result with malformed query was accepted")
	}
	result, ok := normalizeResult(searchCandidate{Title: "good", URL: "HTTPS://Example.com/path"})
	if !ok || result.URL != "https://example.com/path" {
		t.Fatalf("uppercase scheme result = %+v, accepted=%t", result, ok)
	}
}

func TestRetryDelayCapsLargeValues(t *testing.T) {
	t.Parallel()
	if got := retryDelay("9223372036854775807", time.Now()); got != maxRetryDelay {
		t.Fatalf("retry delay = %v, want %v", got, maxRetryDelay)
	}
}
