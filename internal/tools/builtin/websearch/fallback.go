package websearch

import (
	"context"
	"errors"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// FallbackSearcher uses fallback only when primary fails or returns no usable results.
type FallbackSearcher struct {
	primary  Searcher
	fallback Searcher
	log      *config.Logger
}

// NewFallbackSearcher creates a Brave-first searcher with SearXNG fallback.
func NewFallbackSearcher(primary, fallback Searcher, log *config.Logger) *FallbackSearcher {
	return &FallbackSearcher{primary: primary, fallback: fallback, log: log}
}

// Search returns primary results when usable and otherwise tries the fallback.
func (s *FallbackSearcher) Search(ctx context.Context, query string) (response SearchResponse, err error) {
	ctx, complete := beginSearch(ctx, s.log, "fallback")
	usedFallback := false
	defer func() {
		if usedFallback {
			complete(response, err)
		}
	}()
	primary, primaryErr := s.primary.Search(ctx, query)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return SearchResponse{}, ctxErr
	}
	if primaryErr == nil && len(primary.Results) > 0 {
		return primary, nil
	}

	usedFallback = true
	fallback, fallbackErr := s.fallback.Search(ctx, query)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return SearchResponse{}, ctxErr
	}
	if fallbackErr == nil {
		fallback.Stats = addCandidateStats(primary.Stats, fallback.Stats)
		if primaryErr != nil || primary.Stats.CandidateCount > 0 {
			fallback.Degraded = true
			fallback.UnresponsiveEngines = mergeNames(fallback.UnresponsiveEngines, []string{"brave"}, maxUnresponsiveEngines)
		}
		return fallback, nil
	}
	if primaryErr == nil {
		primary.Degraded = true
		primary.UnresponsiveEngines = mergeNames(primary.UnresponsiveEngines, []string{"searxng"}, maxUnresponsiveEngines)
		return primary, nil
	}
	return SearchResponse{}, errors.New("all web search providers failed")
}

func addCandidateStats(left, right CandidateStats) CandidateStats {
	return CandidateStats{
		CandidateCount: left.CandidateCount + right.CandidateCount,
		InspectedCount: left.InspectedCount + right.InspectedCount,
		FilteredCount:  left.FilteredCount + right.FilteredCount,
		DuplicateCount: left.DuplicateCount + right.DuplicateCount,
	}
}
