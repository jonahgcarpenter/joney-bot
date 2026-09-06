package database

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

func TestPermanentV400SQLIsExecutedDirectly(t *testing.T) {
	migrations := orderedMigrations()
	migration := migrations[0]
	for _, forbidden := range []string{"ALTER TABLE", "DROP TABLE", "DROP TRIGGER", "data-transform:", "legacy-ledger:"} {
		if strings.Contains(migration.sql, forbidden) {
			t.Fatalf("direct baseline contains forbidden historical operation %q", forbidden)
		}
	}

	direct, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	if _, err := direct.Exec(migration.sql); err != nil {
		t.Fatalf("execute baseline definition: %v", err)
	}
	for _, migration := range migrations[1:] {
		if _, err := direct.Exec(migration.sql); err != nil {
			t.Fatalf("execute migration %s: %v", migration.name, err)
		}
	}
	if _, err := direct.Exec(`
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);`); err != nil {
		t.Fatalf("execute baseline definition: %v", err)
	}

	path := filepath.Join(t.TempDir(), "applied.db")
	appliedStore, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer appliedStore.Close()

	directSchema := schemaSnapshot(t, direct)
	appliedSchema := schemaSnapshot(t, appliedStore.SQL())
	if directSchema != appliedSchema {
		t.Fatal("fresh schema differs from directly executed permanent migration SQL")
	}
}

func TestPermanentV400IsFreshAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	var name, checksum string
	if err := db.SQL().QueryRow(`SELECT version, name, checksum FROM schema_migration_versions`).Scan(&version, &name, &checksum); err != nil {
		t.Fatal(err)
	}
	if version != 1 || name != "v4.0.0" || checksum != migrationChecksum(orderedMigrations()[0]) {
		t.Fatalf("unexpected baseline ledger: %d %q %q", version, name, checksum)
	}
	for _, removed := range []string{"schema_migrations", "memory_confirmation_presentations", "memory_relations", "maintenance_runs", "tenant_profile_versions", "tenant_profile_version_facts", "tenant_profile_version_counters", "tenant_sessions", "tenant_session_generations", "deployment_memory_candidates", "deployment_memory_entries", "deployment_memory_evidence", "global_memory_claims", "global_memory_evidence"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, removed).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("obsolete table %s still exists", removed)
		}
	}
	toolAnnotations := strings.Join([]string{
		toolnames.UserMemorySearch,
		toolnames.UserMemoryList,
		toolnames.GlobalMemorySearch,
		toolnames.SessionTranscriptSearch,
	}, ",")
	if _, err := db.SQL().Exec(`
INSERT INTO account_users(canonical_user_id) VALUES ('restart-user');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, tool_names, created_at)
VALUES ('restart-session', 'restart-user', 'remember this', 'saved', ?, '2026-07-21T00:00:00Z');
INSERT INTO memory_candidates(
	canonical_user_id, idempotency_key, state, scope, category, statement, claim_slot, claim_value,
	provenance_type, formation_mode, created_at, updated_at
) VALUES ('restart-user', 'restart-candidate', 'approved', 'long_term', 'notes', 'Persisted final tool name.',
	'notes.fact', 'persisted final tool name', 'user_statement', 'explicit_remember', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`, toolAnnotations); err != nil {
		t.Fatalf("persist final tool names: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen compact baseline: %v", err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != len(orderedMigrations()) {
		t.Fatalf("baseline reapplied: count=%d err=%v", count, err)
	}
	var persistedAnnotations, toolTrace, toolSearchText, foregroundMemory string
	if err := reopened.SQL().QueryRow(`SELECT tool_names, tool_trace, tool_search_text, foreground_memory FROM session_turns WHERE canonical_user_id = 'restart-user'`).Scan(&persistedAnnotations, &toolTrace, &toolSearchText, &foregroundMemory); err != nil {
		t.Fatal(err)
	}
	if persistedAnnotations != toolAnnotations {
		t.Fatalf("persisted tool names changed across restart: annotations=%q", persistedAnnotations)
	}
	if toolTrace != `{"version":1,"batches":[]}` || toolSearchText != "" {
		t.Fatalf("unexpected legacy tool history defaults trace=%q search=%q", toolTrace, toolSearchText)
	}
	if foregroundMemory != `{"version":2,"candidates":[]}` {
		t.Fatalf("unexpected foreground memory default %q", foregroundMemory)
	}
	if _, err := reopened.SQL().Exec(`UPDATE session_turns SET tool_trace = '{"version":1,"batches":[{}]}' WHERE canonical_user_id = 'restart-user'`); err == nil {
		t.Fatal("mutable session tool history was accepted")
	}
	if _, err := reopened.SQL().Exec(`UPDATE session_turns SET foreground_memory = '{"version":2,"candidates":[{}]}' WHERE canonical_user_id = 'restart-user'`); err == nil {
		t.Fatal("mutable session foreground memory was accepted")
	}
}

func TestPermanentV407ForegroundMemoryChecks(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('user')`); err != nil {
		t.Fatal(err)
	}
	base := `INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, foreground_memory) VALUES ('session', 'user', 'question', 'answer', datetime('now'), ?)`
	for _, invalid := range []string{`not-json`, `[]`, `{"version":1,"candidates":[]}`, `{"version":2,"candidates":[{},{},{},{},{},{}]}`, `{"version":2,"candidates":["` + strings.Repeat("x", 32768) + `"]}`} {
		if _, err := db.SQL().Exec(base, invalid); err == nil {
			t.Fatalf("invalid foreground memory was accepted: %.80q", invalid)
		}
	}
	if _, err := db.SQL().Exec(base, `{"version":2,"candidates":[{},{},{},{},{}]}`); err != nil {
		t.Fatalf("five foreground candidates were rejected: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, canonical_user_id, idempotency_key, source_session_generation, extractor_version, available_at, updated_at) VALUES ('memory_formation', 'user', 'missing-purpose', 1, 'formation-v4', datetime('now'), datetime('now'))`); err == nil {
		t.Fatal("memory formation job without purpose was accepted")
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, canonical_user_id, idempotency_key, entity_kind, entity_id, operation, formation_purpose, available_at, updated_at) VALUES ('derived_index', 'user', 'unexpected-purpose', 'memory', 1, 'delete', 'agent_save', datetime('now'), datetime('now'))`); err == nil {
		t.Fatal("non-formation job with purpose was accepted")
	}
}

func TestPermanentV406UpgradesPrefixWithEmptyForegroundMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	migrations := orderedMigrations()
	if len(migrations) < 8 {
		t.Fatalf("permanent migration count=%d, want at least 8", len(migrations))
	}
	for _, migration := range migrations[:6] {
		if _, err := raw.Exec(migration.sql); err != nil {
			raw.Close() // nolint:errcheck
			t.Fatalf("apply prefix migration %s: %v", migration.name, err)
		}
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migration_versions (version INTEGER PRIMARY KEY CHECK (version > 0), name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL CHECK (length(checksum) = 64), applied_at TEXT NOT NULL)`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err := raw.Exec(`INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (?, ?, ?, datetime('now'))`, migration.version, migration.name, migrationChecksum(migration)); err != nil {
			raw.Close() // nolint:errcheck
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`
INSERT INTO account_users(canonical_user_id) VALUES ('user');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, delivered_at, source_request_id)
VALUES ('session', 'user', 'question', 'answer', datetime('now'), datetime('now'), 'legacy-request');
INSERT INTO durable_jobs(job_kind, canonical_user_id, idempotency_key, source_request_id, source_session_id, source_session_generation, source_turn_id, extractor_version, available_at, updated_at)
VALUES ('memory_formation', 'user', 'turn:1:formation-v4', 'legacy-request', 'session', 1, 1, 'formation-v4', datetime('now'), datetime('now'))`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var trace, searchText, foregroundMemory, purpose string
	if err := db.SQL().QueryRow(`SELECT tool_trace, tool_search_text, foreground_memory FROM session_turns`).Scan(&trace, &searchText, &foregroundMemory); err != nil {
		t.Fatal(err)
	}
	if trace != `{"version":1,"batches":[]}` || searchText != "" {
		t.Fatalf("upgraded defaults trace=%q search=%q", trace, searchText)
	}
	if foregroundMemory != `{"version":2,"candidates":[]}` {
		t.Fatalf("upgraded foreground memory default=%q", foregroundMemory)
	}
	if err := db.SQL().QueryRow(`SELECT formation_purpose FROM durable_jobs WHERE job_kind = 'memory_formation'`).Scan(&purpose); err != nil || purpose != "background_pattern" {
		t.Fatalf("legacy formation purpose=%q err=%v", purpose, err)
	}
}

func TestPermanentV407WidensForegroundMemoryAndNormalizesSubmissionBudgets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	migrations := orderedMigrations()
	if len(migrations) < 8 {
		t.Fatalf("permanent migration count=%d, want at least 8", len(migrations))
	}
	for _, migration := range migrations[:7] {
		if _, err := raw.Exec(migration.sql); err != nil {
			raw.Close() // nolint:errcheck
			t.Fatalf("apply prefix migration %s: %v", migration.name, err)
		}
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migration_versions (version INTEGER PRIMARY KEY CHECK (version > 0), name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL CHECK (length(checksum) = 64), applied_at TEXT NOT NULL)`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	for _, migration := range migrations[:7] {
		if _, err := raw.Exec(`INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (?, ?, ?, datetime('now'))`, migration.version, migration.name, migrationChecksum(migration)); err != nil {
			raw.Close() // nolint:errcheck
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`
INSERT INTO account_users(canonical_user_id) VALUES ('user');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, session_generation, delivered_at, source_request_id, foreground_memory)
VALUES ('session', 'user', 'one', 'answer', '2026-09-05T00:00:00Z', 1, '2026-09-05T00:00:01Z', 'request-1', '{"version":2,"candidates":[{},{}]}');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, session_generation, delivered_at, source_request_id)
VALUES ('session', 'user', 'two', 'answer', '2026-09-05T00:00:02Z', 1, '2026-09-05T00:00:03Z', 'request-2');
INSERT INTO durable_jobs(
	job_kind, idempotency_key, canonical_user_id, state, source_request_id, source_session_id,
	source_session_generation, source_turn_id, extractor_version, formation_purpose, artifact_payload,
	attempt_count, invalid_output_retry_count, available_at, last_error_code, updated_at
) VALUES (
	'memory_formation', 'formation-capped', 'user', 'retry', 'request-2', 'session',
	1, 2, 'pattern-v1', 'background_pattern', '{"version":1,"turn_ids":[1,2]}',
	2, 1, '2026-09-05T00:00:00Z', 'invalid_pattern_evidence', '2026-09-05T00:00:00Z'
);
INSERT INTO durable_jobs(
	job_kind, idempotency_key, canonical_user_id, state, source_request_id, source_session_id,
	source_session_generation, source_turn_id, extractor_version, formation_purpose, artifact_payload,
	attempt_count, invalid_output_retry_count, available_at, last_error_code, updated_at
) VALUES (
	'memory_formation', 'formation-corrective', 'user', 'retry', 'request-2', 'session',
	1, 2, 'pattern-v1', 'background_pattern', '{"version":1,"turn_ids":[1,2]}',
	1, 1, '2026-09-05T00:00:00Z', 'invalid_pattern_source', '2026-09-05T00:00:00Z'
);
INSERT INTO durable_jobs(
	job_kind, idempotency_key, canonical_user_id, state, source_request_id, source_session_id,
	source_session_generation, source_turn_id, extractor_version, formation_purpose,
	attempt_count, available_at, updated_at
) VALUES (
	'memory_formation', 'agent-save', 'user', 'retry', 'request-2', 'session',
	1, 2, 'agent-save-v1', 'agent_save', 5, '2026-09-05T00:00:00Z', '2026-09-05T00:00:00Z'
);
INSERT INTO durable_jobs(
	job_kind, idempotency_key, canonical_user_id, state, session_id, session_generation,
	covered_from_turn_id, covered_through_turn_id, compaction_target_turn_id,
	compaction_model, compaction_generator_version, attempt_count, redrive_count,
	compaction_invalid_output_retry_count, available_at, last_error_code, updated_at
) VALUES (
	'session_compaction', 'compaction-capped', 'user', 'retry', 'session', 1,
	1, 2, 2, 'model', 'session-summary-v2', 0, 1, 0,
	'2026-09-05T00:00:00Z', 'invalid_summary_shape', '2026-09-05T00:00:00Z'
);`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("apply v4.0.7: %v", err)
	}
	defer db.Close()

	var foreground string
	if err := db.SQL().QueryRow(`SELECT foreground_memory FROM session_turns WHERE id = 1`).Scan(&foreground); err != nil || foreground != `{"version":2,"candidates":[{},{}]}` {
		t.Fatalf("preserved foreground memory=%q err=%v", foreground, err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, foreground_memory) VALUES ('new', 'user', 'question', 'answer', datetime('now'), '{"version":2,"candidates":[{},{},{},{},{}]}')`); err != nil {
		t.Fatalf("five foreground candidates rejected after migration: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, foreground_memory) VALUES ('invalid', 'user', 'question', 'answer', datetime('now'), '{"version":2,"candidates":[{},{},{},{},{},{}]}')`); err == nil {
		t.Fatal("six foreground candidates were accepted after migration")
	}

	for key, wantState := range map[string]string{"formation-capped": "dead", "compaction-capped": "dead", "formation-corrective": "retry", "agent-save": "retry"} {
		var state string
		var submissions int
		if err := db.SQL().QueryRow(`SELECT state, model_submission_count FROM durable_jobs WHERE idempotency_key = ?`, key).Scan(&state, &submissions); err != nil {
			t.Fatal(err)
		}
		wantSubmissions := 3
		if key == "formation-corrective" {
			wantSubmissions = 2
		} else if key == "agent-save" {
			wantSubmissions = 0
		}
		if state != wantState || submissions != wantSubmissions {
			t.Fatalf("job %s state=%q submissions=%d, want %q/%d", key, state, submissions, wantState, wantSubmissions)
		}
	}
	var correctiveCode string
	if err := db.SQL().QueryRow(`SELECT corrective_error_code FROM durable_jobs WHERE idempotency_key = 'formation-corrective'`).Scan(&correctiveCode); err != nil || correctiveCode != "invalid_pattern_source" {
		t.Fatalf("corrective code=%q err=%v", correctiveCode, err)
	}
	var artifact string
	if err := db.SQL().QueryRow(`SELECT artifact_payload FROM durable_jobs WHERE idempotency_key = 'formation-corrective'`).Scan(&artifact); err != nil || artifact != `{"version":1,"turn_ids":[1,2]}` {
		t.Fatalf("preserved formation artifact=%q err=%v", artifact, err)
	}
	var redriveColumns int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('durable_jobs') WHERE name = 'redrive_count'`).Scan(&redriveColumns); err != nil || redriveColumns != 0 {
		t.Fatalf("v4.0.7 submission reconstruction must survive v4.0.9 counter removal: columns=%d err=%v", redriveColumns, err)
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET model_submission_count = 1 WHERE idempotency_key = 'agent-save'`); err == nil {
		t.Fatal("agent-save job accepted model submission credit")
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET model_submission_count = 3 WHERE idempotency_key = 'formation-corrective'`); err == nil {
		t.Fatal("retry job accepted an exhausted model submission budget")
	}
}

func TestPermanentV401AddsEmptyMCPDescriptionForBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	migration := orderedMigrations()[0]
	if _, err := raw.Exec(migration.sql + `
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);
INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (1, 'v4.0.0', '` + migrationChecksum(migration) + `', datetime('now'));
INSERT INTO mcp_servers(id, scope, owner_user_id, name, transport, url_ciphertext, headers_ciphertext, enabled)
VALUES ('mcp-1', 'global', NULL, 'github', 'streamable_http', 'ciphertext', '', 1);`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("apply v4.0.1: %v", err)
	}
	defer db.Close()
	var description string
	if err := db.SQL().QueryRow(`SELECT description FROM mcp_servers WHERE id = 'mcp-1'`).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description != "" {
		t.Fatalf("migrated description = %q, want empty backfill marker", description)
	}
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != len(orderedMigrations()) {
		t.Fatalf("migration ledger count=%d err=%v", count, err)
	}
}

func TestPermanentV402AddsDedicatedFormationInvalidOutputRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	migrations := orderedMigrations()
	if len(migrations) < 3 {
		t.Fatalf("permanent migration count=%d, want at least 3", len(migrations))
	}
	if _, err := raw.Exec(migrations[0].sql + migrations[1].sql + `
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);
INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (1, 'v4.0.0', '` + migrationChecksum(migrations[0]) + `', datetime('now'));
INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (2, 'v4.0.1', '` + migrationChecksum(migrations[1]) + `', datetime('now'));
INSERT INTO account_users(canonical_user_id) VALUES ('user');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, session_generation, delivered_at, source_request_id)
VALUES ('session', 'user', 'remember this', 'noted', '2026-07-21T00:00:00Z', 1, '2026-07-21T00:00:01Z', 'request');
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, source_request_id, source_session_id, source_session_generation, source_turn_id, extractor_version, available_at, updated_at)
VALUES ('memory_formation', 'formation', 'user', 'request', 'session', 1, last_insert_rowid(), 'v1', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z');
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
VALUES ('derived_index', 'index', 'user', 'memory', 1, 'upsert', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z');`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("apply v4.0.2: %v", err)
	}
	defer db.Close()
	var version, formationCount, indexCount int
	var name string
	if err := db.SQL().QueryRow(`SELECT version, name FROM schema_migration_versions ORDER BY version DESC LIMIT 1`).Scan(&version, &name); err != nil {
		t.Fatal(err)
	}
	if version != len(orderedMigrations()) || name != orderedMigrations()[len(orderedMigrations())-1].name {
		t.Fatalf("latest migration=%d/%q", version, name)
	}
	if err := db.SQL().QueryRow(`SELECT invalid_output_retry_count FROM durable_jobs WHERE idempotency_key = 'formation'`).Scan(&formationCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT invalid_output_retry_count FROM durable_jobs WHERE idempotency_key = 'index'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if formationCount != 0 || indexCount != 0 {
		t.Fatalf("migration defaults formation=%d index=%d", formationCount, indexCount)
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET invalid_output_retry_count = 1 WHERE idempotency_key = 'formation'`); err != nil {
		t.Fatalf("formation retry count 1 rejected: %v", err)
	}
	for name, statement := range map[string]string{
		"formation above one": `UPDATE durable_jobs SET invalid_output_retry_count = 2 WHERE idempotency_key = 'formation'`,
		"negative formation":  `UPDATE durable_jobs SET invalid_output_retry_count = -1 WHERE idempotency_key = 'formation'`,
		"non-formation retry": `UPDATE durable_jobs SET invalid_output_retry_count = 1 WHERE idempotency_key = 'index'`,
	} {
		if _, err := db.SQL().Exec(statement); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestPermanentV403AndV404HardenSessionCompactionContracts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	migrations := orderedMigrations()
	if len(migrations) < 4 {
		t.Fatalf("permanent migration count=%d, want at least 4", len(migrations))
	}
	if _, err := raw.Exec(migrations[0].sql + migrations[1].sql + migrations[2].sql + `
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);
INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (1, 'v4.0.0', '` + migrationChecksum(migrations[0]) + `', datetime('now'));
INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (2, 'v4.0.1', '` + migrationChecksum(migrations[1]) + `', datetime('now'));
INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (3, 'v4.0.2', '` + migrationChecksum(migrations[2]) + `', datetime('now'));
INSERT INTO account_users(canonical_user_id) VALUES ('user');
INSERT INTO sessions(canonical_user_id, session_id, generation, is_active, last_seen_at, expires_at, profile_version, profile_version_high_water, renderer_version, source_digest, rendered_content, fact_count, profile_bytes)
VALUES ('user', 'session', 1, 1, '2026-08-23T00:00:00Z', '2027-08-23T00:00:00Z', 1, 1, 'v1', 'digest', 'profile', 0, 7);
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, session_generation, delivered_at)
VALUES ('session', 'user', 'one', 'answer', '2026-08-23T00:00:00Z', 1, '2026-08-23T00:00:01Z');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at, session_generation, delivered_at)
VALUES ('session', 'user', 'two', 'answer', '2026-08-23T00:00:02Z', 1, '2026-08-23T00:00:03Z');
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id, available_at, updated_at)
VALUES ('session_compaction', 'legacy', 'user', 'session', 1, 1, 2, '2026-08-23T00:00:00Z', '2026-08-23T00:00:00Z');`); err != nil {
		raw.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("apply v4.0.3 and v4.0.4: %v", err)
	}
	defer db.Close()
	var state, model, generator, code string
	if err := db.SQL().QueryRow(`SELECT state, compaction_model, compaction_generator_version, last_error_code FROM durable_jobs WHERE idempotency_key = 'legacy'`).Scan(&state, &model, &generator, &code); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || model != "legacy-unknown" || generator != "legacy-unknown" || code != "legacy_compaction_contract" {
		t.Fatalf("legacy state=%q model=%q generator=%q code=%q", state, model, generator, code)
	}
	var target int64
	if err := db.SQL().QueryRow(`SELECT compaction_target_turn_id FROM durable_jobs WHERE idempotency_key = 'legacy'`).Scan(&target); err != nil || target != 2 {
		t.Fatalf("legacy campaign target=%d err=%v", target, err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id, available_at, updated_at) VALUES ('session_compaction', 'missing-contract', 'user', 'session', 1, 1, 2, '2026-08-23T00:00:00Z', '2026-08-23T00:00:00Z')`); err == nil {
		t.Fatal("compaction job without contract was accepted")
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id, compaction_target_turn_id, compaction_model, compaction_generator_version, available_at, updated_at) VALUES ('session_compaction', 'current', 'user', 'session', 1, 1, 2, 2, 'model', 'session-summary-v1', '2026-08-23T00:00:00Z', '2026-08-23T00:00:00Z')`); err != nil {
		t.Fatalf("current compaction contract rejected: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id, compaction_target_turn_id, compaction_model, compaction_generator_version, available_at, updated_at) VALUES ('session_compaction', 'overlap', 'user', 'session', 1, 1, 2, 2, 'other-model', 'session-summary-v1', '2026-08-23T00:00:00Z', '2026-08-23T00:00:00Z')`); err == nil {
		t.Fatal("second active compaction contract was accepted")
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET compaction_invalid_output_retry_count = 1 WHERE idempotency_key = 'current'`); err != nil {
		t.Fatalf("dedicated compaction retry rejected: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET compaction_invalid_output_retry_count = 2 WHERE idempotency_key = 'current'`); err != nil {
		t.Fatalf("second dedicated compaction retry rejected: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET compaction_invalid_output_retry_count = 3 WHERE idempotency_key = 'current'`); err != nil {
		t.Fatalf("third dedicated compaction retry rejected: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET compaction_invalid_output_retry_count = 4 WHERE idempotency_key = 'current'`); err == nil {
		t.Fatal("fourth dedicated compaction retry was accepted")
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET compaction_model = 'changed' WHERE idempotency_key = 'current'`); err == nil {
		t.Fatal("compaction contract mutation was accepted")
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET compaction_target_turn_id = 3 WHERE idempotency_key = 'current'`); err == nil {
		t.Fatal("compaction campaign target mutation was accepted")
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET job_kind = 'memory_formation' WHERE idempotency_key = 'current'`); err == nil {
		t.Fatal("compaction job kind transition was accepted")
	}
}

func TestPermanentV400CanonicalTableInventory(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expected := []string{
		"account_link_challenges", "account_users", "derived_index_revisions", "durable_jobs",
		"global_memories", "linked_accounts", "mcp_servers", "memory_candidates",
		"memory_entries", "schema_migration_versions", "session_summaries",
		"session_turns", "sessions",
	}
	rows, err := db.SQL().Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, name)
	}
	if strings.Join(actual, ",") != strings.Join(expected, ",") {
		t.Fatalf("canonical table inventory changed:\nactual: %v\nexpected: %v", actual, expected)
	}
}

func TestPermanentV400CanonicalObjectInventory(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for objectType, want := range map[string]int{"table": 12, "index": 27, "trigger": 29, "view": 0} {
		var got int
		if err := db.SQL().QueryRow(`
SELECT COUNT(*) FROM sqlite_master
WHERE type = ? AND name NOT LIKE 'sqlite_%'
	AND name != 'schema_migration_versions'`, objectType).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("canonical %s count=%d, want %d", objectType, got, want)
		}
	}
}

func TestPermanentMigrationsRejectNonemptyLedgerlessSchemaWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsupported.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE existing (id INTEGER PRIMARY KEY, value TEXT NOT NULL); INSERT INTO existing VALUES (1, 'preserve me'); CREATE VIEW existing_view AS SELECT value FROM existing;`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if db, err := Open(path, nil); err == nil {
			db.Close()
			t.Fatal("expected nonempty ledgerless database rejection")
		} else if !strings.Contains(err.Error(), "unsupported nonempty database without a permanent migration ledger") {
			t.Fatalf("unexpected rejection: %v", err)
		}
		var count int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Fatalf("rejected database schema changed: %d objects", count)
		}
		var value string
		if err := raw.QueryRow(`SELECT value FROM existing_view`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != "preserve me" {
			t.Fatalf("rejected database data changed: %q", value)
		}
	}
}

func TestPermanentV400RejectsDevelopmentLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE schema_migration_versions (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL); INSERT INTO schema_migration_versions VALUES (1, 'legacy_core_schema', ?, datetime('now'))`, strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(path, nil); err == nil {
		db.Close()
		t.Fatal("expected development migration ledger rejection")
	} else if !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestPermanentV4RejectsOldCompactBaselineLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-v4.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migration_versions (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL); INSERT INTO schema_migration_versions VALUES (1, 'v4_compact_baseline', ?, datetime('now'))`, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	raw.Close() // nolint:errcheck
	if db, err := Open(path, nil); err == nil {
		db.Close() // nolint:errcheck
		t.Fatal("expected old compact baseline ledger rejection")
	} else if !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestPermanentV400RejectsEmptyLedgerWithOtherObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
);
CREATE TABLE unrelated (id INTEGER PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	if db, err := Open(path, nil); err == nil {
		db.Close()
		t.Fatal("expected empty migration ledger with other objects to be rejected")
	}
}

func TestSchemaMigrationApplyFailureRollsBackEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	registry := []schemaMigration{{
		version: 1,
		name:    "v4.0.0",
		sql:     "CREATE TABLE rollback_probe (id INTEGER PRIMARY KEY); invalid statement;",
		major:   4,
	}}
	if err := db.runSchemaMigrations(context.Background(), registry); err == nil {
		t.Fatal("expected schema migration failure")
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed baseline left %d schema objects", count)
	}
}

func TestMigrationForeignKeyCheckFailureRollsBackMissingMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign-key-rollback.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Keep the runner and enforcement assertions on the same physical connection.
	raw.SetMaxOpenConns(1)
	db := &DB{path: path, db: raw}
	base := []schemaMigration{{version: 1, name: "v4.0.0", major: 4, sql: `
CREATE TABLE parent (id INTEGER PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE child (parent_id INTEGER REFERENCES parent(id));
INSERT INTO parent VALUES (1, 'preserved');`}}
	if err := db.runSchemaMigrations(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	registry := append(base,
		schemaMigration{version: 2, name: "v4.0.1", major: 4, patch: 1, sql: `CREATE TABLE rollback_probe (id INTEGER); UPDATE parent SET value = 'changed';`},
		schemaMigration{version: 3, name: "v4.0.2", major: 4, patch: 2, sql: `INSERT INTO child VALUES (999);`},
	)
	if err := db.runSchemaMigrations(context.Background(), registry); err == nil || !strings.Contains(err.Error(), "schema migration foreign key violation") {
		t.Fatalf("expected post-migration foreign-key check failure, got %v", err)
	}
	if after := schemaSnapshot(t, raw); after != before {
		t.Fatal("foreign-key check failure changed schema")
	}
	var ledgerRows, children, enabled int
	var value, checksum string
	if err := raw.QueryRow(`SELECT (SELECT COUNT(*) FROM schema_migration_versions), (SELECT COUNT(*) FROM child), value FROM parent WHERE id = 1`).Scan(&ledgerRows, &children, &value); err != nil || ledgerRows != 1 || children != 0 || value != "preserved" {
		t.Fatalf("rollback ledger=%d children=%d value=%q err=%v", ledgerRows, children, value, err)
	}
	if err := raw.QueryRow(`SELECT checksum FROM schema_migration_versions WHERE version = 1`).Scan(&checksum); err != nil || checksum != migrationChecksum(base[0]) {
		t.Fatalf("prefix checksum=%q err=%v", checksum, err)
	}
	if err := raw.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign-key enforcement=%d err=%v", enabled, err)
	}
	if _, err := raw.Exec(`INSERT INTO child VALUES (999)`); err == nil {
		t.Fatal("foreign-key enforcement was not restored")
	}
	if _, err := raw.Exec(`INSERT INTO child VALUES (1)`); err != nil {
		t.Fatalf("valid write after rollback: %v", err)
	}
}

func TestPermanentMigrationRegistryValidation(t *testing.T) {
	files := fstest.MapFS{
		"migrations/v4.1.0.sql": &fstest.MapFile{Data: []byte("CREATE TABLE second (id INTEGER);")},
		"migrations/v4.0.0.sql": &fstest.MapFile{Data: []byte("CREATE TABLE first (id INTEGER);")},
	}
	registry, err := discoverPermanentMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry) != 2 || registry[0].version != 1 || registry[0].name != "v4.0.0" || registry[1].version != 2 || registry[1].name != "v4.1.0" {
		t.Fatalf("unexpected semantic registry order: %+v", registry)
	}
	for name, files := range map[string]fstest.MapFS{
		"bad filename": {"migrations/004.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}},
		"empty SQL":    {"migrations/v4.0.0.sql": &fstest.MapFile{Data: []byte(" \n")}},
		"wrong start":  {"migrations/v4.0.1.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := discoverPermanentMigrations(files); err == nil {
				t.Fatal("expected invalid permanent migration registry")
			}
		})
	}
}

func TestMigrationChecksumCoversReleaseNameAndSQL(t *testing.T) {
	migration := orderedMigrations()[0]
	changedName := migration
	changedName.name = "v4.0.1"
	changedSQL := migration
	changedSQL.sql += "\n-- changed"
	if migrationChecksum(migration) == migrationChecksum(changedName) || migrationChecksum(migration) == migrationChecksum(changedSQL) {
		t.Fatal("migration checksum does not cover release name and SQL content")
	}
}

func TestPermanentMigrationRunnerAppliesMissingRegisteredFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix.db")
	opened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close() // nolint:errcheck
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	base := orderedMigrations()
	registry := append(base, schemaMigration{
		version: len(base) + 1,
		name:    "v4.1.0",
		sql:     "CREATE TABLE migration_two_probe (id INTEGER PRIMARY KEY);",
		major:   4,
		minor:   1,
	})
	if err := db.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatal(err)
	}
	var rows, probe int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_two_probe'`).Scan(&probe); err != nil {
		t.Fatal(err)
	}
	if rows != len(registry) || probe != 1 {
		t.Fatalf("missing migration not applied: ledger=%d probe=%d", rows, probe)
	}
}

func TestPermanentMigrationRejectsChecksumDriftWithoutChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // nolint:errcheck
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE schema_migration_versions SET checksum = ? WHERE version = 1`, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	raw.Close() // nolint:errcheck
	if reopened, err := Open(path, nil); err == nil {
		reopened.Close() // nolint:errcheck
		t.Fatal("expected checksum drift rejection")
	} else if !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, err = sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if after := schemaSnapshot(t, raw); after != before {
		t.Fatal("checksum drift rejection changed schema")
	}
}

func TestPermanentMigrationRejectsMalformedLedgerPrefixes(t *testing.T) {
	unknownVersion := len(orderedMigrations()) + 1
	for _, test := range []struct {
		name   string
		mutate string
	}{
		{name: "gap", mutate: `UPDATE schema_migration_versions SET version = ` + fmt.Sprint(unknownVersion) + ` WHERE version = 1`},
		{name: "unknown trailing version", mutate: `INSERT INTO schema_migration_versions(version, name, checksum, applied_at) VALUES (` + fmt.Sprint(unknownVersion) + `, 'v9.0.0', '` + strings.Repeat("f", 64) + `', datetime('now'))`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "malformed.db")
			db, err := Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			db.Close() // nolint:errcheck
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(test.mutate); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, raw)
			raw.Close() // nolint:errcheck
			if reopened, err := Open(path, nil); err == nil {
				reopened.Close() // nolint:errcheck
				t.Fatal("expected malformed ledger rejection")
			}
			raw, err = sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if after := schemaSnapshot(t, raw); after != before {
				t.Fatal("malformed ledger rejection changed schema")
			}
		})
	}
}

func TestPermanentMigrationFailureRollsBackMissingPrefixOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix-rollback.db")
	opened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close() // nolint:errcheck
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	base := orderedMigrations()
	registry := append(base, schemaMigration{
		version: len(base) + 1,
		name:    "v4.1.0",
		sql:     "CREATE TABLE migration_two_probe (id INTEGER PRIMARY KEY); invalid statement;",
		major:   4,
		minor:   1,
	})
	if err := db.runSchemaMigrations(context.Background(), registry); err == nil {
		t.Fatal("expected missing-prefix migration failure")
	}
	var ledgerRows, probe int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'migration_two_probe'`).Scan(&probe); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != len(base) || probe != 0 {
		t.Fatalf("failed prefix migration changed database: ledger=%d probe=%d", ledgerRows, probe)
	}
}

func TestPermanentV400ConcurrentOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	const openCount = 8
	dbs := make([]*DB, openCount)
	errs := make([]error, openCount)
	var wg sync.WaitGroup
	for i := range dbs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbs[i], errs[i] = Open(path, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent open %d: %v", i, err)
		}
		defer dbs[i].Close()
	}
	var count int
	if err := dbs[0].SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(orderedMigrations()) {
		t.Fatalf("migration ledger row count=%d, want %d", count, len(orderedMigrations()))
	}
}

func TestPermanentV400ForeignKeysAreValid(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.SQL().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("fresh compact baseline has a foreign-key violation")
	}
}

func TestGlobalMemoryCanonicalConstraints(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('', 'empty', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("expected empty global memory to fail")
	}
	if _, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES (?, 'long', '2026-07-21T00:00:00Z')`, strings.Repeat("x", 1001)); err == nil {
		t.Fatal("expected overlong global memory to fail")
	}
	result, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('Oswald uses Go.', 'oswald uses go.', '2026-07-21T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert valid global memory: %v", err)
	}
	memoryID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('Duplicate key.', 'oswald uses go.', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("expected duplicate normalized key to fail")
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at) VALUES ('derived_index', 'global-upsert', NULL, 'global_memory', ?, 'upsert', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`, memoryID); err != nil {
		t.Fatalf("insert global-memory index job: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at) VALUES ('derived_index', 'bad-global-upsert', 'missing-user', 'global_memory', ?, 'upsert', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`, memoryID); err == nil {
		t.Fatal("expected tenant-owned global-memory index job to fail")
	}
}

func TestDurableJobRunningLeaseConstraints(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('user')`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, state, source_session_generation, extractor_version, available_at, updated_at) VALUES ('memory_formation', 'formation', 'user', 'running', 1, 'v1', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`,
		`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, state, entity_kind, entity_id, operation, available_at, updated_at) VALUES ('derived_index', 'index', 'user', 'running', 'memory', 1, 'upsert', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`,
	} {
		if _, err := db.SQL().Exec(statement); err == nil {
			t.Fatalf("running durable job without lease was accepted: %s", statement)
		}
	}
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, lease_owner, lease_until, available_at, updated_at) VALUES ('derived_index', 'queued-owned', 'user', 'memory', 1, 'upsert', 'owner', '2026-07-21T01:00:00Z', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("non-running durable job with lease ownership was accepted")
	}
}

func TestIndexTableIdentityConstraints(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO derived_index_revisions(index_kind, schema_version, revision, table_name, state, created_at, updated_at) VALUES ('memory_fts', 1, 1, 'unique_table', 'failed', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO derived_index_revisions(index_kind, schema_version, revision, table_name, state, created_at, updated_at) VALUES ('transcript_fts', 1, 1, 'unique_table', 'failed', '2026-07-21T00:00:00Z', '2026-07-21T00:00:00Z')`); err == nil {
		t.Fatal("duplicate derived-index table identity was accepted")
	}
}

func schemaSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`
SELECT type, name, tbl_name, sql
FROM sqlite_master
WHERE name NOT LIKE 'sqlite_%'
ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot strings.Builder
	for rows.Next() {
		var objectType, name, table, definition string
		if err := rows.Scan(&objectType, &name, &table, &definition); err != nil {
			t.Fatal(err)
		}
		snapshot.WriteString(objectType)
		snapshot.WriteByte('|')
		snapshot.WriteString(name)
		snapshot.WriteByte('|')
		snapshot.WriteString(table)
		snapshot.WriteByte('|')
		snapshot.WriteString(definition)
		snapshot.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot.String()
}
