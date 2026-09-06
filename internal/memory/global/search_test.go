package global

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSearchUsesCanonicalFallbackAndEnforcesBounds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for i := 1; i <= MaxSearchLimit+3; i++ {
		if _, err := store.Add(ctx, fmt.Sprintf("Shared search topic %02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	results, stats := store.Search(ctx, "shared search topic", 1)
	if len(results) != 1 || results[0].Memory.ID != 1 || results[0].LexicalScore != 1 || results[0].Score != 1 || strings.Join(results[0].Sources, ",") != "lexical" {
		t.Fatalf("fallback results = %+v", results)
	}
	if !stats.LexicalAvailable || stats.SemanticAvailable || stats.LexicalError == nil || stats.SelectedCount != 1 {
		t.Fatalf("fallback stats = %+v", stats)
	}
	defaulted, _ := store.Search(ctx, "shared search topic", 0)
	if len(defaulted) != DefaultSearchLimit {
		t.Fatalf("default limit returned %d, want %d", len(defaulted), DefaultSearchLimit)
	}
	capped, _ := store.Search(ctx, "shared search topic", MaxSearchLimit+100)
	if len(capped) != MaxSearchLimit {
		t.Fatalf("maximum limit returned %d, want %d", len(capped), MaxSearchLimit)
	}
	empty, _ := store.Search(ctx, "  ", 5)
	if len(empty) != 0 {
		t.Fatalf("empty query returned %+v", empty)
	}
}

func TestSearchLexicalOnlyHandlesNaturalLanguageQuery(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Add(context.Background(), "Oswald is implemented primarily in Go."); err != nil {
		t.Fatal(err)
	}
	results, stats := store.Search(context.Background(), "What language is Oswald implemented in?", 5)
	if !stats.LexicalAvailable || stats.SemanticAvailable || len(results) != 1 || !strings.Contains(results[0].Memory.Text, "Go") {
		t.Fatalf("natural-language lexical results=%+v stats=%+v", results, stats)
	}
}
