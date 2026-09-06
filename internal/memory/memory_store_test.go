package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestStoreSaveSearchAndHardDeleteMemory(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.SyncSpeakerIntro("usr_test", "You are speaking with Test User."); err != nil {
		t.Fatal(err)
	}
	entry, err := store.publishFixtureMemory(context.Background(), "usr_test", memoryFixture{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
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

func TestStoreShortTermExpiry(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	_, err := store.publishFixtureMemory(context.Background(), "usr_test", memoryFixture{Scope: ScopeShortTerm, Category: "notes", Statement: "The user is testing expiry.", Evidence: "test", TTL: time.Nanosecond})
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
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")
	ctx := context.Background()
	memory, err := store.publishFixtureMemory(ctx, "usr_test", memoryFixture{Scope: ScopeShortTerm, Category: "notes", Statement: "Temporary marker is ORBITAL.", Evidence: "retained evidence", TTL: time.Hour})
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

func TestStoreDoesNotRecreateStaleAccountUser(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "stale")
	if _, err := store.sql.Exec(`DELETE FROM account_users WHERE canonical_user_id = 'stale'`); err != nil {
		t.Fatal(err)
	}

	_, err := store.publishFixtureMemory(context.Background(), "stale", memoryFixture{Statement: "Must not be saved"})
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
