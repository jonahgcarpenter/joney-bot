package globalmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

const searchOutputLimit = 5000

// NewSearchHandler creates the sole model-visible global-memory tool.
func NewSearchHandler(store *global.Store, log *config.Logger) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if _, err := authenticatedPrincipal(ctx); err != nil {
			return governance.Result{}, err
		}
		query := global.NormalizeMemory(stringArg(args, "query"))
		if query == "" || utf8.RuneCountInString(query) > 500 {
			return governance.Result{}, fmt.Errorf("%s: query must contain 1..500 characters", toolnames.GlobalMemorySearch)
		}
		limit, validLimit := intArg(args, "limit", global.DefaultSearchLimit)
		if !validLimit || limit < 1 || limit > global.MaxSearchLimit {
			return governance.Result{}, fmt.Errorf("%s: limit must be between 1 and %d", toolnames.GlobalMemorySearch, global.MaxSearchLimit)
		}
		started := time.Now()
		results, stats := store.Search(ctx, query, limit)
		toolLog := requestLog(log, ctx)
		if stats.LexicalError != nil {
			toolLog.Warn("agent.tool.global_memory.search.lexical_degraded", "global-memory lexical search degraded", config.F("status", "degraded"), config.ErrorField(stats.LexicalError))
		}
		if stats.SemanticError != nil {
			toolLog.Warn("agent.tool.global_memory.search.semantic_degraded", "global-memory semantic search degraded", config.F("status", "degraded"), config.ErrorField(stats.SemanticError))
		}
		toolLog.Debug("agent.tool.global_memory.search.complete", "searched global memory",
			config.F("lexical_candidate_count", stats.LexicalCount), config.F("semantic_candidate_count", stats.SemanticCount),
			config.F("selected_memory_count", stats.SelectedCount), config.F("is_lexical_available", stats.LexicalAvailable),
			config.F("is_vector_available", stats.SemanticAvailable), config.F("duration_ms", time.Since(started).Milliseconds()))
		if !stats.LexicalAvailable && !stats.SemanticAvailable {
			return governance.Result{}, fmt.Errorf("%s: retrieval channels unavailable", toolnames.GlobalMemorySearch)
		}
		outcome := governance.OutcomeProductive
		reason := ""
		if len(results) == 0 {
			outcome = governance.OutcomeUnproductive
			reason = "no_results"
		}
		return governance.Result{Content: renderSearch(results, searchOutputLimit), Outcome: outcome, ReasonCode: reason, IsDegraded: stats.LexicalError != nil || stats.SemanticError != nil}, nil
	}
}

func renderSearch(results []global.SearchResult, maxRunes int) string {
	if len(results) == 0 {
		return "No relevant global memories found."
	}
	output := ""
	for _, result := range results {
		record := struct {
			ID      int64    `json:"id"`
			Memory  string   `json:"memory"`
			Score   float64  `json:"score"`
			Sources []string `json:"sources"`
		}{result.Memory.ID, result.Memory.Text, math.Round(result.Score*1000) / 1000, result.Sources}
		encoded, _ := json.Marshal(record)
		line := string(encoded)
		if output != "" {
			line = "\n" + line
		}
		if utf8.RuneCountInString(output)+utf8.RuneCountInString(line) <= maxRunes {
			output += line
		}
	}
	return output
}

func authenticatedPrincipal(ctx context.Context) (identity.Principal, error) {
	principal, _ := requestctx.PrincipalFromContext(ctx)
	if !principal.Valid() || !principal.Authenticated() {
		return identity.Principal{}, fmt.Errorf("%s: authenticated user identity is required", toolnames.GlobalMemorySearch)
	}
	return principal, nil
}

func requestLog(log *config.Logger, ctx context.Context) *config.Logger {
	meta := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	return log.Agent("agent.tool.global_memory", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
}

func stringArg(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func intArg(args map[string]interface{}, key string, fallback int) (int, bool) {
	if args == nil || args[key] == nil {
		return fallback, true
	}
	switch value := args[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), int64(int(value)) == value
	case float64:
		return int(value), !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) && float64(int(value)) == value
	}
	return fallback, false
}
