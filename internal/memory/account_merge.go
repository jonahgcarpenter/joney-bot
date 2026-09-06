package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

// MergeUsersTx moves loser-owned memory data to the winner using the supplied transaction.
// It does not commit or roll back tx.
func MergeUsersTx(ctx context.Context, tx *sql.Tx, winnerID, loserID, intro string) error {
	if tx == nil {
		return fmt.Errorf("user memory merge: transaction is required")
	}
	winnerID = strings.TrimSpace(winnerID)
	loserID = strings.TrimSpace(loserID)
	if winnerID == "" || loserID == "" {
		return fmt.Errorf("user memory merge: winner and loser ids are required")
	}
	if winnerID == loserID {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer memory merge foreign keys: %w", err)
	}

	// Compaction checkpoints are immutable and turns with checkpoint references cannot
	// change tenant or generation. Snapshot the graph, remove its references, move the
	// turns, then restore the same checkpoint and job IDs.
	if _, err := tx.ExecContext(ctx, `
DROP TABLE IF EXISTS temp.merge_session_generation_map;
CREATE TEMP TABLE merge_session_generation_map AS
WITH loser_generations AS (
	SELECT session_id, session_generation AS generation FROM session_turns WHERE canonical_user_id = ?
	UNION SELECT session_id, session_generation FROM session_summaries WHERE canonical_user_id = ?
	UNION SELECT session_id, session_generation FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ?
	UNION SELECT session_id, generation FROM sessions WHERE canonical_user_id = ?
), numbered AS (
	SELECT session_id, generation,
		ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY generation) AS ordinal
	FROM loser_generations
)
SELECT numbered.session_id, numbered.generation AS old_generation,
	CASE WHEN EXISTS (
		SELECT 1 FROM (
			SELECT session_id, session_generation AS generation FROM session_turns WHERE canonical_user_id = ?
			UNION SELECT session_id, session_generation FROM session_summaries WHERE canonical_user_id = ?
			UNION SELECT session_id, session_generation FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ?
			UNION SELECT session_id, generation FROM sessions WHERE canonical_user_id = ?
		) winner_state
		WHERE winner_state.session_id = numbered.session_id AND winner_state.generation = numbered.generation
	) THEN COALESCE((
		SELECT MAX(generation) FROM (
			SELECT session_generation AS generation FROM session_turns WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
			UNION ALL SELECT session_generation FROM session_summaries WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
			UNION ALL SELECT session_generation FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id IN (?, ?) AND session_id = numbered.session_id
			UNION ALL SELECT generation FROM sessions WHERE canonical_user_id IN (?, ?) AND session_id = numbered.session_id
		)
	), 0) + numbered.ordinal
	ELSE numbered.generation END AS new_generation
FROM numbered;

DROP TABLE IF EXISTS temp.merge_session_summaries;
CREATE TEMP TABLE merge_session_summaries AS SELECT * FROM session_summaries WHERE canonical_user_id = ?;
DROP TABLE IF EXISTS temp.merge_compaction_jobs;
CREATE TEMP TABLE merge_compaction_jobs AS SELECT * FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ?;
DROP TABLE IF EXISTS temp.merge_sessions;
CREATE TEMP TABLE merge_sessions AS
SELECT sessions.session_id, COALESCE(map.new_generation, sessions.generation) AS generation,
	sessions.is_active, sessions.last_seen_at, sessions.expires_at,
	sessions.profile_version, sessions.profile_version_high_water, sessions.renderer_version,
	sessions.source_digest, sessions.speaker_intro, sessions.rendered_content, sessions.source_memory_ids
FROM sessions
LEFT JOIN merge_session_generation_map map
	ON map.session_id = sessions.session_id AND map.old_generation = sessions.generation
WHERE sessions.canonical_user_id = ?;

DELETE FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = ?;
DELETE FROM session_summaries WHERE canonical_user_id = ?;
DELETE FROM sessions WHERE canonical_user_id = ?;

UPDATE durable_jobs AS loser
SET idempotency_key = 'merge:' || ? || ':' || loser.idempotency_key || ':' || loser.id
WHERE loser.job_kind = 'memory_formation' AND loser.canonical_user_id = ?
	AND EXISTS (SELECT 1 FROM durable_jobs winner WHERE winner.job_kind = loser.job_kind AND winner.canonical_user_id = ? AND winner.idempotency_key = loser.idempotency_key);

UPDATE session_turns
SET canonical_user_id = ?,
	session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = session_turns.session_id AND map.old_generation = session_turns.session_generation), session_generation)
WHERE canonical_user_id = ?;

UPDATE durable_jobs
SET canonical_user_id = ?,
	source_session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = durable_jobs.source_session_id AND map.old_generation = durable_jobs.source_session_generation), source_session_generation),
	state = CASE WHEN state = 'running' THEN 'retry' ELSE state END,
	lease_owner = CASE WHEN state = 'running' THEN '' ELSE lease_owner END,
	lease_until = CASE WHEN state = 'running' THEN NULL ELSE lease_until END,
	available_at = CASE WHEN state = 'running' THEN ? ELSE available_at END,
	updated_at = CASE WHEN state = 'running' THEN ? ELSE updated_at END
WHERE job_kind = 'memory_formation' AND canonical_user_id = ?;
`, loserID, loserID, loserID, loserID,
		winnerID, winnerID, winnerID, winnerID,
		winnerID, loserID, winnerID, loserID, winnerID, loserID, winnerID, loserID,
		loserID, loserID, loserID,
		loserID, loserID, loserID,
		loserID, loserID, winnerID,
		winnerID, loserID,
		winnerID, formatTime(time.Now().UTC()), formatTime(time.Now().UTC()), loserID); err != nil {
		return fmt.Errorf("snapshot and move merged sessions: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO session_summaries (
	id, canonical_user_id, session_id, session_generation, covered_from_turn_id, covered_through_turn_id,
	narrative, open_tasks, commitments, entities, decisions, topic_tags, source_turn_ids
)
SELECT summary.id, ?, summary.session_id, COALESCE(map.new_generation, summary.session_generation),
	summary.covered_from_turn_id, summary.covered_through_turn_id, summary.narrative, summary.open_tasks,
	summary.commitments, summary.entities, summary.decisions, summary.topic_tags, summary.source_turn_ids
FROM merge_session_summaries summary
LEFT JOIN merge_session_generation_map map
	ON map.session_id = summary.session_id AND map.old_generation = summary.session_generation;

UPDATE merge_compaction_jobs
SET canonical_user_id = ?,
	session_generation = COALESCE((SELECT new_generation FROM merge_session_generation_map map WHERE map.session_id = merge_compaction_jobs.session_id AND map.old_generation = merge_compaction_jobs.session_generation), session_generation),
	state = CASE WHEN state = 'running' THEN 'retry' ELSE state END,
	lease_owner = CASE WHEN state = 'running' THEN '' ELSE lease_owner END,
	lease_until = CASE WHEN state = 'running' THEN NULL ELSE lease_until END;
INSERT INTO durable_jobs SELECT * FROM merge_compaction_jobs;

UPDATE durable_jobs
SET state = 'retry', lease_owner = '', lease_until = NULL
WHERE job_kind = 'session_compaction' AND canonical_user_id = ? AND state = 'running';
`, winnerID, winnerID, winnerID); err != nil {
		return fmt.Errorf("restore merged session compaction state: %w", err)
	}

	mergeTurnNow := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'merge:turn:' || ? || ':' || id || ':' || created_at, ?, 'session_turn', id, 'upsert', ?, ?
FROM session_turns WHERE canonical_user_id = ?
ON CONFLICT(job_kind, idempotency_key) DO UPDATE SET canonical_user_id = excluded.canonical_user_id,
	entity_kind = excluded.entity_kind, entity_id = excluded.entity_id, operation = excluded.operation,
	state = 'queued', available_at = excluded.available_at, lease_owner = '', lease_until = NULL,
	completed_at = NULL, last_error_code = '', updated_at = excluded.updated_at`, loserID, winnerID, mergeTurnNow, mergeTurnNow, winnerID); err != nil {
		return fmt.Errorf("enqueue merged transcript indexes: %w", err)
	}

	// Profile version numbers are tenant-local. Place the losing snapshots after
	// the winner's high-water before restoring the consolidated session rows.
	if _, err := tx.ExecContext(ctx, `
UPDATE merge_sessions
SET profile_version = profile_version + (SELECT COALESCE(MAX(profile_version_high_water), 0) FROM sessions WHERE canonical_user_id = ?),
	profile_version_high_water = profile_version_high_water + (SELECT COALESCE(MAX(profile_version_high_water), 0) FROM sessions WHERE canonical_user_id = ?);
`, winnerID, winnerID); err != nil {
		return fmt.Errorf("renumber merged tenant profiles: %w", err)
	}

	// Tenant-scoped idempotency keys become colliding only after ownership moves.
	// Re-key just those collisions, including the row ID so the mapping is stable.
	for _, table := range []string{"memory_candidates"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` AS loser SET idempotency_key = 'merge:' || ? || ':' || loser.idempotency_key || ':' || loser.id WHERE loser.canonical_user_id = ? AND EXISTS (SELECT 1 FROM `+table+` winner WHERE winner.canonical_user_id = ? AND winner.idempotency_key = loser.idempotency_key)`, loserID, loserID, winnerID); err != nil {
			return fmt.Errorf("re-key merged %s: %w", table, err)
		}
	}
	duplicateJoin := `
FROM memory_entries loser
JOIN memory_entries winner
	ON winner.canonical_user_id = ?
	AND winner.scope = loser.scope
		AND winner.claim_slot = loser.claim_slot
		AND winner.claim_value = loser.claim_value
WHERE loser.canonical_user_id = ?`
	duplicateIDs := `SELECT loser.id ` + duplicateJoin
	type mergedMemoryDuplicate struct {
		loserID, winnerID                   int64
		loserConfidence, winnerConfidence   float64
		loserImportance, winnerImportance   int
		loserProvenance, winnerProvenance   string
		loserSensitivity, winnerSensitivity string
		loserClaimSlot, winnerClaimSlot     string
		loserClaimValue, winnerClaimValue   string
		loserStatement, loserCategory       string
	}
	duplicateRows, err := tx.QueryContext(ctx, `SELECT loser.id, winner.id, loser.confidence, winner.confidence, loser.importance, winner.importance, loser.provenance_type, winner.provenance_type, loser.sensitivity, winner.sensitivity, loser.claim_slot, winner.claim_slot, loser.claim_value, winner.claim_value, loser.statement, loser.category `+duplicateJoin, winnerID, loserID)
	if err != nil {
		return fmt.Errorf("read merged confidence duplicates: %w", err)
	}
	var mergedDuplicates []mergedMemoryDuplicate
	for duplicateRows.Next() {
		var duplicate mergedMemoryDuplicate
		if err := duplicateRows.Scan(&duplicate.loserID, &duplicate.winnerID, &duplicate.loserConfidence, &duplicate.winnerConfidence, &duplicate.loserImportance, &duplicate.winnerImportance, &duplicate.loserProvenance, &duplicate.winnerProvenance, &duplicate.loserSensitivity, &duplicate.winnerSensitivity, &duplicate.loserClaimSlot, &duplicate.winnerClaimSlot, &duplicate.loserClaimValue, &duplicate.winnerClaimValue, &duplicate.loserStatement, &duplicate.loserCategory); err != nil {
			duplicateRows.Close()
			return fmt.Errorf("scan merged confidence duplicate: %w", err)
		}
		mergedDuplicates = append(mergedDuplicates, duplicate)
	}
	if err := duplicateRows.Close(); err != nil {
		return fmt.Errorf("close merged confidence duplicates: %w", err)
	}
	for _, duplicate := range mergedDuplicates {
		provenance := strongestMemoryProvenance(duplicate.winnerProvenance, duplicate.loserProvenance)
		useLoser := provenanceAuthorityRank(duplicate.loserProvenance) > provenanceAuthorityRank(duplicate.winnerProvenance) ||
			(provenanceAuthorityRank(duplicate.loserProvenance) == provenanceAuthorityRank(duplicate.winnerProvenance) && duplicate.loserConfidence > duplicate.winnerConfidence)
		statement, category := "", ""
		if useLoser {
			statement, category = duplicate.loserStatement, duplicate.loserCategory
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET confidence = ?, importance = ?, provenance_type = ?, sensitivity = ?, statement = CASE WHEN ? = '' THEN statement ELSE ? END, category = CASE WHEN ? = '' THEN category ELSE ? END WHERE id = ? AND canonical_user_id = ?`, max(duplicate.winnerConfidence, duplicate.loserConfidence), max(duplicate.winnerImportance, duplicate.loserImportance), provenance, strongestSensitivity(duplicate.winnerSensitivity, duplicate.loserSensitivity), statement, statement, category, category, duplicate.winnerID, winnerID); err != nil {
			return fmt.Errorf("merge confidence evidence metadata: %w", err)
		}
		if err := enqueueDerivedChangeTx(ctx, tx, winnerID, "memory", duplicate.winnerID, "upsert", "account-merge-confidence:"+mergeTurnNow); err != nil {
			return err
		}
	}
	winnerForSuperseded := `
SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_entries.supersedes_id`
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET supersedes_id = (`+winnerForSuperseded+`) WHERE supersedes_id IN (`+duplicateIDs+`)`, winnerID, loserID, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to redirect merged supersedes references: %w", err)
	}
	winnerForCandidatePublished := `SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_candidates.published_memory_id`
	winnerForCandidateSupersedes := `SELECT winner.id ` + duplicateJoin + ` AND loser.id = memory_candidates.supersedes_memory_id`
	if _, err := tx.ExecContext(ctx, `
DROP TABLE IF EXISTS temp.merge_candidate_links;
CREATE TEMP TABLE merge_candidate_links AS
	SELECT id,
		CASE WHEN published_memory_id IN (`+duplicateIDs+`) THEN (`+winnerForCandidatePublished+`) ELSE published_memory_id END AS published_memory_id,
		CASE WHEN supersedes_memory_id IN (`+duplicateIDs+`) THEN (`+winnerForCandidateSupersedes+`) ELSE supersedes_memory_id END AS supersedes_memory_id,
		source_turn_id
	FROM memory_candidates WHERE canonical_user_id = ?;
UPDATE memory_candidates SET published_memory_id = NULL, supersedes_memory_id = NULL, source_turn_id = NULL WHERE canonical_user_id = ?;
`, winnerID, loserID, winnerID, loserID, winnerID, loserID, winnerID, loserID, loserID, loserID); err != nil {
		return fmt.Errorf("snapshot merged formation relationships: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE merge_sessions SET source_memory_ids = COALESCE((SELECT json_group_array(COALESCE((SELECT winner.id `+duplicateJoin+` AND loser.id = CAST(source.value AS INTEGER)), CAST(source.value AS INTEGER))) FROM json_each(merge_sessions.source_memory_ids) source), '[]')`, winnerID, loserID); err != nil {
		return fmt.Errorf("redirect merged profile sources: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET lifecycle_state = 'erasing' WHERE canonical_user_id = ?`, loserID); err != nil {
		return fmt.Errorf("fence merged account retirement: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.merge_duplicate_memory_ids; CREATE TEMP TABLE merge_duplicate_memory_ids AS SELECT id FROM memory_entries WHERE id IN (`+duplicateIDs+`)`, winnerID, loserID); err != nil {
		return fmt.Errorf("snapshot duplicate merged memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_candidates SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memory candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entries WHERE id IN (SELECT id FROM merge_duplicate_memory_ids)`); err != nil {
		return fmt.Errorf("failed to delete duplicate memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
		return fmt.Errorf("failed to move merged memories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE memory_candidates
SET published_memory_id = (SELECT published_memory_id FROM merge_candidate_links links WHERE links.id = memory_candidates.id),
	supersedes_memory_id = (SELECT supersedes_memory_id FROM merge_candidate_links links WHERE links.id = memory_candidates.id),
	source_turn_id = (SELECT source_turn_id FROM merge_candidate_links links WHERE links.id = memory_candidates.id)
WHERE canonical_user_id = ? AND id IN (SELECT id FROM merge_candidate_links);
DROP TABLE merge_candidate_links;
`, winnerID); err != nil {
		return fmt.Errorf("restore merged formation relationships: %w", err)
	}
	mergeNow := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'merge:memory:' || ? || ':' || id || ':' || updated_at, ?, 'memory', id, 'upsert', ?, ?
FROM memory_entries WHERE canonical_user_id = ?
ON CONFLICT(job_kind, idempotency_key) DO UPDATE SET canonical_user_id = excluded.canonical_user_id,
	entity_kind = excluded.entity_kind, entity_id = excluded.entity_id, operation = excluded.operation,
	state = 'queued', available_at = excluded.available_at, lease_owner = '', lease_until = NULL,
	completed_at = NULL, last_error_code = '', updated_at = excluded.updated_at`, loserID, winnerID, mergeNow, mergeNow, winnerID); err != nil {
		return fmt.Errorf("enqueue merged memory indexes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'merge:memory-delete:' || ? || ':' || id, ?, 'memory', id, 'delete', ?, ?
FROM merge_duplicate_memory_ids
WHERE 1
ON CONFLICT(job_kind, idempotency_key) DO UPDATE SET canonical_user_id = excluded.canonical_user_id,
	entity_kind = excluded.entity_kind, entity_id = excluded.entity_id, operation = excluded.operation,
	state = 'queued', available_at = excluded.available_at, lease_owner = '', lease_until = NULL,
	completed_at = NULL, last_error_code = '', updated_at = excluded.updated_at;
DROP TABLE merge_duplicate_memory_ids`, loserID, winnerID, mergeNow, mergeNow); err != nil {
		return fmt.Errorf("enqueue duplicate merged memory index deletion: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sessions (canonical_user_id, session_id, generation, is_active, last_seen_at, expires_at,
	profile_version, profile_version_high_water, renderer_version, source_digest, speaker_intro, rendered_content,
	source_memory_ids)
SELECT ?, session_id, generation, is_active, last_seen_at, expires_at,
	profile_version, profile_version_high_water, renderer_version, source_digest, speaker_intro, rendered_content,
	source_memory_ids
FROM merge_sessions
WHERE 1
ON CONFLICT(canonical_user_id, session_id) DO UPDATE SET
	generation = MAX(sessions.generation, excluded.generation),
	is_active = CASE WHEN excluded.generation > sessions.generation THEN excluded.is_active ELSE sessions.is_active END,
	last_seen_at = MAX(sessions.last_seen_at, excluded.last_seen_at),
	expires_at = CASE WHEN excluded.generation > sessions.generation THEN excluded.expires_at ELSE sessions.expires_at END,
	profile_version = CASE WHEN excluded.generation > sessions.generation THEN excluded.profile_version ELSE sessions.profile_version END,
	profile_version_high_water = MAX(sessions.profile_version_high_water, excluded.profile_version_high_water),
	renderer_version = CASE WHEN excluded.generation > sessions.generation THEN excluded.renderer_version ELSE sessions.renderer_version END,
	source_digest = CASE WHEN excluded.generation > sessions.generation THEN excluded.source_digest ELSE sessions.source_digest END,
	speaker_intro = CASE WHEN excluded.generation > sessions.generation THEN excluded.speaker_intro ELSE sessions.speaker_intro END,
	rendered_content = CASE WHEN excluded.generation > sessions.generation THEN excluded.rendered_content ELSE sessions.rendered_content END,
	source_memory_ids = CASE WHEN excluded.generation > sessions.generation THEN excluded.source_memory_ids ELSE sessions.source_memory_ids END;
`, winnerID); err != nil {
		return fmt.Errorf("restore merged sessions: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE account_users SET speaker_intro = ? WHERE canonical_user_id = ?`, strings.TrimSpace(intro), winnerID); err != nil {
		return fmt.Errorf("failed to update merged speaker intro: %w", err)
	}
	if _, _, err := refreshProfileTx(ctx, tx, winnerID, time.Now().UTC()); err != nil {
		return fmt.Errorf("publish unified merged tenant profile: %w", err)
	}

	// Existing outbox work remains useful after a merge. Move it and make any
	// in-flight lease retryable under the winner before queuing reconciliation work.
	if _, err := tx.ExecContext(ctx, `
UPDATE durable_jobs
SET canonical_user_id = ?,
	state = CASE WHEN state = 'running' THEN 'retry' ELSE state END,
	lease_owner = CASE WHEN state = 'running' THEN '' ELSE lease_owner END,
	lease_until = CASE WHEN state = 'running' THEN NULL ELSE lease_until END,
	available_at = CASE WHEN state = 'running' THEN ? ELSE available_at END,
	updated_at = CASE WHEN state = 'running' THEN ? ELSE updated_at END
WHERE job_kind = 'derived_index' AND canonical_user_id = ?;
`, winnerID, mergeNow, mergeNow, loserID); err != nil {
		return fmt.Errorf("move merged derived index changes: %w", err)
	}

	var remaining int
	if err := tx.QueryRowContext(ctx, `
SELECT SUM(row_count) FROM (
	SELECT COUNT(*) row_count FROM memory_entries WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM sessions WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM durable_jobs WHERE canonical_user_id = ?
	UNION ALL SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = ?
)
`, loserID, loserID, loserID, loserID, loserID, loserID).Scan(&remaining); err != nil {
		return fmt.Errorf("verify merged tenant ownership: %w", err)
	}
	if remaining != 0 {
		return fmt.Errorf("verify merged tenant ownership: %d loser-owned rows remain", remaining)
	}
	return nil
}

// MergeUsersTx moves user memory through a caller-owned transaction.
func (s *Store) MergeUsersTx(ctx context.Context, tx *sql.Tx, winnerID, loserID, intro string) error {
	return MergeUsersTx(ctx, tx, winnerID, loserID, intro)
}

func resolveActiveMemoryByStatementTx(ctx context.Context, tx *sql.Tx, userID, scope, statement string) (int64, error) {
	_, target := policy.NormalizeClaimIdentity(policy.CategoryNotes, "", "", statement)
	rows, err := tx.QueryContext(ctx, `SELECT id, statement FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND status = 'active' ORDER BY id`, userID, scope)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var existing string
		if err := rows.Scan(&id, &existing); err != nil {
			return 0, err
		}
		_, normalized := policy.NormalizeClaimIdentity(policy.CategoryNotes, "", "", existing)
		if normalized == target {
			return id, nil
		}
	}
	return 0, rows.Err()
}
