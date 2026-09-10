package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAssessmentMigrationPreservesArtifactsHighWaterAndRollback(t *testing.T) {
	raw, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "assessment.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{db: raw}
	registry := orderedMigrations()
	if len(registry) != 14 || registry[13].name != "v4.0.13" {
		t.Fatalf("registry: %v", registry)
	}
	if err := db.runSchemaMigrations(context.Background(), registry[:11]); err != nil {
		t.Fatal(err)
	}
	artifact := `{"version":2,"candidates":[]}`
	if _, err := raw.Exec(`INSERT INTO account_users(canonical_user_id) VALUES('user'); INSERT INTO session_turns(id,canonical_user_id,session_id,user_text,assistant_text,created_at,foreground_memory) VALUES(1,'user','session','text','answer','2026-09-01',?); INSERT INTO session_turns(id,canonical_user_id,session_id,user_text,assistant_text,created_at) VALUES(100,'user','session','text','answer','2026-09-01'); DELETE FROM session_turns WHERE id=100`, artifact); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	broken := orderedMigrations()
	broken[11].sql += "\ninvalid statement;"
	if err := db.runSchemaMigrations(context.Background(), broken); err == nil {
		t.Fatal("invalid migration committed")
	}
	if after := schemaSnapshot(t, raw); before != after {
		t.Fatal("failed migration changed schema")
	}
	if err := db.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := raw.QueryRow(`SELECT foreground_memory FROM session_turns WHERE id=1`).Scan(&stored); err != nil || stored != artifact {
		t.Fatalf("changed legacy bytes: %q %v", stored, err)
	}
	result, err := raw.Exec(`INSERT INTO session_turns(canonical_user_id,session_id,user_text,assistant_text,created_at) VALUES('user','session','text','answer','2026-09-01')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil || id <= 100 {
		t.Fatalf("reused ID: %d %v", id, err)
	}
	for _, bad := range []string{`{"version":4,"candidates":[]}`, `{"version":3,"candidates":{}}`, `not json`} {
		if _, err := raw.Exec(`INSERT INTO session_turns(canonical_user_id,session_id,user_text,assistant_text,created_at,foreground_memory) VALUES('user','session','text','answer','2026-09-01',?)`, bad); err == nil {
			t.Fatal("invalid artifact admitted")
		}
	}
	if _, err := raw.Exec(`UPDATE session_turns SET foreground_memory='{"version":3,"candidates":[]}' WHERE id=1`); err == nil {
		t.Fatal("legacy artifact mutable")
	}
	rows, err := raw.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
}
