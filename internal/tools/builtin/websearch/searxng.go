package websearch

import (
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
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const (
	httpTimeout       = 8 * time.Second
	defaultRetryDelay = 100 * time.Millisecond
	maxRetryDelay     = time.Second
)

type searxngResponse struct {
	Results             []searxngResult `json:"results"`
	UnresponsiveEngines [][]interface{} `json:"unresponsive_engines"`
}

type searxngResult struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Content       string   `json:"content"`
	Score         float64  `json:"score"`
	Engine        string   `json:"engine"`
	Engines       []string `json:"engines"`
	Positions     []int    `json:"positions"`
	Category      string   `json:"category"`
	PublishedDate string   `json:"publishedDate"`
}

// SearxngClient implements Searcher against a SearXNG instance.
type SearxngClient struct {
	searchURL  url.URL
	httpClient *http.Client
	log        *config.Logger
}

// NewSearxngClient creates a SearXNG web search client targeting an absolute HTTP(S)
// base URL. A path prefix is preserved when constructing the search endpoint.
func NewSearxngClient(baseURL string, log *config.Logger) (*SearxngClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
	}
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid SearXNG base URL: absolute HTTP(S) URL with host required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid SearXNG base URL: userinfo, query, and fragment are not allowed")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/search"
	parsed.RawPath = ""
	searchOrigin := parsed.Scheme + "://" + parsed.Host
	return &SearxngClient{
		searchURL: *parsed,
		httpClient: &http.Client{
			Timeout: httpTimeout,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				if !strings.EqualFold(req.URL.Scheme+"://"+req.URL.Host, searchOrigin) {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
		log: log,
	}, nil
}

// Search queries SearXNG and returns only validated public web results.
func (c *SearxngClient) Search(ctx context.Context, query string) (SearchResponse, error) {
	startedAt := time.Now()
	if err := validateQuery(query); err != nil {
		return SearchResponse{}, err
	}
	query = strings.TrimSpace(query)

	requestURL := c.searchURL
	params := requestURL.Query()
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("language", "en-US")
	params.Set("categories", "general")
	params.Set("pageno", "1")
	requestURL.RawQuery = params.Encode()

	var resp *http.Response
	for attempt := 1; attempt <= 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
		if err != nil {
			return SearchResponse{}, errors.New("failed to build SearXNG request")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "oswald-ai/web.search")
		resp, err = c.httpClient.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SearchResponse{}, fmt.Errorf("SearXNG request canceled: %w", ctxErr)
			}
			if attempt == 2 {
				c.logRequestFailure(ctx, 0, attempt, time.Since(startedAt))
				return SearchResponse{}, errors.New("SearXNG transport request failed")
			}
			c.logRetry(ctx, 0, attempt)
			if err := waitForRetry(ctx, defaultRetryDelay); err != nil {
				return SearchResponse{}, fmt.Errorf("SearXNG request canceled: %w", err)
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			break
		}
		status := resp.StatusCode
		delay := retryDelay(resp.Header.Get("Retry-After"), time.Now())
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if attempt == 2 || !retryableStatus(status) {
			c.logRequestFailure(ctx, status, attempt, time.Since(startedAt))
			return SearchResponse{}, fmt.Errorf("SearXNG returned status %d", status)
		}
		c.logRetry(ctx, status, attempt)
		if err := waitForRetry(ctx, delay); err != nil {
			return SearchResponse{}, fmt.Errorf("SearXNG request canceled: %w", err)
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return SearchResponse{}, errors.New("failed to read SearXNG response")
	}
	if len(body) > maxResponseBytes {
		return SearchResponse{}, errors.New("SearXNG response exceeded size limit")
	}
	var backend searxngResponse
	if err := json.Unmarshal(body, &backend); err != nil {
		return SearchResponse{}, errors.New("failed to parse SearXNG response")
	}

	candidates := make([]searchCandidate, 0, len(backend.Results))
	for _, result := range backend.Results {
		candidates = append(candidates, searchCandidate{
			Title: result.Title, URL: result.URL, Content: result.Content, Score: result.Score,
			Engine: result.Engine, Engines: result.Engines, Positions: result.Positions,
			Category: result.Category, PublishedDate: result.PublishedDate,
		})
	}
	response := normalizeCandidates(candidates, unresponsiveEngineNames(backend.UnresponsiveEngines))
	c.logCompletion(ctx, query, len(body), response, time.Since(startedAt))
	return response, nil
}

func unresponsiveEngineNames(values [][]interface{}) []string {
	names := make([]string, 0, min(len(values), maxUnresponsiveEngines))
	for _, entry := range values {
		if len(entry) == 0 {
			continue
		}
		name, ok := entry[0].(string)
		if !ok {
			continue
		}
		names = mergeNames(names, []string{name}, maxUnresponsiveEngines)
	}
	return names
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(maxRetryDelay/time.Second) {
			return maxRetryDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return min(max(when.Sub(now), time.Duration(0)), maxRetryDelay)
	}
	return defaultRetryDelay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *SearxngClient) logRetry(ctx context.Context, status, attempt int) {
	if c.log == nil {
		return
	}
	fields := requestLogFields(ctx, config.F("provider", "searxng"), config.F("attempt", attempt), config.F("status", "retry"))
	if status != 0 {
		fields = append(fields, config.F("http_status", status))
	}
	c.log.Warn("tool.web.search.retry", "retrying web search request", fields...)
}

func (c *SearxngClient) logRequestFailure(ctx context.Context, status, attempt int, duration time.Duration) {
	if c.log == nil {
		return
	}
	fields := requestLogFields(ctx,
		config.F("provider", "searxng"),
		config.F("attempt_count", attempt),
		config.F("duration_ms", duration.Milliseconds()),
		config.F("status", "error"),
	)
	if status != 0 {
		fields = append(fields, config.F("http_status", status))
	}
	c.log.Warn("tool.web.search.request_failed", "web search request failed", fields...)
}

func (c *SearxngClient) logCompletion(ctx context.Context, query string, responseBytes int, response SearchResponse, duration time.Duration) {
	if c.log == nil {
		return
	}
	status := "ok"
	if response.Degraded {
		status = "degraded"
	}
	fields := requestLogFields(ctx,
		config.F("provider", "searxng"),
		config.F("query_chars", utf8.RuneCountInString(query)),
		config.F("response_bytes", responseBytes),
		config.F("candidate_count", response.Stats.CandidateCount),
		config.F("inspected_count", response.Stats.InspectedCount),
		config.F("filtered_count", response.Stats.FilteredCount),
		config.F("duplicate_count", response.Stats.DuplicateCount),
		config.F("result_count", len(response.Results)),
		config.F("unresponsive_engine_count", len(response.UnresponsiveEngines)),
		config.F("duration_ms", duration.Milliseconds()),
		config.F("is_degraded", response.Degraded),
		config.F("status", status),
	)
	if response.Degraded {
		c.log.Warn("tool.web.search.results_degraded", "web search returned partial results", fields...)
		return
	}
	c.log.Debug("tool.web.search.results_returned", "web search returned results", fields...)
}

func requestLogFields(ctx context.Context, fields ...config.Field) []config.Field {
	meta := requestctx.MetadataFromContext(ctx)
	return append([]config.Field{config.F("request_id", meta.RequestID)}, fields...)
}
