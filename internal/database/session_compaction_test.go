package database

import (
	"fmt"
	"strings"
	"testing"
)

func TestSessionTurnsPendingDeliveryIndexIsPartial(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	var definition string
	if err := db.SQL().QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_session_turns_pending_delivery'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(definition, "delivered_at IS NULL AND delivery_failed_at IS NULL") {
		t.Fatalf("pending delivery index is not partial: %s", definition)
	}
}

func TestSessionCompactionFreshSchemaAndIdempotency(t *testing.T) {
	db := openTestDB(t)
	for _, table := range []string{"session_summaries", "durable_jobs"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count=%d err=%v", table, count, err)
		}
	}
	var deliveredColumn int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('session_turns') WHERE name = 'delivered_at'`).Scan(&deliveredColumn); err != nil || deliveredColumn != 1 {
		t.Fatalf("delivered_at column count=%d err=%v", deliveredColumn, err)
	}
	var deliveryFailedColumn int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('session_turns') WHERE name = 'delivery_failed_at'`).Scan(&deliveryFailedColumn); err != nil || deliveryFailedColumn != 1 {
		t.Fatalf("delivery_failed_at column count=%d err=%v", deliveryFailedColumn, err)
	}
	for _, column := range []string{"compaction_pressure_tokens", "compaction_pressure_limit", "compaction_pressure_version"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('session_turns') WHERE name = ?`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("session pressure column %s count=%d err=%v", column, count, err)
		}
	}
	for table, columns := range map[string][]string{
		"session_turns":     {"importance"},
		"session_summaries": {"generation_model", "generator_version", "source_digest", "created_at", "expires_at"},
		"sessions":          {"started_at", "profile_created_at"},
		"durable_jobs":      {"generation_model", "generator_version", "started_at", "created_at"},
	} {
		for _, column := range columns {
			var count int
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil || count != 0 {
				t.Fatalf("removed column %s.%s count=%d err=%v", table, column, count, err)
			}
		}
	}
	for _, column := range []string{"compaction_model", "compaction_generator_version", "compaction_invalid_output_retry_count", "compaction_target_turn_id"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('durable_jobs') WHERE name = ?`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("compaction column %s count=%d err=%v", column, count, err)
		}
	}

	insertSessionTestUser(t, db, "user-a")
	fromID := insertSessionTestTurn(t, db, "user-a", "session-a", 1, "first telescope note", "first answer")
	throughID := insertSessionTestTurn(t, db, "user-a", "session-a", 1, "second telescope note", "second answer")
	summaryID := insertSessionTestSummary(t, db, "user-a", "session-a", 1, fromID, throughID)
	if _, err := db.SQL().Exec(`UPDATE session_summaries SET source_turn_ids = json_array(?, ?) WHERE id = ?`, fromID, throughID, summaryID); err == nil {
		t.Fatal("expected immutable summary source update to fail")
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	compaction_target_turn_id, artifact_summary_id, artifact_payload, compaction_model, compaction_generator_version, available_at, updated_at
	) VALUES ('session_compaction', 'job-a', 'user-a', 'session-a', 1, ?, ?, ?, ?, '{"generation_model":"model","generator_version":"v1"}', 'model', 'v1', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, fromID, throughID, throughID, summaryID); err != nil {
		t.Fatalf("insert compaction job: %v", err)
	}

	var summaryCount, jobCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM session_summaries`).Scan(&summaryCount); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction'`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if summaryCount != 1 || jobCount != 1 {
		t.Fatalf("baseline changed records: summaries=%d jobs=%d", summaryCount, jobCount)
	}
}

func TestSessionCompactionConstraintsAndTenantIsolation(t *testing.T) {
	db := openTestDB(t)
	insertSessionTestUser(t, db, "user-a")
	insertSessionTestUser(t, db, "user-b")
	a1 := insertSessionTestTurn(t, db, "user-a", "session", 1, "a one", "answer")
	a2 := insertSessionTestTurn(t, db, "user-a", "session", 1, "a two", "answer")
	b1 := insertSessionTestTurn(t, db, "user-b", "session", 1, "b one", "answer")
	otherSession := insertSessionTestTurn(t, db, "user-a", "other-session", 1, "other session", "answer")
	otherGeneration := insertSessionTestTurn(t, db, "user-a", "session", 2, "other generation", "answer")
	summaryID := insertSessionTestSummary(t, db, "user-a", "session", 1, a1, a2)

	if _, err := db.SQL().Exec(`UPDATE session_summaries SET narrative = 'changed' WHERE id = ?`, summaryID); err == nil {
		t.Fatal("expected immutable summary update to fail")
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO session_summaries (
		canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
		narrative, source_turn_ids
	) VALUES ('user-a', 'session', 1, ?, ?, 'duplicate', json_array(?, ?))`, a1, a2, a1, a2); err == nil {
		t.Fatal("expected duplicate summary range to fail")
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO session_summaries (
		canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
		narrative, open_tasks, source_turn_ids
	) VALUES ('user-a', 'session', 1, ?, ?, 'invalid json', 'not-json', json_array(?, ?))`, a1, a2, a1, a2); err == nil {
		t.Fatal("expected invalid summary JSON to fail")
	}
	for name, sourceIDs := range map[string]string{
		"malformed":        `not-json`,
		"noninteger":       fmt.Sprintf(`[%d,"%d"]`, a1, a2),
		"duplicate":        fmt.Sprintf(`[%d,%d,%d]`, a1, a1, a2),
		"unordered":        fmt.Sprintf(`[%d,%d]`, a2, a1),
		"missing endpoint": fmt.Sprintf(`[%d]`, a1),
		"cross tenant":     fmt.Sprintf(`[%d,%d,%d]`, a1, b1, a2),
		"cross session":    fmt.Sprintf(`[%d,%d,%d]`, a1, otherSession, a2),
		"cross generation": fmt.Sprintf(`[%d,%d,%d]`, a1, otherGeneration, a2),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.SQL().Exec(`
INSERT INTO session_summaries (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, source_turn_ids
) VALUES ('user-a', 'session', 1, ?, ?, ?, ?)`, a1, a2, name, sourceIDs); err == nil {
				t.Fatalf("expected %s source turn array to fail", name)
			}
		})
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, state, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	compaction_target_turn_id, artifact_payload, attempt_count, compaction_invalid_output_retry_count, model_submission_count,
	corrective_error_code, compaction_model, compaction_generator_version, available_at, updated_at
	) VALUES ('session_compaction', 'maximum-retries', 'user-a', 'skipped', 'session', 1, ?, ?, ?, '{}', 4, 3, 4,
	'missing_tool_call', 'model', 'v1', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, a1, a2, a2); err != nil {
		t.Fatalf("maximum compaction retry state was rejected: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET compaction_invalid_output_retry_count = 4 WHERE idempotency_key = 'maximum-retries'`); err == nil {
		t.Fatal("expected excessive invalid-output retry count to fail")
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET model_submission_count = 5 WHERE idempotency_key = 'maximum-retries'`); err == nil {
		t.Fatal("expected excessive model submission count to fail")
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	compaction_target_turn_id, attempt_count, redrive_count, compaction_model, compaction_generator_version, available_at, updated_at
	) VALUES ('session_compaction', 'too-many-attempts', 'user-a', 'session', 1, ?, ?, ?, 5, 0, 'model', 'v1', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, a1, a2, a2); err == nil {
		t.Fatal("expected excessive attempt count to fail")
	}
	if _, err := db.SQL().Exec(`
	INSERT INTO durable_jobs (
	job_kind, idempotency_key, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	compaction_target_turn_id, compaction_model, compaction_generator_version, available_at, updated_at
	) VALUES ('session_compaction', 'cross-tenant', 'user-a', 'session', 1, ?, ?, ?, 'model', 'v1', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, a1, b1, b1); err == nil {
		t.Fatal("expected cross-tenant compaction range to fail")
	}
}

func insertSessionTestUser(t *testing.T, db *DB, userID string) {
	t.Helper()
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?)`, userID); err != nil {
		t.Fatalf("insert user %s: %v", userID, err)
	}
}

func insertSessionTestTurn(t *testing.T, db *DB, userID, sessionID string, generation int, userText, assistantText string) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`
INSERT INTO session_turns (canonical_user_id, session_id, session_generation, user_text, assistant_text, created_at, delivered_at)
VALUES (?, ?, ?, ?, ?, '2026-07-18T12:00:00Z', '2026-07-18T12:00:01Z')`, userID, sessionID, generation, userText, assistantText)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("turn id: %v", err)
	}
	return id
}

func insertSessionTestSummary(t *testing.T, db *DB, userID, sessionID string, generation int, fromID, throughID int64) int64 {
	t.Helper()
	result, err := db.SQL().Exec(`
INSERT INTO session_summaries (
	canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, open_tasks, commitments, entities, decisions, topic_tags,
	source_turn_ids
) VALUES (?, ?, ?, ?, ?, 'A summary.', '["task"]', '["commitment"]', '["entity"]', '["decision"]', '["topic"]', json_array(?, ?))`, userID, sessionID, generation, fromID, throughID, fromID, throughID)
	if err != nil {
		t.Fatalf("insert summary: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("summary id: %v", err)
	}
	return id
}
