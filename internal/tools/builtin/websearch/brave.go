package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const (
	braveContextEndpoint = "https://api.search.brave.com/res/v1/llm/context"
	braveAPIVersion      = "2026-07-31"
	braveHTTPTimeout     = 30 * time.Second
	braveRateLimit       = 50
)

type braveContextResponse struct {
	Grounding *braveGrounding               `json:"grounding"`
	Sources   map[string]braveContextSource `json:"sources"`
}

type braveGrounding struct {
	Generic *[]braveContextResult `json:"generic"`
}

type braveContextResult struct {
	URL      string   `json:"url"`
	Title    string   `json:"title"`
	Snippets []string `json:"snippets"`
}

type braveContextSource struct {
	Title    string   `json:"title"`
	Hostname string   `json:"hostname"`
	Age      []string `json:"age"`
}

type braveContextRequest struct {
	Query                 string `json:"q"`
	Country               string `json:"country"`
	SearchLanguage        string `json:"search_lang"`
	Count                 int    `json:"count"`
	Spellcheck            bool   `json:"spellcheck"`
	SafeSearch            string `json:"safesearch"`
	MaximumURLs           int    `json:"maximum_number_of_urls"`
	MaximumTokens         int    `json:"maximum_number_of_tokens"`
	MaximumSnippets       int    `json:"maximum_number_of_snippets"`
	MaximumTokensPerURL   int    `json:"maximum_number_of_tokens_per_url"`
	MaximumSnippetsPerURL int    `json:"maximum_number_of_snippets_per_url"`
	ContextThresholdMode  string `json:"context_threshold_mode"`
	EnableLocal           bool   `json:"enable_local"`
	EnableSourceMetadata  bool   `json:"enable_source_metadata"`
}

// BraveClient implements Searcher with Brave's Search-plan LLM Context endpoint.
type BraveClient struct {
	endpoint   url.URL
	apiKey     string
	httpClient *http.Client
	limiter    attemptLimiter
	log        *config.Logger
}

// NewBraveClient creates a fixed-endpoint Brave LLM Context client.
func NewBraveClient(apiKey string, log *config.Logger) (*BraveClient, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("Brave API key is required")
	}
	httpClient := &http.Client{
		Timeout: braveHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return newBraveClient(braveContextEndpoint, apiKey, httpClient, newRollingLimiter(braveRateLimit, time.Second), log)
}

func newBraveClient(endpoint, apiKey string, httpClient *http.Client, limiter attemptLimiter, log *config.Logger) (*BraveClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("invalid Brave endpoint")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid Brave endpoint")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("Brave API key is required")
	}
	if httpClient == nil {
		return nil, errors.New("Brave HTTP client is required")
	}
	if limiter == nil {
		limiter = noopLimiter{}
	}
	return &BraveClient{endpoint: *parsed, apiKey: strings.TrimSpace(apiKey), httpClient: httpClient, limiter: limiter, log: log}, nil
}

// Search requests bounded, extracted web context from Brave.
func (c *BraveClient) Search(ctx context.Context, query string) (response SearchResponse, err error) {
	ctx, complete := beginSearch(ctx, c.log, "brave")
	defer func() { complete(response, err) }()
	startedAt := time.Now()
	if err := validateQuery(query); err != nil {
		return SearchResponse{}, err
	}
	query = strings.TrimSpace(query)
	payload, err := json.Marshal(braveContextRequest{
		Query: query, Country: "US", SearchLanguage: "en", Count: 20, Spellcheck: true,
		SafeSearch: "off", MaximumURLs: 8, MaximumTokens: 3072, MaximumSnippets: 24,
		MaximumTokensPerURL: 1024, MaximumSnippetsPerURL: 6, ContextThresholdMode: "balanced",
		EnableLocal: false, EnableSourceMetadata: false,
	})
	if err != nil {
		return SearchResponse{}, errors.New("failed to encode Brave request")
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return SearchResponse{}, fmt.Errorf("Brave request canceled: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
		if err != nil {
			return SearchResponse{}, errors.New("failed to build Brave request")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subscription-Token", c.apiKey)
		req.Header.Set("Api-Version", braveAPIVersion)
		req.Header.Set("User-Agent", "oswald-ai/web.search")

		resp, err := searchAttempt(c.log, c.httpClient, req, "brave", attempt)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SearchResponse{}, fmt.Errorf("Brave request canceled: %w", ctxErr)
			}
			c.logFailure(ctx, 0, attempt, "transport", time.Since(startedAt))
			return SearchResponse{}, errors.New("Brave transport request failed")
		}
		if resp.StatusCode == http.StatusOK {
			response, bodyBytes, err := decodeBraveResponse(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				c.logFailure(ctx, resp.StatusCode, attempt, "response", time.Since(startedAt))
				return SearchResponse{}, err
			}
			c.logCompletion(ctx, query, bodyBytes, response, time.Since(startedAt))
			return response, nil
		}

		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		delay, retry, class := braveRetry(status, resp.Header)
		if attempt == 2 || !retry {
			c.logFailure(ctx, status, attempt, class, time.Since(startedAt))
			return SearchResponse{}, &searchHTTPError{status: status, message: fmt.Sprintf("Brave returned status %d", status)}
		}
		c.logRetry(ctx, status, attempt, class)
		if err := waitForRetry(ctx, delay); err != nil {
			return SearchResponse{}, fmt.Errorf("Brave request canceled: %w", err)
		}
	}
	return SearchResponse{}, errors.New("Brave search failed")
}

func decodeBraveResponse(body io.Reader) (SearchResponse, int, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return SearchResponse{}, 0, errors.New("failed to read Brave response")
	}
	if len(data) > maxResponseBytes {
		return SearchResponse{}, len(data), errors.New("Brave response exceeded size limit")
	}
	var backend braveContextResponse
	if err := json.Unmarshal(data, &backend); err != nil {
		return SearchResponse{}, len(data), errors.New("failed to parse Brave response")
	}
	if backend.Grounding == nil || backend.Grounding.Generic == nil {
		return SearchResponse{}, len(data), errors.New("failed to parse Brave response")
	}
	candidates := make([]searchCandidate, 0, len(*backend.Grounding.Generic))
	for _, result := range *backend.Grounding.Generic {
		source := backend.Sources[result.URL]
		title := result.Title
		if strings.TrimSpace(title) == "" {
			title = source.Title
		}
		published := ""
		if len(source.Age) > 3 {
			published = source.Age[3]
		} else if len(source.Age) > 1 {
			published = source.Age[1]
		}
		cleanSnippets := make([]string, 0, len(result.Snippets))
		for _, snippet := range result.Snippets {
			if cleaned := cleanContextText(snippet); cleaned != "" {
				cleanSnippets = append(cleanSnippets, cleaned)
			}
		}
		candidates = append(candidates, searchCandidate{
			Title: title, URL: result.URL, Content: strings.Join(cleanSnippets, "\n\n"), Engine: "brave",
			PublishedDate: published, PreserveWhitespace: true,
		})
	}
	return normalizeCandidates(candidates, []string{}), len(data), nil
}

func braveRetry(status int, headers http.Header) (time.Duration, bool, string) {
	switch status {
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return defaultRetryDelay, true, "transient"
	case http.StatusTooManyRequests:
		delay, class, ok := braveRateLimitDelay(headers)
		return delay, ok, class
	default:
		return 0, false, "permanent"
	}
}

func braveRateLimitDelay(headers http.Header) (time.Duration, string, bool) {
	limits := splitHeaderValues(headers.Get("X-RateLimit-Limit"))
	remaining := splitHeaderValues(headers.Get("X-RateLimit-Remaining"))
	resets := splitHeaderValues(headers.Get("X-RateLimit-Reset"))
	policies := splitHeaderValues(headers.Get("X-RateLimit-Policy"))
	if len(remaining) == 0 || len(limits) != len(remaining) || len(policies) != len(remaining) || len(remaining) != len(resets) {
		return 0, "unknown", false
	}
	maxReset := time.Duration(0)
	found := false
	class := "burst"
	for i := range remaining {
		limit, limitErr := strconv.ParseInt(limits[i], 10, 64)
		window := policyWindowSeconds(policies[i])
		if limitErr != nil || limit < 0 || window <= 0 {
			return 0, "unknown", false
		}
		if limit == 0 {
			continue
		}
		value, err := strconv.ParseInt(remaining[i], 10, 64)
		if err != nil {
			return 0, "unknown", false
		}
		if value > 0 {
			continue
		}
		seconds, err := strconv.ParseFloat(resets[i], 64)
		if err != nil || seconds < 0 {
			return 0, "unknown", false
		}
		found = true
		reset := time.Duration(seconds * float64(time.Second))
		if reset > maxReset {
			maxReset = reset
		}
		if reset > maxRetryDelay || window > 1 {
			class = "quota"
		}
	}
	if !found || class == "quota" || maxReset > maxRetryDelay {
		return 0, class, false
	}
	return maxReset, class, true
}

func splitHeaderValues(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func policyWindowSeconds(policy string) int64 {
	_, raw, ok := strings.Cut(policy, ";w=")
	if !ok {
		return 0
	}
	window, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return window
}

func (c *BraveClient) logRetry(ctx context.Context, status, attempt int, class string) {
	if c.log == nil {
		return
	}
	c.log.Debug("tool.web.search.retry", "retrying web search request", requestLogFields(ctx,
		config.F("provider", "brave"), config.F("attempt", attempt), config.F("http_status", status),
		config.F("rate_limit_scope", class), config.F("status", "retry"))...)
}

func (c *BraveClient) logFailure(ctx context.Context, status, attempt int, class string, duration time.Duration) {
	if c.log == nil {
		return
	}
	fields := requestLogFields(ctx, config.F("provider", "brave"), config.F("attempt_count", attempt),
		config.F("failure_kind", class), config.F("duration_ms", duration.Milliseconds()), config.F("status", "error"))
	if status != 0 {
		fields = append(fields, config.F("http_status", status))
	}
	c.log.Debug("tool.web.search.request_failed", "web search request failed", fields...)
}

func (c *BraveClient) logCompletion(ctx context.Context, query string, responseBytes int, response SearchResponse, duration time.Duration) {
	if c.log == nil {
		return
	}
	c.log.Debug("tool.web.search.results_returned", "web search returned results", requestLogFields(ctx,
		config.F("provider", "brave"), config.F("query_chars", utf8.RuneCountInString(query)),
		config.F("response_bytes", responseBytes), config.F("candidate_count", response.Stats.CandidateCount),
		config.F("filtered_count", response.Stats.FilteredCount), config.F("duplicate_count", response.Stats.DuplicateCount),
		config.F("result_count", len(response.Results)), config.F("duration_ms", duration.Milliseconds()), config.F("status", "ok"))...)
}
