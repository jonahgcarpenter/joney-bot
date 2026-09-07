package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestLexicalLongQueryEvaluation(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "eval.db"), config.NewLogger(config.LevelError))
	defer store.Close()
	seedAccountUsers(t, store, "eval-user")
	ctx := context.Background()
	statements := []string{
		"My Atlas deployment uses C++.",
		"My Atlas deployment uses C#.",
		"I enjoy deployment podcasts during morning walks.",
		"I review weekend recipes with friends.",
		"Atlas maps decorate my living room.",
	}
	var want int64
	for i, statement := range statements {
		entry, err := store.publishFixtureMemory(ctx, "eval-user", memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: statement, Confidence: 0.9})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			want = entry.ID
		}
	}
	rebuildTestIndexes(t, store)
	query := "Can you help me review my Atlas C++ deployment before the weekend release and suggest practical debugging steps?"
	results, stats := store.Recall(ctx, "eval-user", query, RecallRequest{TopK: 3})
	if stats.LexicalError != nil || stats.SemanticAvailable || len(results) != 1 || results[0].Entry.ID != want {
		t.Fatalf("lexical-only evaluation results=%+v stats=%+v", results, stats)
	}
	terms := ftsRecallTerms(query)
	baseline := 3.0 / float64(len(terms))
	candidate := lexicalRecallCoverage(statements[0], terms)
	if baseline >= defaultRecallMinRelevance || candidate < defaultRecallMinRelevance {
		t.Fatalf("expected baseline miss and candidate hit: baseline=%f candidate=%f", baseline, candidate)
	}
	t.Logf("corpus=lexical-long-query-v1 baseline_recall_at_3=0 candidate_recall_at_3=1 candidate_precision_at_3=1 baseline_relevance=%.3f candidate_relevance=%.3f", baseline, candidate)
}

func TestRecallMeaningfulPunctuationAndNegationRemainDistinct(t *testing.T) {
	for _, pair := range [][2]string{
		{"I use C++ for the Atlas production deployment", "I use C# for the Atlas production deployment"},
		{"I use C for the Atlas production deployment", "I use C++ for the Atlas production deployment"},
		{"I like the quiet morning train ride to work", "I do not like the quiet morning train ride to work"},
		{"I can deploy the Atlas service on Friday", "I can't deploy the Atlas service on Friday"},
		{"My production runtime is version 3.1", "My production runtime is version 31"},
	} {
		if nearDuplicateRecallText(pair[0], pair[1]) || duplicateRecallText(pair[0]) == duplicateRecallText(pair[1]) {
			t.Errorf("meaningfully distinct claims collapsed: %q / %q", pair[0], pair[1])
		}
	}
	if !nearDuplicateRecallText("I use C++ for Atlas.", "I use C++ for Atlas!") {
		t.Fatal("cosmetic sentence punctuation should still deduplicate")
	}
}

func TestLexicalCoverageDoesNotBoostLoneDistractorsOrWeakInferences(t *testing.T) {
	terms := ftsRecallTerms("Please help plan my Atlas deployment debugging release review this weekend")
	if got := lexicalRecallCoverage("Atlas", terms); got >= defaultRecallMinRelevance {
		t.Fatalf("lone distractor relevance=%f", got)
	}
	results := RankDurableMemories([]RecallCandidate{{Entry: MemoryEntry{ID: 1, Statement: "Atlas deployment", Confidence: 0.34}, Source: RecallSourceLexical, Relevance: lexicalRecallCoverage("Atlas deployment", terms), Authority: RecallAuthorityInferred}}, RecallOptions{UncertaintyAware: true})
	if len(results) != 0 {
		t.Fatalf("unsupported inference promoted: %+v", results)
	}
}

func TestLexicalRecallCanonicalEligibilityOverridesStaleIndex(t *testing.T) {
	for _, state := range []string{"expired", "superseded"} {
		t.Run(state, func(t *testing.T) {
			store := newTestStore(filepath.Join(t.TempDir(), "eval.db"), config.NewLogger(config.LevelError))
			defer store.Close()
			seedAccountUsers(t, store, "eval-user")
			ctx := context.Background()
			entry, err := store.publishFixtureMemory(ctx, "eval-user", memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: "Atlas deployment uses C++.", Confidence: 0.9})
			if err != nil {
				t.Fatal(err)
			}
			rebuildTestIndexes(t, store)
			// Deliberately leave a stale physical index to exercise canonical checks.
			if state == "expired" {
				_, err = store.sql.Exec(`UPDATE memory_entries SET expires_at = ? WHERE id = ?`, formatTime(time.Now().Add(-time.Hour)), entry.ID)
			} else {
				_, err = store.sql.Exec(`UPDATE memory_entries SET status = 'superseded' WHERE id = ?`, entry.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			results, stats := store.Recall(ctx, "eval-user", "Help debug my Atlas C++ deployment before the weekend release", RecallRequest{})
			if stats.LexicalError != nil || len(results) != 0 {
				t.Fatalf("stale canonical memory served: results=%+v stats=%+v", results, stats)
			}
		})
	}
}
