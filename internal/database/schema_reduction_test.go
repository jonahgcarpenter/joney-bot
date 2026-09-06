package database

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestV409SupportedPrefixesPreserveDataAndReopen(t *testing.T) {
	registry := orderedMigrations()
	if len(registry) < 10 || registry[9].name != "v4.0.9" {
		t.Fatalf("unexpected migration registry: %+v", registry)
	}
	for prefix := 0; prefix <= 9; prefix++ {
		t.Run(fmt.Sprint(prefix), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prefix.db")
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			db := &DB{path: path, db: raw}
			if prefix > 0 {
				if err := db.runSchemaMigrations(context.Background(), registry[:prefix]); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`
INSERT INTO account_users(canonical_user_id) VALUES ('user');
INSERT INTO sessions(canonical_user_id, session_id, generation, last_seen_at, expires_at,
 profile_version, profile_version_high_water, renderer_version, source_digest, rendered_content,
 fact_count, profile_bytes, source_memory_ids)
VALUES ('user', 'session', 7, '2026-09-01T00:00:00Z', '2027-09-01T00:00:00Z',
 3, 9, 'historical', 'digest', 'frozen content', 0, 999, '[]');
INSERT INTO durable_jobs(id, job_kind, idempotency_key, canonical_user_id, state,
 entity_kind, entity_id, operation, attempt_count, redrive_count, available_at,
 lease_owner, lease_until, last_error_code, updated_at)
VALUES (42, 'derived_index', 'preserve', 'user', 'running', 'memory', 123, 'delete',
 2, 1, '2026-09-01T00:00:00Z', 'exact-lease', '2027-09-01T00:00:00Z', 'retry-code', '2026-09-01T00:00:00Z');`); err != nil {
					t.Fatal(err)
				}
				memoryID := insertFormationMemory(t, db, "user", "preserved source")
				if memoryID != 1 {
					t.Fatalf("unexpected fixture memory ID %d", memoryID)
				}
				if _, err := raw.Exec(`UPDATE sessions SET source_memory_ids = '[1]'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				opened, err := Open(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				for table, columns := range map[string][]string{"sessions": {"fact_count", "profile_bytes"}, "durable_jobs": {"redrive_count"}} {
					for _, column := range columns {
						var count int
						if err := opened.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil || count != 0 {
							t.Fatalf("removed %s.%s count=%d err=%v", table, column, count, err)
						}
					}
				}
				if prefix > 0 {
					var count int
					if err := opened.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE id = 42 AND state = 'running' AND attempt_count = 2 AND lease_owner = 'exact-lease' AND lease_until = '2027-09-01T00:00:00Z' AND last_error_code = 'retry-code' AND entity_id = 123`).Scan(&count); err != nil || count != 1 {
						t.Fatalf("job state lost: count=%d err=%v", count, err)
					}
					if err := opened.SQL().QueryRow(`SELECT COUNT(*) FROM sessions WHERE generation = 7 AND profile_version = 3 AND profile_version_high_water = 9 AND renderer_version = 'historical' AND source_digest = 'digest' AND rendered_content = 'frozen content' AND source_memory_ids = '[1]'`).Scan(&count); err != nil || count != 1 {
						t.Fatalf("frozen profile lost: count=%d err=%v", count, err)
					}
					if _, err := opened.SQL().Exec(`UPDATE sessions SET source_memory_ids = '[1,1]'`); err == nil {
						t.Fatal("duplicate profile sources accepted after migration")
					}
					if _, err := opened.SQL().Exec(`UPDATE sessions SET source_memory_ids = '[999]'`); err == nil {
						t.Fatal("missing profile source accepted after migration")
					}
				}
				if err := opened.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestV409FailureRollsBackDroppedColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	registry := orderedMigrations()
	if err := db.runSchemaMigrations(context.Background(), registry[:9]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
INSERT INTO account_users(canonical_user_id) VALUES ('user');
INSERT INTO sessions(canonical_user_id, session_id, generation, last_seen_at, expires_at,
 profile_version, profile_version_high_water, renderer_version, source_digest,
 rendered_content, fact_count, profile_bytes, source_memory_ids)
VALUES ('user', 'session', 7, '2026-09-01T00:00:00Z', '2027-09-01T00:00:00Z',
 3, 9, 'historical', 'digest', 'frozen', 0, 6, '[]');
INSERT INTO durable_jobs(id, job_kind, idempotency_key, canonical_user_id,
 entity_kind, entity_id, operation, redrive_count, available_at, updated_at)
VALUES (42, 'derived_index', 'preserve', 'user', 'memory', 123, 'delete', 1,
 '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	registry[9].sql += "\ninvalid statement;"
	if err := db.runSchemaMigrations(context.Background(), registry); err == nil || !strings.Contains(err.Error(), "v4.0.9") {
		t.Fatalf("migration failure=%v", err)
	}
	if after := schemaSnapshot(t, raw); before != after {
		t.Fatal("failed reduction changed the prefix schema")
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != 9 {
		t.Fatalf("ledger count=%d err=%v", count, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sessions WHERE generation = 7 AND rendered_content = 'frozen' AND fact_count = 0 AND profile_bytes = 6 AND source_memory_ids = '[]'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback changed session data: count=%d err=%v", count, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE id = 42 AND redrive_count = 1 AND entity_id = 123`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rollback changed job data: count=%d err=%v", count, err)
	}
	if err := db.runSchemaMigrations(context.Background(), orderedMigrations()); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}
