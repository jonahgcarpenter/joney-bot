package webfetch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

// NewHandler returns an authenticated handler for direct public-page retrieval.
func NewHandler(fetcher Fetcher, log *config.Logger) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("web fetch requires an authenticated request")
		}
		rawURL, _ := args["url"].(string)
		rawURL = strings.TrimSpace(rawURL)
		if _, err := validateURL(rawURL); err != nil {
			return governance.Result{}, err
		}
		meta := requestctx.MetadataFromContext(ctx)
		agentLog := log.Agent("agent.tool.web.fetch", meta.RequestID, meta.SessionID, principal.CanonicalUserID, principal.Gateway, meta.Model)
		agentLog.Debug("agent.tool.web.fetch.start", "starting direct public page fetch",
			config.F("tool_name", toolnames.WebFetch), config.F("url_chars", utf8.RuneCountInString(rawURL)))

		response, err := fetcher.Fetch(ctx, rawURL)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return governance.Result{}, fmt.Errorf("fetch canceled: %w", ctxErr)
			}
			return governance.Result{}, errors.New("public page fetch failed")
		}
		result := governance.Result{Outcome: governance.OutcomeProductive, IsDegraded: response.IsDegraded}
		if strings.TrimSpace(response.Content) == "" {
			result.Outcome = governance.OutcomeUnproductive
			result.ReasonCode = "no_readable_content"
		}
		content, err := encodeBoundedResponse(response)
		if err != nil {
			return governance.Result{}, err
		}
		boundedResponse, err := DecodeToolResponse(content)
		if err != nil {
			return governance.Result{}, err
		}
		result.IsDegraded = boundedResponse.IsDegraded
		if boundedResponse.IsTruncated && result.ReasonCode == "" {
			result.ReasonCode = "output_truncated"
		}
		result.Content = content
		agentLog.Debug("agent.tool.web.fetch.complete", "completed direct public page fetch",
			config.F("tool_name", toolnames.WebFetch), config.F("source", boundedResponse.Source),
			config.F("content_chars", utf8.RuneCountInString(boundedResponse.Content)),
			config.F("is_truncated", boundedResponse.IsTruncated), config.F("is_degraded", boundedResponse.IsDegraded), config.F("status", "ok"))
		return result, nil
	}
}
