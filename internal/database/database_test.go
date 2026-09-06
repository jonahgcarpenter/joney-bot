package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestAccountLinkChallengesSchema(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() // nolint:errcheck
	removedColumns := map[string][]string{
		"account_users":           {"created_at", "updated_at", "banned_at", "banned_by"},
		"linked_accounts":         {"linked_at"},
		"account_link_challenges": {"created_at", "consumed_by_user_id", "invalidated_by_user_id", "invalidated_reason"},
		"mcp_servers":             {"type", "url_host_hash", "created_at", "updated_at"},
	}
	for table, columns := range removedColumns {
		for _, column := range columns {
			var count int
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
				t.Fatalf("inspect %s.%s: %v", table, column, err)
			}
			if count != 0 {
				t.Fatalf("removed column %s.%s still exists", table, column)
			}
		}
	}

	_, err = db.SQL().Exec(`
INSERT INTO account_link_challenges (
	id, code_hash, initiator_user_id, initiator_gateway, initiator_identifier,
	expires_at, consumed_at, consumed_gateway, consumed_identifier, result_user_id, invalidated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"challenge-1", "hash-1", "user-1", "discord", "account-1",
		"2026-07-18T12:10:00Z", nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("insert challenge: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO account_link_challenges (
	id, code_hash, initiator_user_id, initiator_gateway, initiator_identifier, expires_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		"challenge-2", "hash-1", "user-2", "homeassistant", "account-2",
		"2026-07-18T12:10:00Z",
	); err == nil {
		t.Fatal("expected duplicate code_hash to fail")
	}

	rows, err := db.SQL().Query(`PRAGMA foreign_key_list(account_link_challenges)`)
	if err != nil {
		t.Fatalf("inspect foreign keys: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("account_link_challenges must not have foreign keys")
	}

	for _, name := range []string{"idx_account_link_challenges_expiry", "idx_account_link_challenges_initiator_state"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect index %s: %v", name, err)
		}
		if count != 1 {
			t.Fatalf("expected index %s", name)
		}
	}
}

func TestWithTxCommitsAndRollsBack(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() // nolint:errcheck

	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?)`, "committed")
		return err
	})
	if err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	sentinel := errors.New("rollback")
	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?)`, "rolled-back"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected callback error, got %v", err)
	}

	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM account_users WHERE canonical_user_id IN ('committed', 'rolled-back')`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one committed user, got %d", count)
	}
}

func TestOpenEnablesSecureDeleteOnPooledConnections(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // nolint:errcheck
	for i := 0; i < 3; i++ {
		conn, err := db.SQL().Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var enabled int
		err = conn.QueryRowContext(context.Background(), `PRAGMA secure_delete`).Scan(&enabled)
		conn.Close() // nolint:errcheck
		if err != nil || enabled != 1 {
			t.Fatalf("secure_delete=%d err=%v", enabled, err)
		}
	}
}

func TestOpenConfiguresWALAndAllowsTruncateCheckpoint(t *testing.T) {
	db := openTestDB(t)
	var journalMode string
	var autoCheckpoint, autoVacuum int
	if err := db.SQL().QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&autoCheckpoint); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" || autoCheckpoint <= 0 || autoVacuum != 2 {
		t.Fatalf("journal_mode=%q wal_autocheckpoint=%d auto_vacuum=%d", journalMode, autoCheckpoint, autoVacuum)
	}
	var busy, logFrames, checkpointedFrames int
	if err := db.SQL().QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		t.Fatalf("truncate WAL checkpoint: %v", err)
	}
	if busy != 0 {
		t.Fatalf("truncate WAL checkpoint remained busy: log=%d checkpointed=%d", logFrames, checkpointedFrames)
	}
}

func TestOpenDoesNotCreateLegacyDerivedIndexTablesOrTriggers(t *testing.T) {
	db := openTestDB(t)
	for _, name := range []string{"memory_entries_fts", "session_turns_fts"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect table %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("legacy table %s count = %d, want 0", name, count)
		}
	}
	for _, name := range []string{"memory_entries_fts_insert", "memory_entries_fts_update", "memory_entries_fts_delete", "session_turns_fts_insert", "session_turns_fts_update", "session_turns_fts_delete"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("inspect trigger %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("trigger %s count = %d, want 0", name, count)
		}
	}
}

func TestSessionsValidateProfileMemorySources(t *testing.T) {
	db := openTestDB(t)
	insertSessionTestUser(t, db, "user-a")
	insertSessionTestUser(t, db, "user-b")
	memoryA := insertFormationMemory(t, db, "user-a", "session-source-a")
	memoryB := insertFormationMemory(t, db, "user-b", "session-source-b")

	insertSession := func(sessionID, sources string) error {
		_, err := db.SQL().Exec(`
INSERT INTO sessions (
	canonical_user_id, session_id, generation, last_seen_at, expires_at,
	profile_version, profile_version_high_water, renderer_version, source_digest,
	rendered_content, source_memory_ids
) VALUES ('user-a', ?, 1, ?, ?, 1, 1, 'v1', 'digest', 'profile', ?)`,
			sessionID, formationTestTime, "2026-07-19T12:00:00Z", sources)
		return err
	}
	if err := insertSession("empty", `[]`); err != nil {
		t.Fatalf("insert session with empty sources: %v", err)
	}
	if err := insertSession("valid", fmt.Sprintf(`[%d]`, memoryA)); err != nil {
		t.Fatalf("insert session with valid sources: %v", err)
	}

	invalid := map[string]string{
		"cross tenant": fmt.Sprintf(`[%d]`, memoryB),
		"duplicate":    fmt.Sprintf(`[%d,%d]`, memoryA, memoryA),
		"noninteger":   fmt.Sprintf(`[%d,"%d"]`, memoryA, memoryA),
	}
	for name, sources := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := insertSession("invalid-"+name, sources); err == nil {
				t.Fatal("expected invalid session insert to fail")
			}
			if _, err := db.SQL().Exec(`UPDATE sessions SET source_memory_ids = ? WHERE canonical_user_id = 'user-a' AND session_id = 'valid'`, sources); err == nil {
				t.Fatal("expected invalid session update to fail")
			}
		})
	}
	if _, err := db.SQL().Exec(`UPDATE sessions SET canonical_user_id = 'user-b' WHERE canonical_user_id = 'user-a' AND session_id = 'valid'`); err == nil {
		t.Fatal("expected session owner update with cross-tenant source to fail")
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() }) // nolint:errcheck
	return db
}
