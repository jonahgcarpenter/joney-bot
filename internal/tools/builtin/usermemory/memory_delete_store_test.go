package usermemory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestHardDeleteMemoryRemovesMemoryGraphAndKeepsTranscript(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	seedAccountUsers(t, store, "user", "other")
	target, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "I like purple.", Confidence: 1, Importance: 5})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.SaveMemory(ctx, "other", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "The other user likes blue.", Evidence: "I like blue."})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if profile.FactCount != 1 {
		t.Fatalf("initial profile=%+v", profile)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "I like purple.", "Noted.", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HardDeleteMemory(ctx, "user", target.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rebound, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil || rebound.FactCount != 0 || rebound.Bytes != len(rebound.Content) || rebound.Generation != profile.Generation {
		t.Fatalf("rebound profile=%+v err=%v", rebound, err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_entries WHERE id = ?`, 0, target.ID)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE published_memory_id = ?`, 0, target.ID)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE canonical_user_id = 'user' AND entity_kind = 'memory' AND entity_id = ?`, 0, target.ID)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ?`, 1, turn.ID)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_entries WHERE id = ? AND canonical_user_id = 'other'`, 1, other.ID)
	assertForeignKeysClean(t, store)
}

func TestHardDeleteAllUserDataPreservesAccountAndResetsEverySession(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	seedAccountUsers(t, store, "user", "other")
	if _, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Category: "identity", Statement: "The user's name is Ada.", Evidence: "My name is Ada."}); err != nil {
		t.Fatal(err)
	}
	other, err := store.SaveMemory(ctx, "other", SaveRequest{Scope: ScopeLongTerm, Category: "identity", Statement: "The other user's name is Bob.", Evidence: "My name is Bob."})
	if err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{"one", "two"} {
		profile, err := store.ResolveSessionProfile(ctx, "user", sessionID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		turn, err := store.AppendSessionTurnForGenerationResult(ctx, sessionID, "user", profile.Generation, "hello", "hi", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = created_at WHERE id = ?`, turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.sql.Exec(`INSERT INTO mcp_servers(id,scope,owner_user_id,name,transport,url_ciphertext) VALUES ('user-server','user','user','home','streamable_http',X'01')`); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.HardDeleteAllUserData(ctx, "user", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0] != "one" || sessions[1] != "two" {
		t.Fatalf("reset sessions=%v", sessions)
	}
	for _, table := range []string{"memory_entries", "memory_candidates", "session_turns", "session_summaries"} {
		assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+table+` WHERE canonical_user_id = 'user'`, 0)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE canonical_user_id = 'user'`, 0)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM mcp_servers WHERE owner_user_id = 'user'`, 0)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = 'user'`, 1)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM sessions WHERE canonical_user_id = 'user' AND is_active = 0 AND source_memory_ids = '[]'`, 2)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_entries WHERE id = ? AND canonical_user_id = 'other'`, 1, other.ID)
	fresh, err := store.ResolveSessionProfile(ctx, "user", "one", time.Hour)
	if err != nil || !fresh.IsNewSession || fresh.FactCount != 0 || fresh.Generation <= 1 {
		t.Fatalf("fresh session=%+v err=%v", fresh, err)
	}
	assertForeignKeysClean(t, store)
}

func assertForeignKeysClean(t *testing.T, store *Store) {
	t.Helper()
	rows, err := store.sql.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key check returned violations")
	}
}
