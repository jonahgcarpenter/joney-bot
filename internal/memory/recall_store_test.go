package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

type fixedRecallEmbedder struct {
	vector []float64
	err    error
}

type panicRecallEmbedder struct{}

func (panicRecallEmbedder) Embed(context.Context, llm.EmbedRequest) (*llm.EmbedResponse, error) {
	panic("embedding provider called while semantic recall is disabled")
}

func (f fixedRecallEmbedder) Embed(context.Context, llm.EmbedRequest) (*llm.EmbedResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{append([]float64(nil), f.vector...)}}, nil
}

func TestRecallFTSFindsExactTermsAndScopesTenant(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")
	ctx := context.Background()
	_, err := store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "The deployment identifier is ZXQ-741.", Evidence: "project notes", Confidence: 0.9, Importance: 4, SourceSessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.publishFixtureMemory(ctx, "user-2", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "Private ZXQ-741 details for another tenant.", Evidence: "private", Confidence: 1, Importance: 5})
	if err != nil {
		t.Fatal(err)
	}

	rebuildTestIndexes(t, store)
	results, stats := store.Recall(ctx, "user-1", "ZXQ-741", RecallRequest{TopK: 4})
	if stats.LexicalError != nil || !stats.LexicalAvailable || stats.LexicalCandidateCount != 1 {
		t.Fatalf("unexpected FTS stats: %+v", stats)
	}
	if len(results) != 1 || results[0].Entry.UserID != "user-1" || strings.Contains(results[0].Entry.Statement, "Private") {
		t.Fatalf("tenant-scoped FTS results = %+v", results)
	}
	if results[0].Authority != RecallAuthorityUserStated || len(results[0].Provenance) == 0 || results[0].Provenance[0].Authority != RecallAuthorityUserStated {
		t.Fatalf("published statement authority was not preserved: %+v", results[0])
	}
	wantRevision := results[0].Entry.Revision + 1
	if _, err := store.sql.Exec(`UPDATE memory_entries SET assessment_context = 'Only for the retired deployment.' WHERE id = ?`, results[0].Entry.ID); err != nil {
		t.Fatal(err)
	}
	results, stats = store.Recall(ctx, "user-1", "ZXQ-741", RecallRequest{TopK: 4})
	if stats.LexicalError != nil || len(results) != 1 || results[0].Entry.Revision != wantRevision || results[0].Entry.Context != "Only for the retired deployment." {
		t.Fatalf("automatic recall did not hydrate canonical revision: %+v %+v", results, stats)
	}
	rendered := RenderDurableMemoryRecall(results, 12000)
	for _, field := range []string{fmt.Sprintf(`"revision":%d`, wantRevision), `"claim_slot":`, `"claim_value":`, `"assessment_context":"Only for the retired deployment."`} {
		if !strings.Contains(rendered, field) {
			t.Fatalf("automatic recall missing %s: %s", field, rendered)
		}
	}
}

func TestRecallPreservesUnknownPersistedAuthority(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	memory, err := store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "The deployment identifier is ZXQ-741.", Confidence: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	// Existing v4 memories can carry unknown provenance even though new saves do not.
	if _, err := store.sql.Exec(`UPDATE memory_entries SET provenance_type = 'legacy_import' WHERE id = ?`, memory.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_candidates SET provenance_type = 'legacy_import' WHERE published_memory_id = ?`, memory.ID); err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	results, stats := store.Recall(ctx, "user-1", "ZXQ-741", RecallRequest{TopK: 2})
	if stats.LexicalError != nil || len(results) != 1 {
		t.Fatalf("unknown provenance recall results=%+v stats=%+v", results, stats)
	}
	if results[0].Authority != RecallAuthorityUnknown || len(results[0].Provenance) == 0 || results[0].Provenance[0].Authority != RecallAuthorityUnknown {
		t.Fatalf("unknown persisted provenance was promoted: %+v", results[0])
	}
}

func TestRecallFTSHydratesEvidenceFromCanonicalCandidate(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	memory, err := store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "Project marker is Meridian.", Evidence: "stale indexed evidence", Confidence: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	if _, err := store.sql.Exec(`UPDATE memory_candidates SET evidence = 'current canonical evidence' WHERE canonical_user_id = ? AND published_memory_id = ?`, "user-1", memory.ID); err != nil {
		t.Fatal(err)
	}

	results, stats := store.Recall(ctx, "user-1", "Meridian", RecallRequest{TopK: 2})
	if stats.LexicalError != nil || len(results) != 1 || results[0].Entry.Evidence != "current canonical evidence" {
		t.Fatalf("canonical evidence recall results=%+v stats=%+v", results, stats)
	}
}

func TestRecallAllStopWordsIsAvailableEmpty(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	rebuildTestIndexes(t, store)

	results, stats := store.Recall(context.Background(), "user-1", "what is the user", RecallRequest{TopK: 2})
	if stats.LexicalError != nil || !stats.LexicalAvailable || len(results) != 0 || stats.LexicalCandidateCount != 0 {
		t.Fatalf("all-stop-word recall results=%+v stats=%+v", results, stats)
	}
}

func TestRecallVectorPrefilterPreventsForeignNeighborCrowding(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")
	ctx := context.Background()
	_, err = store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user's favorite color is purple.", Evidence: "user statement", Confidence: 0.9, Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		_, err = store.publishFixtureMemory(ctx, "user-2", memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: fmt.Sprintf("Foreign private fact %d", i), Evidence: "foreign", Confidence: 1, Importance: 5})
		if err != nil {
			t.Fatal(err)
		}
	}

	rebuildTestIndexes(t, store)
	results, stats := store.Recall(ctx, "user-1", "Which shade do I enjoy?", RecallRequest{TopK: 4})
	if stats.SemanticError != nil || !stats.SemanticAvailable || stats.SemanticCandidateCount != 1 {
		t.Fatalf("unexpected vector stats: %+v", stats)
	}
	if len(results) != 1 || results[0].Entry.UserID != "user-1" || !strings.Contains(results[0].Entry.Statement, "purple") {
		t.Fatalf("tenant-prefiltered vector results = %+v", results)
	}
}

func TestRecallVectorPrefiltersScopeBeforeKNN(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	_, err = store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "Project home is Lisbon.", Evidence: "user statement"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		_, err = store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "notes", Statement: fmt.Sprintf("Closer unrelated note %d", i), Evidence: "note"})
		if err != nil {
			t.Fatal(err)
		}
	}
	rebuildTestIndexes(t, store)
	results, stats := store.Recall(ctx, "user-1", "Where is the base?", RecallRequest{Category: "projects", CandidateLimit: 4, TopK: 2})
	if stats.SemanticError != nil || len(results) != 1 || results[0].Entry.Category != "projects" {
		t.Fatalf("metadata-prefiltered results=%+v stats=%+v", results, stats)
	}
}

func TestRecallRemovesInactiveVectorsBeforeKNN(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	_, err = store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "identity", Statement: "Active home city is Porto.", Evidence: "user statement"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		statement := fmt.Sprintf("Obsolete home city %d", i)
		obsolete, saveErr := store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "identity", Statement: statement, Evidence: "obsolete"})
		if saveErr != nil {
			t.Fatal(saveErr)
		}
		if err := store.HardDeleteMemory(ctx, "user-1", obsolete.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	rebuildTestIndexes(t, store)
	results, stats := store.Recall(ctx, "user-1", "Where is home?", RecallRequest{CandidateLimit: 4, TopK: 2})
	if stats.SemanticError != nil || len(results) != 1 || !strings.Contains(results[0].Entry.Statement, "Porto") {
		t.Fatalf("active vector results=%+v stats=%+v", results, stats)
	}
}

func TestRecallDegradesIndependentlyWhenFTSIsUnavailable(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	_, err = store.publishFixtureMemory(context.Background(), "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "identity", Statement: "The user lives in Lisbon.", Evidence: "user statement", Confidence: 1, Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	liveFTS, err := store.LiveIndexRevision(context.Background(), IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`DROP TABLE ` + liveFTS.TableName); err != nil {
		t.Fatal(err)
	}

	results, stats := store.Recall(context.Background(), "user-1", "Where is home?", RecallRequest{TopK: 2})
	if stats.LexicalError == nil || stats.SemanticError != nil || len(results) != 1 {
		t.Fatalf("degraded hybrid recall results=%+v stats=%+v", results, stats)
	}
}

func TestRecallFallsBackToFTSWhenEmbeddingFails(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{err: fmt.Errorf("embedding offline")}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	_, err = store.publishFixtureMemory(context.Background(), "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "Exact project name is Meridian.", Evidence: "user statement"})
	if err != nil {
		t.Fatal(err)
	}

	rebuildTestIndexes(t, store)
	results, stats := store.Recall(context.Background(), "user-1", "Meridian", RecallRequest{TopK: 2})
	if stats.SemanticError == nil || stats.LexicalError != nil || len(results) != 1 {
		t.Fatalf("FTS fallback results=%+v stats=%+v", results, stats)
	}
}

func TestRecallDoesNotDropIncompatibleLiveVectorIndex(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	_, err = store.publishFixtureMemory(context.Background(), "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "Project codename is Helios.", Evidence: "user statement"})
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	store.embedder = fixedRecallEmbedder{vector: []float64{1, 0, 0}}
	results, stats := store.Recall(context.Background(), "user-1", "Helios", RecallRequest{TopK: 2})
	if stats.SemanticError == nil || stats.LexicalError != nil || len(results) != 1 {
		t.Fatalf("dimension fallback results=%+v stats=%+v", results, stats)
	}
	liveVector, err := store.LiveIndexRevision(context.Background(), IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if dimension, ok := store.vectorTableDimension(liveVector.TableName); !ok || dimension != 2 {
		t.Fatalf("live vector dimension = %d available=%v, want unchanged dimension 2", dimension, ok)
	}
}

func TestRecallDisablesSemanticWhenConfiguredModelIsEmpty(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	if _, err := store.publishFixtureMemory(context.Background(), "user-1", memoryFixture{Scope: ScopeLongTerm, Statement: "Semantic recall can be disabled."}); err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	live, err := store.LiveIndexRevision(context.Background(), IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	store.embedModel = ""
	store.embedder = panicRecallEmbedder{}
	_, stats := store.Recall(context.Background(), "user-1", "disabled", RecallRequest{TopK: 2})
	if stats.SemanticAvailable || stats.SemanticError != nil || stats.SemanticCandidateCount != 0 {
		t.Fatalf("semantic recall was not disabled: %+v", stats)
	}
	retained, err := store.LiveIndexRevision(context.Background(), IndexKindMemoryVector)
	if err != nil || retained.ID != live.ID {
		t.Fatalf("disabled semantic recall changed retained live revision: retained=%+v err=%v", retained, err)
	}
}

func TestRecallExcludesSupersededCorrection(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	_, err := store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "The launch meeting is Monday.", Evidence: "initial schedule"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.publishFixtureMemory(ctx, "user-1", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "The launch meeting is Tuesday.", Evidence: "corrected schedule", Supersedes: "The launch meeting is Monday."})
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	results, _ := store.Recall(ctx, "user-1", "launch meeting", RecallRequest{TopK: 4})
	if len(results) != 1 || !strings.Contains(results[0].Entry.Statement, "Tuesday") {
		t.Fatalf("correction recall results = %+v", results)
	}
}

func TestRecallOmitsExpiredMemories(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	_, err := store.publishFixtureMemory(context.Background(), "user-1", memoryFixture{Scope: ScopeShortTerm, Category: "notes", Statement: "Temporary launch code ORBITAL.", Evidence: "temporary", TTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	rebuildTestIndexes(t, store)
	results, _ := store.Recall(context.Background(), "user-1", "ORBITAL", RecallRequest{TopK: 2})
	if len(results) != 0 {
		t.Fatalf("expired recall results = %+v", results)
	}
}

func TestMergeMovesTenantVectorOwnership(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	_, err = store.publishFixtureMemory(context.Background(), "loser", memoryFixture{Scope: ScopeLongTerm, Category: "identity", Statement: "The user was born in Porto.", Evidence: "user statement"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.sql.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() // nolint:errcheck
	if err := store.MergeUsersTx(context.Background(), tx, "winner", "loser", ""); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	results, stats := store.Recall(context.Background(), "winner", "Where was I born?", RecallRequest{TopK: 2})
	if stats.SemanticError != nil || len(results) != 1 || results[0].Entry.UserID != "winner" {
		t.Fatalf("merged recall results=%+v stats=%+v", results, stats)
	}
	var loserVectors int
	liveVector, err := store.LiveIndexRevision(context.Background(), IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM ` + liveVector.TableName + ` WHERE canonical_user_id = 'loser'`).Scan(&loserVectors); err != nil {
		t.Fatal(err)
	}
	if loserVectors != 0 {
		t.Fatalf("loser vector count = %d", loserVectors)
	}
}
