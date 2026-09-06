package websearch

import (
	"context"
	"errors"
	"testing"
)

type countingSearcher struct {
	response SearchResponse
	err      error
	calls    int
}

func (s *countingSearcher) Search(context.Context, string) (SearchResponse, error) {
	s.calls++
	return s.response, s.err
}

func TestFallbackSearcherUsesPrimaryWhenUsable(t *testing.T) {
	primary := &countingSearcher{response: SearchResponse{Results: []SearchResult{{Title: "Brave"}}}}
	fallback := &countingSearcher{}
	response, err := NewFallbackSearcher(primary, fallback, nil).Search(context.Background(), "test")
	if err != nil || len(response.Results) != 1 || fallback.calls != 0 {
		t.Fatalf("response=%+v err=%v fallback_calls=%d", response, err, fallback.calls)
	}
}

func TestFallbackSearcherHandlesEmptyAndFailedPrimary(t *testing.T) {
	for _, test := range []struct {
		name         string
		primary      *countingSearcher
		wantDegraded bool
		wantMissing  string
	}{
		{name: "empty", primary: &countingSearcher{}},
		{name: "filtered", primary: &countingSearcher{response: SearchResponse{Stats: CandidateStats{CandidateCount: 1, FilteredCount: 1}}}, wantDegraded: true, wantMissing: "brave"},
		{name: "failed", primary: &countingSearcher{err: errors.New("failed")}, wantDegraded: true, wantMissing: "brave"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fallback := &countingSearcher{response: SearchResponse{Results: []SearchResult{{Title: "SearXNG"}}, Stats: CandidateStats{CandidateCount: 2}}}
			response, err := NewFallbackSearcher(test.primary, fallback, nil).Search(context.Background(), "test")
			if err != nil || fallback.calls != 1 || response.Degraded != test.wantDegraded || response.Stats.CandidateCount != test.primary.response.Stats.CandidateCount+2 {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			if test.wantMissing != "" && len(response.UnresponsiveEngines) != 1 || test.wantMissing != "" && response.UnresponsiveEngines[0] != test.wantMissing {
				t.Fatalf("missing providers = %v", response.UnresponsiveEngines)
			}
		})
	}
}

func TestFallbackSearcherReturnsValidEmptyPrimaryWhenFallbackFails(t *testing.T) {
	primary := &countingSearcher{response: SearchResponse{Stats: CandidateStats{CandidateCount: 1}}}
	fallback := &countingSearcher{err: errors.New("failed")}
	response, err := NewFallbackSearcher(primary, fallback, nil).Search(context.Background(), "test")
	if err != nil || !response.Degraded || len(response.UnresponsiveEngines) != 1 || response.UnresponsiveEngines[0] != "searxng" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestFallbackSearcherFailsWhenBothProvidersFail(t *testing.T) {
	primary := &countingSearcher{err: errors.New("primary")}
	fallback := &countingSearcher{err: errors.New("fallback")}
	if _, err := NewFallbackSearcher(primary, fallback, nil).Search(context.Background(), "test"); err == nil {
		t.Fatal("dual provider failure returned nil error")
	}
}

type cancelingSearcher struct{}

func (cancelingSearcher) Search(ctx context.Context, _ string) (SearchResponse, error) {
	<-ctx.Done()
	return SearchResponse{}, ctx.Err()
}

func TestFallbackSearcherDoesNotFallbackAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fallback := &countingSearcher{}
	_, err := NewFallbackSearcher(cancelingSearcher{}, fallback, nil).Search(ctx, "test")
	if !errors.Is(err, context.Canceled) || fallback.calls != 0 {
		t.Fatalf("err=%v fallback_calls=%d", err, fallback.calls)
	}
}
