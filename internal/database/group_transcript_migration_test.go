package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestGroupTranscriptMigrationPreservesUnsharedLegacyAndConstraints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "group.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	registry := orderedMigrations()
	if len(registry) != 11 || registry[10].name != "v4.0.10" {
		t.Fatalf("unexpected registry: %+v", registry)
	}
	if err := db.runSchemaMigrations(context.Background(), registry[:10]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('user');
INSERT INTO session_turns(canonical_user_id, session_id, user_text, assistant_text, created_at)
VALUES ('user', 'discord:group:private-identifier', 'legacy prompt', 'legacy answer', '2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	broken := orderedMigrations()
	broken[10].sql += "\ninvalid statement;"
	if err := db.runSchemaMigrations(context.Background(), broken); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	if after := schemaSnapshot(t, raw); before != after {
		t.Fatal("failed migration changed schema")
	}
	if err := db.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM session_turns WHERE group_gateway = '' AND group_chat_id = '' AND public_user_text = '' AND user_text = 'legacy prompt'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy projection count=%d err=%v", count, err)
	}
	for _, scope := range [][3]string{{"discord", "", "public"}, {"", "chat", "public"}, {"homeassistant", "chat", "public"}, {"", "", "public"}, {"discord", " chat", "public"}} {
		if _, err := raw.Exec(`INSERT INTO session_turns(canonical_user_id, session_id, user_text, assistant_text, created_at, group_gateway, group_chat_id, public_user_text) VALUES ('user', 'session', 'private', 'answer', '2026-09-01T00:00:00Z', ?, ?, ?)`, scope[0], scope[1], scope[2]); err == nil {
			t.Fatalf("invalid scope accepted: %q", scope)
		}
	}
	for _, gateway := range []string{"discord", "imessage"} {
		if _, err := raw.Exec(`INSERT INTO session_turns(canonical_user_id, session_id, user_text, assistant_text, created_at, group_gateway, group_chat_id, public_user_text) VALUES ('user', 'session', 'private', 'answer', '2026-09-01T00:00:00Z', ?, 'chat', '')`, gateway); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []string{
		`UPDATE session_turns SET group_gateway = 'discord', group_chat_id = 'chat', public_user_text = user_text WHERE group_gateway = ''`,
		`UPDATE session_turns SET group_chat_id = 'other' WHERE group_gateway = 'discord'`,
		`UPDATE session_turns SET group_gateway = 'imessage' WHERE group_gateway = 'discord'`,
		`UPDATE session_turns SET public_user_text = 'changed' WHERE group_gateway = 'discord'`,
		`UPDATE session_turns SET assistant_text = 'changed' WHERE group_gateway = 'discord'`,
	} {
		if _, err := raw.Exec(mutation); err == nil {
			t.Fatalf("immutable mutation accepted: %s", mutation)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := opened.SQL().QueryRow(`SELECT COUNT(*) FROM session_turns`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("reopened turns=%d err=%v", count, err)
	}
}
