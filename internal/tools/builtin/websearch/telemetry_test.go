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
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestSearchTelemetryAtDebugDoesNotReflectProviderJSONOrQueries(t *testing.T) {
	const canary = "private_provider_prose"
	for _, provider := range []string{"brave", "searxng"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if provider == "brave" {
					_, _ = io.WriteString(w, `{"grounding":{"generic":[{"title":"`+canary+`","url":"https://example.com/`+canary+`","snippets":["`+canary+`"]}]},"unknown":"`+canary+`"}`)
				} else {
					_, _ = io.WriteString(w, `{"results":[{"title":"`+canary+`","url":"https://example.com/`+canary+`","content":"`+canary+`"}],"unresponsive_engines":[["`+canary+`","`+canary+`"]],"unknown":"`+canary+`"}`)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&output)
			var searcher Searcher
			var err error
			if provider == "brave" {
				searcher, err = newBraveClient(server.URL, canary, server.Client(), nil, log)
			} else {
				searcher, err = NewSearxngClient(server.URL, log)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req_search", OperationID: "op_parent"})
			if _, err := searcher.Search(ctx, canary); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), canary) {
				t.Fatalf("private data: %s", output.String())
			}
			attempts, completions := 0, 0
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var r map[string]any
				if err := json.Unmarshal([]byte(line), &r); err != nil {
					t.Fatal(err)
				}
				if r["event"] == "provider.web.search.attempt.complete" {
					attempts++
					if r["http_status"] != float64(200) {
						t.Errorf("attempt=%+v", r)
					}
				}
				if r["event"] == "provider.web.search.complete" {
					completions++
					if r["level"] != "info" || r["request_id"] != "req_search" || r["operation_id"] == "op_parent" || r["parent_operation_id"] != "op_parent" {
						t.Errorf("completion=%+v", r)
					}
					if provider == "searxng" && r["status"] != "degraded" {
						t.Errorf("completion=%+v", r)
					}
				}
			}
			if attempts != 1 || completions != 1 {
				t.Fatalf("logs=%s", output.String())
			}
		})
	}
}

func TestFallbackCompletionReportsFinalDegradation(t *testing.T) {
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	primary := &countingSearcher{err: errors.New("private fallback error prose")}
	fallback := &countingSearcher{response: SearchResponse{Results: []SearchResult{{Title: "private title prose"}}}}
	response, err := NewFallbackSearcher(primary, fallback, log).Search(context.Background(), "private query prose")
	if err != nil || !response.Degraded {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["event"] == "provider.web.search.fallback.complete" {
			found = true
			if r["level"] != "info" || r["status"] != "degraded" || r["result_count"] != float64(1) {
				t.Errorf("record=%+v", r)
			}
		}
	}
	if !found || strings.Contains(output.String(), "private") {
		t.Fatalf("logs=%s", output.String())
	}
}
