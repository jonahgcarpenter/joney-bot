package usermemory

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

type fakeMemoryEmbedder struct{}

func (fakeMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

type countingMemoryEmbedder struct {
	inputs []string
}

func (f *countingMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.inputs = append(f.inputs, req.Input)
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

func TestStoreSaveSearchAndHardDeleteMemory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.SyncSpeakerIntro("usr_test", "You are speaking with Test User."); err != nil {
		t.Fatal(err)
	}
	entry, err := store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Scope != ScopeLongTerm || entry.Category != "durable_preferences" {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	rebuildTestIndexes(t, store)
	liveFTS, err := store.LiveIndexRevision(context.Background(), IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Search(context.Background(), "usr_test", ScopeLongTerm, "", "purple", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Statement, "purple") {
		t.Fatalf("expected purple memory, got %+v", entries)
	}

	if err := store.HardDeleteMemory(context.Background(), "usr_test", entry.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var memoryCount, candidateCount, ftsCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE id = ?`, entry.ID).Scan(&memoryCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE published_memory_id = ?`, entry.ID).Scan(&candidateCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM `+liveFTS.TableName+` WHERE rowid = ?`, entry.ID).Scan(&ftsCount); err != nil {
		t.Fatal(err)
	}
	if memoryCount != 0 || candidateCount != 0 || ftsCount != 0 {
		t.Fatalf("hard-deleted content retained: memory=%d candidates=%d fts=%d", memoryCount, candidateCount, ftsCount)
	}
	staleResults, staleStats := store.Recall(context.Background(), "usr_test", "purple", RecallRequest{TopK: 5})
	if staleStats.LexicalError != nil || len(staleResults) != 0 {
		t.Fatalf("stale derived row became observable: results=%+v stats=%+v", staleResults, staleStats)
	}
	entries, err = store.ListMemories("usr_test", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no active memories, got %+v", entries)
	}
}

func TestPublicationCreatesNewObservationAfterHardDelete(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	request := SaveRequest{Scope: ScopeLongTerm, Category: "projects", Statement: "The user builds Atlas.", Evidence: "I build Atlas.", Confidence: 0.9, Importance: 4}
	first, err := store.SaveMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HardDeleteMemory(ctx, "user", first.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("hard-deleted memory was unexpectedly reused")
	}
	var observations int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user' AND published_memory_id = ?`, second.ID).Scan(&observations); err != nil || observations != 1 {
		t.Fatalf("fresh observation count=%d err=%v", observations, err)
	}
}

func TestPublicationDoesNotReuseHardDeletedMemory(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	request := SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "The user keeps a private journal.", Evidence: "original evidence"}
	deleted, err := store.SaveMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HardDeleteMemory(ctx, "user", deleted.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	fresh, err := store.SaveMemory(ctx, "user", request)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == deleted.ID || fresh.Status != StatusActive {
		t.Fatalf("fresh memory=%+v deleted=%+v", fresh, deleted)
	}
	var oldCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE id = ?`, deleted.ID).Scan(&oldCount); err != nil || oldCount != 0 {
		t.Fatalf("deleted memory count=%d err=%v", oldCount, err)
	}
}

func TestPublicationDeduplicatesBackgroundFallbackIdentity(t *testing.T) {
	store := newFormationTestStore(t)
	output := evaluatedFormationCandidate(t, "I build Atlas.", "I build Atlas.", "The user builds Atlas.", memoryformation.CategoryProjects)
	background, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: output, IdempotencyKey: "background-fallback"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := store.SaveMemory(context.Background(), "user", SaveRequest{Scope: ScopeLongTerm, Category: "projects", Statement: output.Statement, Evidence: "additional evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if observation.ID != background.PublishedMemoryID {
		t.Fatalf("reinforced memory=%d background memory=%d", observation.ID, background.PublishedMemoryID)
	}
	var memoryCount, candidateCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id = 'user' AND status = 'active'`).Scan(&memoryCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user' AND published_memory_id = ?`, observation.ID).Scan(&candidateCount); err != nil {
		t.Fatal(err)
	}
	if memoryCount != 1 || candidateCount != 2 {
		t.Fatalf("memory count=%d candidate count=%d", memoryCount, candidateCount)
	}
}

func TestPublicationSupersedesBackgroundFallbackByNormalizedStatement(t *testing.T) {
	store := newFormationTestStore(t)
	oldOutput := evaluatedFormationCandidate(t, "I prefer tea.", "I prefer tea.", "The user prefers tea.", memoryformation.CategoryDurablePreferences)
	oldCandidate, _, err := store.ProposeCandidate(context.Background(), "user", CandidateProposal{Output: oldOutput, IdempotencyKey: "background-tea"})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.SaveMemory(context.Background(), "user", SaveRequest{
		Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user prefers coffee.",
		Evidence: "I prefer coffee.", Confidence: 1, Supersedes: "  THE user prefers tea  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.EntryByID(oldCandidate.PublishedMemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != "superseded" || replacement.SupersedesID != old.ID {
		t.Fatalf("old=%+v replacement=%+v", old, replacement)
	}
}

func TestPublicationReinforcementKeepsTenantScopedIDAndOutbox(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")
	ctx := context.Background()
	first, err := store.SaveMemory(ctx, "user-1", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "User one fact.", Evidence: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveMemory(ctx, "user-2", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "User two private fact.", Evidence: "private"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.SaveMemory(ctx, "user-1", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "User one fact.", Evidence: "updated without embedding"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != first.ID || updated.UserID != "user-1" || updated.Statement != "User one fact." {
		t.Fatalf("reinforcement returned wrong tenant memory: %+v", updated)
	}
	var secondChanges int
	if err := store.sql.QueryRow(`SELECT count(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind = 'memory' AND entity_id = ?`, second.ID).Scan(&secondChanges); err != nil {
		t.Fatal(err)
	}
	if secondChanges != 1 {
		t.Fatalf("foreign tenant derived change count = %d, want 1", secondChanges)
	}
}

func TestStoreShortTermExpiry(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	_, err := store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeShortTerm, Category: "notes", Statement: "The user is testing expiry.", Evidence: "test", TTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	entries, err := store.ListMemories("usr_test", ScopeShortTerm, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected expired memory to be hidden, got %+v", entries)
	}
}

func TestRecallAndListFilterExpiryWithoutMutatingRetentionState(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")
	ctx := context.Background()
	memory, err := store.SaveMemory(ctx, "usr_test", SaveRequest{Scope: ScopeShortTerm, Category: "notes", Statement: "Temporary marker is ORBITAL.", Evidence: "retained evidence", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	if _, err := store.sql.Exec(`UPDATE memory_entries SET expires_at = ? WHERE id = ?`, formatTime(time.Now().Add(-time.Hour)), memory.ID); err != nil {
		t.Fatal(err)
	}

	results, _ := store.Recall(ctx, "usr_test", "ORBITAL", RecallRequest{TopK: 2})
	entries, err := store.ListMemories("usr_test", ScopeShortTerm, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 || len(entries) != 0 {
		t.Fatalf("expired memory served: recall=%+v list=%+v", results, entries)
	}
	var status, statement string
	if err := store.sql.QueryRow(`SELECT status, statement FROM memory_entries WHERE id = ?`, memory.ID).Scan(&status, &statement); err != nil {
		t.Fatal(err)
	}
	if status != StatusActive || statement != "Temporary marker is ORBITAL." {
		t.Fatalf("read path mutated expired memory: status=%q statement=%q", status, statement)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE published_memory_id = ?`, 1, memory.ID)
}

func TestStoreRecentExchangesKeepExactRoles(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fakeMemoryEmbedder{}, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.AppendSessionTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}

	turns, err := store.RecentCompletedExchanges(context.Background(), "usr_test", "session-1", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("unexpected exchanges: %+v", turns)
	}
	messages := SessionTurnMessages(turns[0])
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Content != "I like purple" || messages[1].Role != "assistant" || messages[1].Content != "Noted." {
		t.Fatalf("unexpected messages: %+v", messages)
	}
}

func TestStoreSessionContextKeepsToolMetadataOutOfProseAndScopesUser(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")

	ctx := context.Background()
	if err := store.AppendSessionTurn(ctx, "shared-session", "user-1", "first question", "first answer", []string{"github.get_issue", "web.search"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "shared-session", "user-2", "private question", "private answer", []string{"other.secret"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	retrieved, err := store.RecentCompletedExchanges(ctx, "user-1", "shared-session", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(retrieved) != 1 || retrieved[0].UserText != "first question" || SessionTurnMessages(retrieved[0])[1].Content != "first answer" {
		t.Fatalf("unexpected tenant exchange: %+v", retrieved)
	}
	if strings.Join(retrieved[0].ToolNames, ",") != "github.get_issue,web.search" {
		t.Fatalf("recent tool names = %+v", retrieved[0].ToolNames)
	}

	turns, err := store.RecentCompletedExchanges(ctx, "user-2", "shared-session", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].UserText != "private question" {
		t.Fatalf("unexpected scoped turns: %+v", turns)
	}
}

func TestRecentCompletedExchangesDoNotEmbed(t *testing.T) {
	embedder := &countingMemoryEmbedder{}
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), embedder, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.AppendSessionTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	seedEmbeddingCount := len(embedder.inputs)

	_, err = store.RecentCompletedExchanges(context.Background(), "usr_test", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	queryEmbeddings := embedder.inputs[seedEmbeddingCount:]
	if len(queryEmbeddings) != 0 {
		t.Fatalf("expected no query embeddings from automatic context, got %d: %+v", len(queryEmbeddings), queryEmbeddings)
	}
}

func TestMergeUsersTxCoalescesDuplicatesAndMovesData(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")

	ctx := context.Background()
	if _, err := store.sql.Exec(`UPDATE account_users SET speaker_intro = CASE canonical_user_id WHEN 'winner' THEN 'old winner' ELSE 'old loser' END WHERE canonical_user_id IN ('winner','loser')`); err != nil {
		t.Fatal(err)
	}
	winnerDuplicate, err := store.SaveMemory(ctx, "winner", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Duplicate statement", Evidence: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	loserDuplicate, err := store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Duplicate statement", Evidence: "loser"})
	if err != nil {
		t.Fatal(err)
	}
	loserUnique, err := store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Unique statement", Evidence: "loser"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET supersedes_id = ? WHERE id = ?`, loserDuplicate.ID, loserUnique.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`CREATE TABLE memory_entry_vectors (rowid INTEGER PRIMARY KEY, embedding BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO memory_entry_vectors (rowid, embedding) VALUES (?, X'02')`, loserDuplicate.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "session", "loser", "question", "answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "You are speaking with Winner."); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var duplicateCount, loserCount, loserVectorCount, winnerVectorCount int
	_, duplicateClaimValue := memoryformation.NormalizeClaimIdentity(memoryformation.CategoryNotes, "", "", "Duplicate statement")
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'winner' AND scope = ? AND claim_slot = 'notes.fact' AND claim_value = ?`, ScopeLongTerm, duplicateClaimValue).Scan(&duplicateCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'loser'`).Scan(&loserCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entry_vectors WHERE rowid = ?`, loserDuplicate.ID).Scan(&loserVectorCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entry_vectors WHERE rowid = ?`, winnerDuplicate.ID).Scan(&winnerVectorCount); err != nil {
		t.Fatal(err)
	}
	if duplicateCount != 1 || loserCount != 0 || loserVectorCount != 1 || winnerVectorCount != 0 {
		t.Fatalf("duplicate=%d loser entries=%d loser vectors=%d winner vectors=%d", duplicateCount, loserCount, loserVectorCount, winnerVectorCount)
	}
	var supersedesID int64
	if err := store.sql.QueryRow(`SELECT supersedes_id FROM memory_entries WHERE id = ?`, loserUnique.ID).Scan(&supersedesID); err != nil {
		t.Fatal(err)
	}
	if supersedesID != winnerDuplicate.ID {
		t.Fatalf("supersedes=%d, want %d", supersedesID, winnerDuplicate.ID)
	}
	var turnOwner, intro string
	if err := store.sql.QueryRow(`SELECT canonical_user_id FROM session_turns WHERE session_id = 'session'`).Scan(&turnOwner); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT speaker_intro FROM account_users WHERE canonical_user_id = 'winner'`).Scan(&intro); err != nil {
		t.Fatal(err)
	}
	if turnOwner != "winner" || intro != "You are speaking with Winner." {
		t.Fatalf("turn owner=%q intro=%q", turnOwner, intro)
	}
	var loserProfileCount int
	if err := store.sql.QueryRow(`SELECT count(*) FROM account_users WHERE canonical_user_id = 'loser'`).Scan(&loserProfileCount); err != nil {
		t.Fatal(err)
	}
	if loserProfileCount != 1 {
		t.Fatalf("loser profile count = %d", loserProfileCount)
	}
}

func TestMergeUsersTxRebuildsWinnerVectorAsynchronously(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	winner, err := store.SaveMemory(ctx, "winner", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Shared fact", Evidence: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Shared fact", Evidence: "loser"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "winner intro"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	store.embedder = fixedRecallEmbedder{vector: []float64{0.1, 0.2}}
	rebuildTestIndexes(t, store)
	results, stats := store.Recall(ctx, "winner", "unmatched semantic query", RecallRequest{TopK: 2})
	if stats.SemanticError != nil || len(results) != 1 || results[0].Entry.ID != winner.ID {
		t.Fatalf("merged duplicate semantic recall results=%+v stats=%+v", results, stats)
	}
}

func TestMergeUsersTxPreservesFormationRowsAcrossDuplicatePublication(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	output := evaluatedFormationCandidate(t, "I use Go", "I use Go", "The user uses Go.", memoryformation.CategoryProjects)
	var winnerMemoryID, loserCandidateID int64
	for _, userID := range []string{"winner", "loser"} {
		candidate, created, err := store.ProposeCandidate(ctx, userID, CandidateProposal{Output: output, IdempotencyKey: "same-key", Source: FormationSource{RequestID: userID + "-request"}})
		if err != nil || !created {
			t.Fatalf("propose %s candidate=%+v created=%v err=%v", userID, candidate, created, err)
		}
		if candidate.PublishedMemoryID == 0 {
			t.Fatalf("candidate for %s was not atomically published: %+v", userID, candidate)
		}
		if userID == "winner" {
			winnerMemoryID = candidate.PublishedMemoryID
		} else {
			loserCandidateID = candidate.ID
		}
	}

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "winner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var owner, key string
	var publishedID int64
	if err := store.sql.QueryRow(`SELECT canonical_user_id, idempotency_key, published_memory_id FROM memory_candidates WHERE id = ?`, loserCandidateID).Scan(&owner, &key, &publishedID); err != nil {
		t.Fatal(err)
	}
	if owner != "winner" || !strings.HasPrefix(key, "merge:loser:same-key:") || publishedID != winnerMemoryID {
		t.Fatalf("merged candidate owner=%q key=%q published=%d want=%d", owner, key, publishedID, winnerMemoryID)
	}
	for table, want := range map[string]int{"memory_candidates": 2} {
		var got int
		query := `SELECT COUNT(*) FROM ` + table + ` WHERE canonical_user_id = 'winner'`
		if err := store.sql.QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("merged %s count=%d want=%d", table, got, want)
		}
	}
}

func TestMergeUsersTxUsesMaxConfidenceAndStrongestAssessmentForDuplicate(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	winnerOutput := evaluatedClaimCandidate(t, "I prefer tea", "The user prefers tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.6, "preference.drink", "tea")
	loserOutput := evaluatedClaimCandidate(t, "My preferred beverage is tea", "The user's preferred beverage is tea.", memoryformation.CategoryDurablePreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityIdentityOrContact, 0.9, "preference.drink", "tea")
	winnerCandidate, _, err := store.ProposeCandidate(ctx, "winner", CandidateProposal{Output: winnerOutput, IdempotencyKey: "winner-tea"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ProposeCandidate(ctx, "loser", CandidateProposal{Output: loserOutput, IdempotencyKey: "loser-tea"}); err != nil {
		t.Fatal(err)
	}
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "winner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	memory, err := store.EntryByID(winnerCandidate.PublishedMemoryID)
	if err != nil || memory.Confidence != 0.9 || memory.Statement != loserOutput.Statement || memory.Category != string(loserOutput.Category) || memory.Sensitivity != string(memoryformation.SensitivityIdentityOrContact) || memory.EvidenceCount != 2 {
		t.Fatalf("merged memory=%+v err=%v", memory, err)
	}
}

func TestMergeUsersTxMovesRunningFormationJobAfterSourceTurn(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	if _, err := store.ResolveSessionProfile(ctx, "winner", "shared", time.Hour); err != nil {
		t.Fatal(err)
	}
	loserProfile, err := store.ResolveSessionProfile(ctx, "loser", "shared", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turnCtx := requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "formation-request"})
	turn, err := store.AppendSessionTurnForGenerationResult(turnCtx, "shared", "loser", loserProfile.Generation, "I use Go", "noted", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(ctx, "loser", turn.ID); err != nil {
		t.Fatal(err)
	}
	jobID, err := store.EnqueueFormationJob(ctx, FormationSource{RequestID: "formation-request", SessionID: "shared", SessionGeneration: loserProfile.Generation, TurnID: turn.ID, Model: "model", ExtractorVersion: FormationExtractorVersion}, "loser")
	if err != nil {
		t.Fatal(err)
	}
	artifact := `{"memories":[],"submitted_count":1,"malformed_count":1}`
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'running', lease_owner = 'old-worker', lease_until = ?, invalid_output_retry_count = 1, extraction_payload = ? WHERE id = ?`, formatTime(time.Now().Add(time.Minute)), artifact, jobID); err != nil {
		t.Fatal(err)
	}

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "winner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var owner, state, leaseOwner, mergedArtifact string
	var sourceGeneration, turnGeneration, invalidRetryCount int
	var leaseUntil sql.NullString
	if err := store.sql.QueryRow(`SELECT canonical_user_id, source_session_generation, state, lease_owner, lease_until, invalid_output_retry_count, extraction_payload FROM durable_jobs WHERE id = ?`, jobID).Scan(&owner, &sourceGeneration, &state, &leaseOwner, &leaseUntil, &invalidRetryCount, &mergedArtifact); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT session_generation FROM session_turns WHERE id = ? AND canonical_user_id = 'winner'`, turn.ID).Scan(&turnGeneration); err != nil {
		t.Fatal(err)
	}
	if owner != "winner" || sourceGeneration != turnGeneration || sourceGeneration == loserProfile.Generation || state != "retry" || leaseOwner != "" || leaseUntil.Valid || invalidRetryCount != 1 || mergedArtifact != artifact {
		t.Fatalf("merged formation owner=%q source_generation=%d turn_generation=%d state=%q lease_owner=%q lease_until=%v invalid_retries=%d artifact=%q", owner, sourceGeneration, turnGeneration, state, leaseOwner, leaseUntil, invalidRetryCount, mergedArtifact)
	}
}

func TestMergeUsersTxRollbackLeavesDataUnchanged(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	if _, err := store.SaveMemory(context.Background(), "loser", SaveRequest{Scope: ScopeLongTerm, Statement: "Rollback memory"}); err != nil {
		t.Fatal(err)
	}

	tx, err := store.sql.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(context.Background(), tx, "winner", "loser", "winner intro"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	var loserEntries, winnerProfiles int
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'loser'`).Scan(&loserEntries); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM account_users WHERE canonical_user_id = 'winner' AND speaker_intro != ''`).Scan(&winnerProfiles); err != nil {
		t.Fatal(err)
	}
	if loserEntries != 1 || winnerProfiles != 0 {
		t.Fatalf("loser entries=%d winner profiles=%d", loserEntries, winnerProfiles)
	}
}

func TestMergeUsersTxPreservesProfilesAndCompactedSessionCollision(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()

	_, err := store.SaveMemory(ctx, "winner", SaveRequest{Scope: ScopeLongTerm, Category: "projects", Statement: "Winner builds Atlas", Evidence: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	loserMemory, err := store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "projects", Statement: "Loser builds Beacon", Evidence: "loser"})
	if err != nil {
		t.Fatal(err)
	}
	winnerProfile, err := store.ResolveSessionProfile(ctx, "winner", "shared", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	loserProfile, err := store.ResolveSessionProfile(ctx, "loser", "shared", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	winnerFirst := appendDeliveredCompactionTurn(t, store, "winner", "shared", winnerProfile.Generation, "winner one")
	winnerLast := appendDeliveredCompactionTurn(t, store, "winner", "shared", winnerProfile.Generation, "winner two")
	loserFirst := appendDeliveredCompactionTurn(t, store, "loser", "shared", loserProfile.Generation, "loser one")
	loserLast := appendDeliveredCompactionTurn(t, store, "loser", "shared", loserProfile.Generation, "loser two")
	winnerSummary, winnerJob := publishMergeTestSummary(t, store, "winner", "shared", winnerProfile.Generation, winnerFirst, winnerLast, "winner checkpoint")
	loserSummary, loserJob := publishMergeTestSummary(t, store, "loser", "shared", loserProfile.Generation, loserFirst, loserLast, "loser checkpoint")
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'running', lease_owner = 'old-worker', lease_until = ?, compaction_invalid_output_retry_count = 1 WHERE id = ? AND job_kind = 'session_compaction'`, formatTime(time.Now().Add(time.Minute)), loserJob); err != nil {
		t.Fatal(err)
	}

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "merged intro"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var activeGeneration int
	var activeProfileID int64
	if err := store.sql.QueryRow(`SELECT generation, profile_version FROM sessions WHERE canonical_user_id = 'winner' AND session_id = 'shared'`).Scan(&activeGeneration, &activeProfileID); err != nil {
		t.Fatal(err)
	}
	if activeGeneration <= winnerProfile.Generation || activeProfileID <= winnerProfile.VersionID {
		t.Fatalf("active generation=%d profile=%d, winner generation=%d loser profile=%d", activeGeneration, activeProfileID, winnerProfile.Generation, loserProfile.VersionID)
	}
	var turnCount, summaryCount, jobCount, sessionCount int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'winner' AND session_id = 'shared'`:                                                                                 &turnCount,
		`SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'winner' AND id IN (` + fmt.Sprint(winnerSummary.ID) + `,` + fmt.Sprint(loserSummary.ID) + `)`:                  &summaryCount,
		`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = 'winner' AND id IN (` + fmt.Sprint(winnerJob) + `,` + fmt.Sprint(loserJob) + `)`: &jobCount,
		`SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'winner' AND session_id = 'shared'`:                                                                                      &sessionCount,
	} {
		if err := store.sql.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if turnCount != 4 || summaryCount != 2 || jobCount != 2 || sessionCount != 1 {
		t.Fatalf("turns=%d summaries=%d jobs=%d sessions=%d", turnCount, summaryCount, jobCount, sessionCount)
	}
	var jobState, leaseOwner, artifactPayload, model, generatorVersion string
	var leaseUntil sql.NullString
	var artifactSummaryID int64
	var invalidOutputRetryCount int
	if err := store.sql.QueryRow(`SELECT state, lease_owner, lease_until, artifact_summary_id, artifact_payload, compaction_model, compaction_generator_version, compaction_invalid_output_retry_count FROM durable_jobs WHERE id = ? AND job_kind = 'session_compaction'`, loserJob).Scan(&jobState, &leaseOwner, &leaseUntil, &artifactSummaryID, &artifactPayload, &model, &generatorVersion, &invalidOutputRetryCount); err != nil {
		t.Fatal(err)
	}
	if jobState != "retry" || leaseOwner != "" || leaseUntil.Valid || artifactSummaryID != loserSummary.ID || model != compactionTestModel || generatorVersion != compactionTestGeneratorVersion || invalidOutputRetryCount != 1 {
		t.Fatalf("loser job state=%q owner=%q lease=%v artifact=%d model=%q generator=%q invalid_retries=%d", jobState, leaseOwner, leaseUntil, artifactSummaryID, model, generatorVersion, invalidOutputRetryCount)
	}
	artifact, err := decodeSummaryArtifact(artifactPayload)
	if err != nil || artifact.GenerationModel != compactionTestModel || artifact.GeneratorVersion != compactionTestGeneratorVersion {
		t.Fatalf("merged summary artifact=%+v err=%v", artifact, err)
	}
	latest, err := store.LatestSessionSummary(ctx, "winner", "shared", activeGeneration)
	if err != nil || latest.ID != loserSummary.ID || latest.Narrative != "loser checkpoint" {
		t.Fatalf("latest merged summary=%+v err=%v", latest, err)
	}
	resolved, err := store.ResolveSessionProfile(ctx, "winner", "shared", time.Hour)
	if err != nil || resolved.VersionID != activeProfileID {
		t.Fatalf("frozen merged profile=%+v err=%v", resolved, err)
	}
	contextResult, err := store.RecentCompletedExchanges(ctx, "winner", "shared", activeGeneration, 10)
	if err != nil || len(contextResult) == 0 || !strings.Contains(contextResult[0].UserText, "loser two") {
		t.Fatalf("merged prompt exchanges=%+v err=%v", contextResult, err)
	}
	rebuildTestIndexes(t, store)
	transcript, err := store.SearchTranscript(ctx, "winner", "shared", activeGeneration, "loser two", 5)
	if err != nil || len(transcript) == 0 || transcript[0].TurnID != loserLast {
		t.Fatalf("merged transcript=%+v err=%v", transcript, err)
	}
	recalled, _ := store.Recall(ctx, "winner", "Loser builds Beacon", RecallRequest{TopK: 5})
	if len(recalled) == 0 || recalled[0].Entry.ID != loserMemory.ID {
		t.Fatalf("merged recall=%+v", recalled)
	}
	memories, err := store.ListMemories("winner", "", "", 10)
	if err != nil || len(memories) != 2 {
		t.Fatalf("merged recall source memories=%+v err=%v", memories, err)
	}
}

func publishMergeTestSummary(t *testing.T, store *Store, userID, sessionID string, generation int, fromID, throughID int64, narrative string) (SessionSummary, int64) {
	t.Helper()
	jobID, err := store.EnqueueSessionCompactionCampaignJob(context.Background(), userID, sessionID, generation, fromID, throughID, throughID, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "merge-test", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || job.ID != jobID {
		t.Fatalf("claim merge job=%+v want=%d err=%v", job, jobID, err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: narrative, GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.PublishSessionSummary(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	return summary, jobID
}

func TestStoreDoesNotRecreateStaleAccountUser(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "stale")
	if _, err := store.sql.Exec(`DELETE FROM account_users WHERE canonical_user_id = 'stale'`); err != nil {
		t.Fatal(err)
	}

	_, err := store.SaveMemory(context.Background(), "stale", SaveRequest{Statement: "Must not be saved"})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("stale user error = %v", err)
	}
	var accountCount int
	if err := store.sql.QueryRow(`SELECT count(*) FROM account_users WHERE canonical_user_id = 'stale'`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 0 {
		t.Fatalf("stale account was recreated")
	}
}

func seedAccountUsers(t *testing.T, store *Store, userIDs ...string) {
	t.Helper()
	for _, userID := range userIDs {
		if _, err := store.sql.Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?)`, userID); err != nil {
			t.Fatalf("seed account user %q: %v", userID, err)
		}
	}
}
