package usermemory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Derived index kinds persisted in derived_index_revisions.
const (
	IndexKindMemoryFTS          = "memory_fts"
	IndexKindTranscriptFTS      = "transcript_fts"
	IndexKindMemoryVector       = "memory_vector"
	IndexKindGlobalMemoryFTS    = "global_memory_fts"
	IndexKindGlobalMemoryVector = "global_memory_vector"
)

var generatedIndexTable = regexp.MustCompile(`^derived_index_(memory_fts|transcript_fts|memory_vector|global_memory_fts|global_memory_vector)_r[1-9][0-9]*$`)

// ErrStaleIndexRecord reports that canonical state changed after an index
// record was loaded. Callers should reload canonical state and retry.
var ErrStaleIndexRecord = errors.New("stale derived index record")

// ErrDerivedIndexDegraded reports that a serving revision is known incomplete
// while remaining available as a best-effort retrieval channel.
var ErrDerivedIndexDegraded = errors.New("derived index coverage is degraded")

// ErrStaleDerivedIndexChangeLease indicates that a claimed outbox lease was
// released or reclaimed by another worker.
var ErrStaleDerivedIndexChangeLease = errors.New("stale derived index change lease")

// DerivedIndexRevision describes one immutable physical index generation.
type DerivedIndexRevision struct {
	ID, Revision                int64
	Kind, Model                 string
	Dimension, SchemaVersion    int
	TableName, State            string
	ExpectedCount, IndexedCount int64
	CreatedAt, UpdatedAt        time.Time
}

// DerivedIndexChange is one leased canonical mutation from the durable outbox.
type DerivedIndexChange struct {
	Sequence, EntityID            int64
	UserID, EntityKind, Operation string
	AttemptCount                  int
	LeaseOwner                    string
	LeaseUntil                    time.Time
}

// MemoryIndexRecord is canonical content eligible for memory indexes.
type MemoryIndexRecord struct {
	ID                                           int64
	UserID, Scope, Category, Statement, Evidence string
	Version                                      string
}

// GlobalMemoryIndexRecord is canonical content eligible for shared indexes.
type GlobalMemoryIndexRecord struct {
	ID      int64
	Memory  string
	Version string
}

// TranscriptIndexRecord is canonical content eligible for transcript search.
type TranscriptIndexRecord struct {
	ID                                                   int64
	UserID, SessionID, UserText, AssistantText, ToolText string
	Generation                                           int
	Version                                              string
}

func providerForKind(kind string) string {
	if kind == IndexKindMemoryVector || kind == IndexKindGlobalMemoryVector {
		return "llm_gateway"
	}
	return "sqlite_fts5"
}

// LiveIndexRevision returns the active physical revision for a kind.
func (s *Store) LiveIndexRevision(ctx context.Context, kind string) (DerivedIndexRevision, error) {
	return scanIndexRevision(s.sql.QueryRowContext(ctx, `SELECT id, revision, index_kind, model, dimension, schema_version, table_name, state, expected_count, indexed_count, created_at, updated_at FROM derived_index_revisions WHERE index_kind = ? AND state = 'live'`, kind))
}

// LiveIndexDegraded reports whether the serving revision has a recorded health error.
func (s *Store) LiveIndexDegraded(ctx context.Context, kind string) (bool, error) {
	var code string
	if err := s.sql.QueryRowContext(ctx, `SELECT last_error_code FROM derived_index_revisions WHERE index_kind = ? AND state = 'live'`, kind).Scan(&code); err != nil {
		return false, err
	}
	return code != "", nil
}

// BuildingIndexRevision returns the current shadow revision, if any.
func (s *Store) BuildingIndexRevision(ctx context.Context, kind string) (DerivedIndexRevision, error) {
	return scanIndexRevision(s.sql.QueryRowContext(ctx, `SELECT id, revision, index_kind, model, dimension, schema_version, table_name, state, expected_count, indexed_count, created_at, updated_at FROM derived_index_revisions WHERE index_kind = ? AND state = 'building' ORDER BY revision DESC LIMIT 1`, kind))
}

func scanIndexRevision(row interface{ Scan(...any) error }) (DerivedIndexRevision, error) {
	var revision DerivedIndexRevision
	var created, updated string
	err := row.Scan(&revision.ID, &revision.Revision, &revision.Kind, &revision.Model, &revision.Dimension, &revision.SchemaVersion, &revision.TableName, &revision.State, &revision.ExpectedCount, &revision.IndexedCount, &created, &updated)
	revision.CreatedAt, revision.UpdatedAt = parseTime(created), parseTime(updated)
	return revision, err
}

// CreateIndexRevision creates an empty internally named shadow table.
func (s *Store) CreateIndexRevision(ctx context.Context, kind, provider, model string, dimension int) (DerivedIndexRevision, error) {
	if kind != IndexKindMemoryFTS && kind != IndexKindTranscriptFTS && kind != IndexKindMemoryVector && kind != IndexKindGlobalMemoryFTS && kind != IndexKindGlobalMemoryVector {
		return DerivedIndexRevision{}, fmt.Errorf("invalid derived index kind")
	}
	if provider != providerForKind(kind) {
		return DerivedIndexRevision{}, fmt.Errorf("invalid derived index provider")
	}
	if (kind == IndexKindMemoryVector || kind == IndexKindGlobalMemoryVector) && (provider != "llm_gateway" || strings.TrimSpace(model) == "" || dimension <= 0) {
		return DerivedIndexRevision{}, fmt.Errorf("invalid vector revision metadata")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return DerivedIndexRevision{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM derived_index_revisions WHERE index_kind = ?`, kind).Scan(&revision); err != nil {
		return DerivedIndexRevision{}, err
	}
	table := fmt.Sprintf("derived_index_%s_r%d", kind, revision)
	if err := validateGeneratedTable(table); err != nil {
		return DerivedIndexRevision{}, err
	}
	ddl := `CREATE VIRTUAL TABLE ` + table + ` USING fts5(canonical_user_id, statement, evidence)`
	if kind == IndexKindTranscriptFTS {
		ddl = `CREATE VIRTUAL TABLE ` + table + ` USING fts5(canonical_user_id, session_id, session_generation, user_text, assistant_text, tool_text)`
	} else if kind == IndexKindMemoryVector {
		ddl = fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING vec0(canonical_user_id text, embedding_model text, canonical_version text, scope text, category text, embedding float[%d])`, table, dimension)
	} else if kind == IndexKindGlobalMemoryFTS {
		ddl = `CREATE VIRTUAL TABLE ` + table + ` USING fts5(memory)`
	} else if kind == IndexKindGlobalMemoryVector {
		ddl = fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING vec0(embedding_model text, canonical_version text, embedding float[%d])`, table, dimension)
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return DerivedIndexRevision{}, fmt.Errorf("create derived index table: %w", err)
	}
	schemaVersion := 1
	if kind == IndexKindMemoryVector || kind == IndexKindGlobalMemoryVector || kind == IndexKindTranscriptFTS {
		schemaVersion = 2
	}
	now := formatTime(time.Now().UTC())
	result, err := tx.ExecContext(ctx, `INSERT INTO derived_index_revisions(index_kind, model, dimension, schema_version, revision, table_name, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 'building', ?, ?)`, kind, strings.TrimSpace(model), dimension, schemaVersion, revision, table, now, now)
	if err != nil {
		return DerivedIndexRevision{}, err
	}
	id, _ := result.LastInsertId()
	if err := tx.Commit(); err != nil {
		return DerivedIndexRevision{}, err
	}
	return s.indexRevisionByID(ctx, id)
}

func validateGeneratedTable(table string) error {
	if !generatedIndexTable.MatchString(table) {
		return fmt.Errorf("invalid generated derived-index table name")
	}
	return nil
}

func validateRevisionTableIdentity(revision DerivedIndexRevision) error {
	want := fmt.Sprintf("derived_index_%s_r%d", revision.Kind, revision.Revision)
	if revision.TableName != want || validateGeneratedTable(revision.TableName) != nil {
		return fmt.Errorf("derived-index table identity mismatch")
	}
	return nil
}

func (s *Store) indexRevisionByID(ctx context.Context, id int64) (DerivedIndexRevision, error) {
	return scanIndexRevision(s.sql.QueryRowContext(ctx, `SELECT id, revision, index_kind, model, dimension, schema_version, table_name, state, expected_count, indexed_count, created_at, updated_at FROM derived_index_revisions WHERE id = ?`, id))
}

// ActiveMemoryIndexRecords enumerates canonical active approved unexpired rows.
func (s *Store) ActiveMemoryIndexRecords(ctx context.Context, afterID int64, limit int) ([]MemoryIndexRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT entries.id, entries.canonical_user_id, entries.scope, entries.category, entries.statement, COALESCE((SELECT evidence FROM memory_candidates candidate WHERE candidate.canonical_user_id = entries.canonical_user_id AND candidate.published_memory_id = entries.id AND candidate.evidence != '' ORDER BY CASE candidate.provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, candidate.confidence DESC, candidate.id LIMIT 1), ''), entries.updated_at FROM memory_entries entries WHERE entries.id > ? AND entries.status = 'active' AND (entries.expires_at IS NULL OR entries.expires_at > ?) ORDER BY entries.id LIMIT ?`, afterID, formatTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []MemoryIndexRecord
	for rows.Next() {
		var record MemoryIndexRecord
		if err := rows.Scan(&record.ID, &record.UserID, &record.Scope, &record.Category, &record.Statement, &record.Evidence, &record.Version); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// GlobalMemoryIndexRecords enumerates canonical shared memories.
func (s *Store) GlobalMemoryIndexRecords(ctx context.Context, afterID int64, limit int) ([]GlobalMemoryIndexRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT id, memory, created_at FROM global_memories WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []GlobalMemoryIndexRecord
	for rows.Next() {
		var record GlobalMemoryIndexRecord
		if err := rows.Scan(&record.ID, &record.Memory, &record.Version); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// DeliveredTranscriptIndexRecords enumerates delivered active-generation rows.
func (s *Store) DeliveredTranscriptIndexRecords(ctx context.Context, afterID int64, limit int) ([]TranscriptIndexRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT turns.id, turns.canonical_user_id, turns.session_id, turns.session_generation, turns.user_text, turns.assistant_text, turns.tool_search_text, turns.delivered_at FROM session_turns turns JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.id > ? AND turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ? ORDER BY turns.id LIMIT ?`, afterID, formatTime(time.Now().UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []TranscriptIndexRecord
	for rows.Next() {
		var record TranscriptIndexRecord
		if err := rows.Scan(&record.ID, &record.UserID, &record.SessionID, &record.Generation, &record.UserText, &record.AssistantText, &record.ToolText, &record.Version); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// MemoryIndexRecordByID resolves current canonical eligibility and ownership.
func (s *Store) MemoryIndexRecordByID(ctx context.Context, id int64, userID string) (MemoryIndexRecord, error) {
	var record MemoryIndexRecord
	err := s.sql.QueryRowContext(ctx, `SELECT entries.id, entries.canonical_user_id, entries.scope, entries.category, entries.statement, COALESCE((SELECT evidence FROM memory_candidates candidate WHERE candidate.canonical_user_id = entries.canonical_user_id AND candidate.published_memory_id = entries.id AND candidate.evidence != '' ORDER BY CASE candidate.provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, candidate.confidence DESC, candidate.id LIMIT 1), ''), entries.updated_at FROM memory_entries entries WHERE entries.id = ? AND entries.canonical_user_id = ? AND entries.status = 'active' AND (entries.expires_at IS NULL OR entries.expires_at > ?)`, id, userID, formatTime(time.Now().UTC())).Scan(&record.ID, &record.UserID, &record.Scope, &record.Category, &record.Statement, &record.Evidence, &record.Version)
	return record, err
}

// GlobalMemoryIndexRecordByID resolves current canonical shared content.
func (s *Store) GlobalMemoryIndexRecordByID(ctx context.Context, id int64) (GlobalMemoryIndexRecord, error) {
	var record GlobalMemoryIndexRecord
	err := s.sql.QueryRowContext(ctx, `SELECT id, memory, created_at FROM global_memories WHERE id = ?`, id).Scan(&record.ID, &record.Memory, &record.Version)
	return record, err
}

// TranscriptIndexRecordByID resolves delivered active-generation eligibility.
func (s *Store) TranscriptIndexRecordByID(ctx context.Context, id int64, userID string) (TranscriptIndexRecord, error) {
	var record TranscriptIndexRecord
	err := s.sql.QueryRowContext(ctx, `SELECT turns.id, turns.canonical_user_id, turns.session_id, turns.session_generation, turns.user_text, turns.assistant_text, turns.tool_search_text, turns.delivered_at FROM session_turns turns JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.id = ? AND turns.canonical_user_id = ? AND turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ?`, id, userID, formatTime(time.Now().UTC())).Scan(&record.ID, &record.UserID, &record.SessionID, &record.Generation, &record.UserText, &record.AssistantText, &record.ToolText, &record.Version)
	return record, err
}

// WriteMemoryIndexRecord idempotently updates one memory revision row.
func (s *Store) WriteMemoryIndexRecord(ctx context.Context, revision DerivedIndexRevision, record MemoryIndexRecord, vector []float64) error {
	if err := validateGeneratedTable(revision.TableName); err != nil {
		return err
	}
	if revision.Kind != IndexKindMemoryFTS && (revision.Kind != IndexKindMemoryVector || len(vector) != revision.Dimension) {
		return fmt.Errorf("invalid memory index write")
	}
	if s.indexWriteHook != nil {
		s.indexWriteHook("before_recheck")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	var current MemoryIndexRecord
	err = tx.QueryRowContext(ctx, `SELECT entries.id, entries.canonical_user_id, entries.scope, entries.category, entries.statement, COALESCE((SELECT evidence FROM memory_candidates candidate WHERE candidate.canonical_user_id = entries.canonical_user_id AND candidate.published_memory_id = entries.id AND candidate.evidence != '' ORDER BY CASE candidate.provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, candidate.confidence DESC, candidate.id LIMIT 1), ''), entries.updated_at FROM memory_entries entries WHERE entries.id = ? AND entries.canonical_user_id = ? AND entries.status = 'active' AND (entries.expires_at IS NULL OR entries.expires_at > ?)`, record.ID, record.UserID, formatTime(time.Now().UTC())).Scan(&current.ID, &current.UserID, &current.Scope, &current.Category, &current.Statement, &current.Evidence, &current.Version)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != record) {
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = ?`, record.ID, record.UserID); deleteErr != nil {
			return deleteErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		return ErrStaleIndexRecord
	}
	if err != nil {
		return err
	}
	if s.indexWriteHook != nil {
		s.indexWriteHook("after_recheck")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = ?`, record.ID, record.UserID); err != nil {
		return err
	}
	if revision.Kind == IndexKindMemoryFTS {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid, canonical_user_id, statement, evidence) VALUES (?, ?, ?, ?)`, record.ID, record.UserID, record.Statement, record.Evidence)
	} else {
		serialized, serializeErr := serializeVector(vector)
		if serializeErr != nil {
			return serializeErr
		}
		if revision.SchemaVersion >= 2 && generatedIndexTable.MatchString(revision.TableName) {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid, canonical_user_id, embedding_model, canonical_version, scope, category, embedding) VALUES (?, ?, ?, ?, ?, ?, ?)`, record.ID, record.UserID, revision.Model, record.Version, record.Scope, record.Category, serialized)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid, canonical_user_id, embedding_model, scope, category, embedding) VALUES (?, ?, ?, ?, ?, ?)`, record.ID, record.UserID, revision.Model, record.Scope, record.Category, serialized)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// WriteTranscriptIndexRecord idempotently updates one transcript revision row.
func (s *Store) WriteTranscriptIndexRecord(ctx context.Context, revision DerivedIndexRevision, record TranscriptIndexRecord) error {
	if revision.Kind != IndexKindTranscriptFTS {
		return fmt.Errorf("invalid transcript index write")
	}
	if err := validateGeneratedTable(revision.TableName); err != nil {
		return err
	}
	if s.indexWriteHook != nil {
		s.indexWriteHook("before_recheck")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	var current TranscriptIndexRecord
	err = tx.QueryRowContext(ctx, `SELECT turns.id, turns.canonical_user_id, turns.session_id, turns.session_generation, turns.user_text, turns.assistant_text, turns.tool_search_text, turns.delivered_at FROM session_turns turns JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.id = ? AND turns.canonical_user_id = ? AND turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ?`, record.ID, record.UserID, formatTime(time.Now().UTC())).Scan(&current.ID, &current.UserID, &current.SessionID, &current.Generation, &current.UserText, &current.AssistantText, &current.ToolText, &current.Version)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != record) {
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = ?`, record.ID, record.UserID); deleteErr != nil {
			return deleteErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		return ErrStaleIndexRecord
	}
	if err != nil {
		return err
	}
	if s.indexWriteHook != nil {
		s.indexWriteHook("after_recheck")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = ?`, record.ID, record.UserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid, canonical_user_id, session_id, session_generation, user_text, assistant_text, tool_text) VALUES (?, ?, ?, ?, ?, ?, ?)`, record.ID, record.UserID, record.SessionID, record.Generation, record.UserText, record.AssistantText, record.ToolText); err != nil {
		return err
	}
	return tx.Commit()
}

// WriteGlobalMemoryIndexRecord idempotently updates one shared-memory revision row.
func (s *Store) WriteGlobalMemoryIndexRecord(ctx context.Context, revision DerivedIndexRevision, record GlobalMemoryIndexRecord, vector []float64) error {
	if err := validateGeneratedTable(revision.TableName); err != nil {
		return err
	}
	if revision.Kind != IndexKindGlobalMemoryFTS && (revision.Kind != IndexKindGlobalMemoryVector || len(vector) != revision.Dimension) {
		return fmt.Errorf("invalid global memory index write")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	var current GlobalMemoryIndexRecord
	err = tx.QueryRowContext(ctx, `SELECT id, memory, created_at FROM global_memories WHERE id = ?`, record.ID).Scan(&current.ID, &current.Memory, &current.Version)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != record) {
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ?`, record.ID); deleteErr != nil {
			return deleteErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		return ErrStaleIndexRecord
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ?`, record.ID); err != nil {
		return err
	}
	if revision.Kind == IndexKindGlobalMemoryFTS {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid, memory) VALUES (?, ?)`, record.ID, record.Memory)
	} else {
		serialized, serializeErr := serializeVector(vector)
		if serializeErr != nil {
			return serializeErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO `+revision.TableName+`(rowid, embedding_model, canonical_version, embedding) VALUES (?, ?, ?, ?)`, record.ID, revision.Model, record.Version, serialized)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// IndexRevisionNeedsRebuild reports whether the serving revision is unhealthy
// or its physical table is missing.
func (s *Store) IndexRevisionNeedsRebuild(ctx context.Context, kind string) (bool, error) {
	var table, code string
	var schemaVersion int
	err := s.sql.QueryRowContext(ctx, `SELECT table_name, last_error_code, schema_version FROM derived_index_revisions WHERE index_kind = ? AND state = 'live'`, kind).Scan(&table, &code, &schemaVersion)
	if err != nil {
		return true, err
	}
	var exists int
	if err := s.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
		return true, err
	}
	if code != "" || exists == 0 || (kind == IndexKindTranscriptFTS && schemaVersion != 2) {
		return true, nil
	}
	if err := validateGeneratedTable(table); err != nil {
		return true, err
	}
	var rows int64
	if err := s.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&rows); err != nil {
		return true, nil
	}
	return false, nil
}

// DeleteIndexRecord removes one tenant-owned row from a revision.
func (s *Store) DeleteIndexRecord(ctx context.Context, revision DerivedIndexRevision, id int64, userID string) error {
	if err := validateGeneratedTable(revision.TableName); err != nil {
		return err
	}
	_, err := s.sql.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = ?`, id, userID)
	return err
}

// DeleteGlobalMemoryIndexRecord removes one shared row from a revision.
func (s *Store) DeleteGlobalMemoryIndexRecord(ctx context.Context, revision DerivedIndexRevision, id int64) error {
	if err := validateGeneratedTable(revision.TableName); err != nil {
		return err
	}
	_, err := s.sql.ExecContext(ctx, `DELETE FROM `+revision.TableName+` WHERE rowid = ?`, id)
	return err
}

// WritableIndexRevisions returns live and building targets for a canonical kind.
func (s *Store) WritableIndexRevisions(ctx context.Context, entityKind string) ([]DerivedIndexRevision, error) {
	kinds := []string{IndexKindTranscriptFTS}
	if entityKind == "memory" {
		kinds = []string{IndexKindMemoryFTS, IndexKindMemoryVector}
	} else if entityKind == "global_memory" {
		kinds = []string{IndexKindGlobalMemoryFTS, IndexKindGlobalMemoryVector}
	}
	query := `SELECT id, revision, index_kind, model, dimension, schema_version, table_name, state, expected_count, indexed_count, created_at, updated_at FROM derived_index_revisions WHERE state IN ('live', 'building') AND index_kind IN (`
	args := make([]any, 0, len(kinds))
	for i, kind := range kinds {
		if i > 0 {
			query += ","
		}
		query += "?"
		args = append(args, kind)
	}
	query += `) ORDER BY index_kind, revision`
	rows, err := s.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []DerivedIndexRevision
	for rows.Next() {
		revision, err := scanIndexRevision(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// ClaimDerivedIndexChange leases the oldest durable change.
func (s *Store) ClaimDerivedIndexChange(ctx context.Context, lease time.Duration) (DerivedIndexChange, error) {
	if lease <= 0 {
		lease = time.Minute
	}
	owner, err := newLeaseOwner()
	if err != nil {
		return DerivedIndexChange{}, fmt.Errorf("create derived index lease owner: %w", err)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return DerivedIndexChange{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	var change DerivedIndexChange
	var userID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, canonical_user_id, entity_kind, entity_id, operation, attempt_count FROM durable_jobs WHERE job_kind = 'derived_index' AND ((state IN ('queued', 'retry') AND available_at <= ?) OR (state = 'running' AND lease_until <= ?)) ORDER BY id LIMIT 1`, formatTime(now), formatTime(now)).Scan(&change.Sequence, &userID, &change.EntityKind, &change.EntityID, &change.Operation, &change.AttemptCount)
	if err != nil {
		return DerivedIndexChange{}, err
	}
	change.UserID = userID.String
	result, err := tx.ExecContext(ctx, `UPDATE durable_jobs SET state = 'running', attempt_count = attempt_count + 1, lease_owner = ?, lease_until = ?, updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND ((state IN ('queued', 'retry') AND available_at <= ?) OR (state = 'running' AND lease_until <= ?))`, owner, formatTime(leaseUntil), formatTime(now), change.Sequence, formatTime(now), formatTime(now))
	if err != nil {
		return DerivedIndexChange{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return DerivedIndexChange{}, sql.ErrNoRows
	}
	change.AttemptCount++
	change.LeaseOwner = owner
	change.LeaseUntil = leaseUntil
	if err := tx.Commit(); err != nil {
		return DerivedIndexChange{}, err
	}
	return change, nil
}

// RenewDerivedIndexChangeLease extends an exactly owned, still-live outbox lease.
func (s *Store) RenewDerivedIndexChangeLease(ctx context.Context, change DerivedIndexChange, lease time.Duration) error {
	if lease <= 0 {
		return fmt.Errorf("renew derived index lease: duration must be positive")
	}
	now := time.Now().UTC()
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET lease_until = ?, updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND state = 'running' AND lease_owner = ? AND julianday(lease_until) > julianday(?)`, formatTime(now.Add(lease)), formatTime(now), change.Sequence, change.LeaseOwner, formatTime(now))
	return requireDerivedIndexLeaseMutation(result, err)
}

// CompleteDerivedIndexChange acknowledges a leased change.
func (s *Store) CompleteDerivedIndexChange(ctx context.Context, change DerivedIndexChange) error {
	now := formatTime(time.Now().UTC())
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'succeeded', completed_at = ?, lease_owner = '', lease_until = NULL, last_error_code = '', updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND state = 'running' AND lease_owner = ? AND julianday(lease_until) > julianday(?)`, now, now, change.Sequence, change.LeaseOwner, now)
	return requireDerivedIndexLeaseMutation(result, err)
}

// RetryDerivedIndexChange durably releases a failed change with backoff.
func (s *Store) RetryDerivedIndexChange(ctx context.Context, change DerivedIndexChange, code string) error {
	now := time.Now().UTC()
	delay := time.Duration(1<<min(change.AttemptCount, 6)) * time.Second
	result, err := s.sql.ExecContext(ctx, `UPDATE durable_jobs SET state = 'retry', available_at = ?, lease_owner = '', lease_until = NULL, last_error_code = ?, updated_at = ? WHERE id = ? AND job_kind = 'derived_index' AND state = 'running' AND lease_owner = ?`, formatTime(now.Add(delay)), safeErrorCode(code), formatTime(now), change.Sequence, change.LeaseOwner)
	return requireDerivedIndexLeaseMutation(result, err)
}

func requireDerivedIndexLeaseMutation(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrStaleDerivedIndexChangeLease
	}
	return nil
}

// ReconcileDerivedIndexChanges restores abandoned leases and inserts missing
// idempotent changes for every currently indexable canonical record.
func (s *Store) ReconcileDerivedIndexChanges(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.sql.ExecContext(ctx, `
UPDATE durable_jobs SET state = 'retry', available_at = ?, lease_owner = '', lease_until = NULL, updated_at = ? WHERE job_kind = 'derived_index' AND state = 'running' AND lease_until <= ?;
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'reconcile:memory:' || entity.id || ':' || entity.updated_at, entity.canonical_user_id, 'memory', entity.id, 'upsert', ?, ? FROM memory_entries entity WHERE entity.status = 'active' AND (entity.expires_at IS NULL OR entity.expires_at > ?) AND NOT EXISTS (SELECT 1 FROM durable_jobs receipt WHERE receipt.job_kind = 'derived_index' AND receipt.state = 'succeeded' AND receipt.operation = 'upsert' AND receipt.entity_kind = 'memory' AND receipt.entity_id = entity.id AND receipt.canonical_user_id = entity.canonical_user_id)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING;
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'reconcile:turn:' || turns.id || ':' || turns.delivered_at, turns.canonical_user_id, 'session_turn', turns.id, 'upsert', ?, ? FROM session_turns turns JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ? AND NOT EXISTS (SELECT 1 FROM durable_jobs receipt WHERE receipt.job_kind = 'derived_index' AND receipt.state = 'succeeded' AND receipt.operation = 'upsert' AND receipt.entity_kind = 'session_turn' AND receipt.entity_id = turns.id AND receipt.canonical_user_id = turns.canonical_user_id)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING;
INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at)
SELECT 'derived_index', 'reconcile:global_memory:' || entity.id || ':' || entity.created_at, NULL, 'global_memory', entity.id, 'upsert', ?, ? FROM global_memories entity WHERE NOT EXISTS (SELECT 1 FROM durable_jobs receipt WHERE receipt.job_kind = 'derived_index' AND receipt.state = 'succeeded' AND receipt.operation = 'upsert' AND receipt.entity_kind = 'global_memory' AND receipt.entity_id = entity.id AND receipt.canonical_user_id IS NULL)
ON CONFLICT(job_kind, idempotency_key) DO NOTHING;`, now, now, now, now, now, now, now, now, now, now, now)
	return err
}

// ValidateAndPublishIndexRevision rejects stale, orphaned, cross-tenant, or
// incomplete rows and atomically switches the live pointer on success.
func (s *Store) ValidateAndPublishIndexRevision(ctx context.Context, id int64) (DerivedIndexRevision, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return DerivedIndexRevision{}, err
	}
	defer tx.Rollback() // nolint:errcheck
	revision, err := scanIndexRevision(tx.QueryRowContext(ctx, `SELECT id, revision, index_kind, model, dimension, schema_version, table_name, state, expected_count, indexed_count, created_at, updated_at FROM derived_index_revisions WHERE id = ? AND state = 'building'`, id))
	if err != nil {
		return DerivedIndexRevision{}, err
	}
	if err := validateRevisionTableIdentity(revision); err != nil {
		return DerivedIndexRevision{}, err
	}
	if revision.Kind == IndexKindTranscriptFTS && revision.SchemaVersion != 2 {
		return DerivedIndexRevision{}, fmt.Errorf("derived transcript schema version mismatch: metadata=%d want=2", revision.SchemaVersion)
	}
	if revision.Kind == IndexKindMemoryVector || revision.Kind == IndexKindGlobalMemoryVector {
		if generatedIndexTable.MatchString(revision.TableName) && revision.SchemaVersion != 2 {
			return DerivedIndexRevision{}, fmt.Errorf("derived vector schema version mismatch: metadata=%d want=2", revision.SchemaVersion)
		}
		var definition string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, revision.TableName).Scan(&definition); err != nil {
			return DerivedIndexRevision{}, err
		}
		if dimension, ok := vectorDimensionFromSQL(definition); !ok || dimension != revision.Dimension {
			return DerivedIndexRevision{}, fmt.Errorf("derived vector dimension mismatch: metadata=%d table=%d", revision.Dimension, dimension)
		}
	}
	expectedQuery, validJoin := canonicalValidationSQL(revision)
	var expected, indexed, valid int64
	nowText := formatTime(time.Now().UTC())
	expectedArgs, validArgs := validationArgs(revision, nowText)
	if err := tx.QueryRowContext(ctx, expectedQuery, expectedArgs...).Scan(&expected); err != nil {
		return DerivedIndexRevision{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+revision.TableName).Scan(&indexed); err != nil {
		return DerivedIndexRevision{}, err
	}
	if err := tx.QueryRowContext(ctx, validJoin, validArgs...).Scan(&valid); err != nil {
		return DerivedIndexRevision{}, err
	}
	if expected != indexed || indexed != valid {
		return DerivedIndexRevision{}, fmt.Errorf("derived index validation failed: expected=%d indexed=%d valid=%d", expected, indexed, valid)
	}
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE derived_index_revisions SET state = 'retired', updated_at = ? WHERE index_kind = ? AND state = 'live'`, now, revision.Kind); err != nil {
		return DerivedIndexRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE derived_index_revisions SET state = 'live', expected_count = ?, indexed_count = ?, updated_at = ?, last_error_code = '' WHERE id = ? AND state = 'building'`, expected, indexed, now, id); err != nil {
		return DerivedIndexRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return DerivedIndexRevision{}, err
	}
	return s.indexRevisionByID(ctx, id)
}

func canonicalValidationSQL(revision DerivedIndexRevision) (string, string) {
	nowClause := `(entries.expires_at IS NULL OR entries.expires_at > ?)`
	expected := `SELECT COUNT(*) FROM memory_entries entries WHERE entries.status = 'active' AND ` + nowClause
	valid := `SELECT COUNT(*) FROM ` + revision.TableName + ` idx JOIN memory_entries entries ON entries.id = idx.rowid AND entries.canonical_user_id = idx.canonical_user_id WHERE entries.status = 'active' AND ` + nowClause + ` AND idx.statement = entries.statement AND idx.evidence = COALESCE((SELECT evidence FROM memory_candidates candidate WHERE candidate.canonical_user_id = entries.canonical_user_id AND candidate.published_memory_id = entries.id AND candidate.evidence != '' ORDER BY CASE candidate.provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, candidate.confidence DESC, candidate.id LIMIT 1), '')`
	if revision.Kind == IndexKindMemoryVector {
		valid = `SELECT COUNT(*) FROM ` + revision.TableName + ` idx JOIN memory_entries entries ON entries.id = idx.rowid AND entries.canonical_user_id = idx.canonical_user_id WHERE entries.status = 'active' AND ` + nowClause + ` AND idx.embedding_model = ?`
		if revision.SchemaVersion >= 2 {
			valid += ` AND idx.canonical_version = entries.updated_at AND idx.scope = entries.scope AND idx.category = entries.category`
		}
	}
	if revision.Kind == IndexKindTranscriptFTS {
		expected = `SELECT COUNT(*) FROM session_turns turns JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ?`
		valid = `SELECT COUNT(*) FROM ` + revision.TableName + ` idx JOIN session_turns turns ON turns.id = idx.rowid AND turns.canonical_user_id = idx.canonical_user_id AND turns.session_id = idx.session_id AND turns.session_generation = CAST(idx.session_generation AS INTEGER) JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ? AND idx.user_text = turns.user_text AND idx.assistant_text = turns.assistant_text AND idx.tool_text = turns.tool_search_text`
	}
	if revision.Kind == IndexKindGlobalMemoryFTS {
		expected = `SELECT COUNT(*) FROM global_memories`
		valid = `SELECT COUNT(*) FROM ` + revision.TableName + ` idx JOIN global_memories memories ON memories.id = idx.rowid WHERE idx.memory = memories.memory`
	}
	if revision.Kind == IndexKindGlobalMemoryVector {
		expected = `SELECT COUNT(*) FROM global_memories`
		valid = `SELECT COUNT(*) FROM ` + revision.TableName + ` idx JOIN global_memories memories ON memories.id = idx.rowid WHERE idx.embedding_model = ? AND idx.canonical_version = memories.created_at`
	}
	return expected, valid
}

func validationArgs(revision DerivedIndexRevision, now string) ([]any, []any) {
	if revision.Kind == IndexKindGlobalMemoryFTS {
		return nil, nil
	}
	if revision.Kind == IndexKindGlobalMemoryVector {
		return nil, []any{revision.Model}
	}
	valid := []any{now}
	if revision.Kind == IndexKindMemoryVector {
		valid = append(valid, revision.Model)
	}
	return []any{now}, valid
}

// FailIndexRevision records a failed shadow build without touching the live revision.
func (s *Store) FailIndexRevision(ctx context.Context, id int64, code string) error {
	now := formatTime(time.Now().UTC())
	_, err := s.sql.ExecContext(ctx, `UPDATE derived_index_revisions SET state = 'failed', updated_at = ?, last_error_code = ? WHERE id = ? AND state = 'building'`, now, safeErrorCode(code), id)
	return err
}

// IndexMaintenanceCounts reports aggregate derived-index repair totals.
type IndexMaintenanceCounts struct {
	RowsDeleted       int64
	RevisionsDegraded int64
	TablesDropped     int64
}

// MaintainDerivedIndexes removes non-canonical rows, verifies exact coverage,
// and drops only expired internally generated retired/failed tables.
func (s *Store) MaintainDerivedIndexes(ctx context.Context, now time.Time, retiredRetention time.Duration, batch int) (IndexMaintenanceCounts, error) {
	var counts IndexMaintenanceCounts
	defer func() {
		if counts.RevisionsDegraded > 0 {
			s.signalDerivedIndex()
		}
	}()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if batch <= 0 {
		batch = 100
	}
	revisions, err := s.liveIndexRevisions(ctx)
	if err != nil {
		return counts, err
	}
	for _, revision := range revisions {
		if err := validateGeneratedTable(revision.TableName); err != nil {
			return counts, err
		}
		nowText := formatTime(now)
		deleteSQL := `DELETE FROM ` + revision.TableName + ` WHERE rowid IN (SELECT idx.rowid FROM ` + revision.TableName + ` idx WHERE NOT EXISTS (SELECT 1 FROM memory_entries entries WHERE entries.id = idx.rowid AND entries.canonical_user_id = idx.canonical_user_id AND entries.status = 'active' AND (entries.expires_at IS NULL OR entries.expires_at > ?)) LIMIT ?)`
		args := []any{nowText, batch}
		if revision.Kind == IndexKindMemoryVector {
			deleteSQL = `DELETE FROM ` + revision.TableName + ` WHERE rowid IN (SELECT idx.rowid FROM ` + revision.TableName + ` idx WHERE idx.embedding_model != ? OR NOT EXISTS (SELECT 1 FROM memory_entries entries WHERE entries.id = idx.rowid AND entries.canonical_user_id = idx.canonical_user_id AND entries.status = 'active' AND (entries.expires_at IS NULL OR entries.expires_at > ?) AND (`
			if revision.SchemaVersion >= 2 {
				deleteSQL += `idx.canonical_version = entries.updated_at AND idx.scope = entries.scope AND idx.category = entries.category`
			} else {
				deleteSQL += `1`
			}
			deleteSQL += `)) LIMIT ?)`
			args = []any{revision.Model, nowText, batch}
		} else if revision.Kind == IndexKindTranscriptFTS {
			deleteSQL = `DELETE FROM ` + revision.TableName + ` WHERE rowid IN (SELECT idx.rowid FROM ` + revision.TableName + ` idx WHERE NOT EXISTS (SELECT 1 FROM session_turns turns JOIN sessions active ON active.canonical_user_id = turns.canonical_user_id AND active.session_id = turns.session_id AND active.generation = turns.session_generation WHERE turns.id = idx.rowid AND turns.canonical_user_id = idx.canonical_user_id AND turns.session_id = idx.session_id AND turns.session_generation = CAST(idx.session_generation AS INTEGER) AND turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL AND active.is_active = 1 AND active.expires_at > ?) LIMIT ?)`
			args = []any{nowText, batch}
		} else if revision.Kind == IndexKindGlobalMemoryFTS {
			deleteSQL = `DELETE FROM ` + revision.TableName + ` WHERE rowid IN (SELECT idx.rowid FROM ` + revision.TableName + ` idx WHERE NOT EXISTS (SELECT 1 FROM global_memories memories WHERE memories.id = idx.rowid) LIMIT ?)`
			args = []any{batch}
		} else if revision.Kind == IndexKindGlobalMemoryVector {
			deleteSQL = `DELETE FROM ` + revision.TableName + ` WHERE rowid IN (SELECT idx.rowid FROM ` + revision.TableName + ` idx WHERE idx.embedding_model != ? OR idx.canonical_version IS NOT COALESCE((SELECT memories.created_at FROM global_memories memories WHERE memories.id = idx.rowid), '') LIMIT ?)`
			args = []any{revision.Model, batch}
		}
		result, err := s.sql.ExecContext(ctx, deleteSQL, args...)
		if err != nil {
			if markErr := s.markLiveIndexUnhealthy(ctx, revision.ID, "physical_table_unavailable", nowText); markErr != nil {
				return counts, markErr
			}
			counts.RevisionsDegraded++
			continue
		}
		deleted, _ := result.RowsAffected()
		counts.RowsDeleted += deleted

		expectedSQL, validSQL := canonicalValidationSQL(revision)
		var expected, indexed, valid int64
		expectedArgs, validArgs := validationArgs(revision, nowText)
		if err := s.sql.QueryRowContext(ctx, expectedSQL, expectedArgs...).Scan(&expected); err != nil {
			return counts, err
		}
		if err := s.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+revision.TableName).Scan(&indexed); err != nil {
			if markErr := s.markLiveIndexUnhealthy(ctx, revision.ID, "physical_table_unavailable", nowText); markErr != nil {
				return counts, markErr
			}
			counts.RevisionsDegraded++
			continue
		}
		if err := s.sql.QueryRowContext(ctx, validSQL, validArgs...).Scan(&valid); err != nil {
			if markErr := s.markLiveIndexUnhealthy(ctx, revision.ID, "physical_table_corrupt", nowText); markErr != nil {
				return counts, markErr
			}
			counts.RevisionsDegraded++
			continue
		}
		if expected != indexed || indexed != valid {
			result, err := s.sql.ExecContext(ctx, `UPDATE derived_index_revisions SET expected_count = ?, indexed_count = ?, last_error_code = 'coverage_mismatch', updated_at = ? WHERE id = ? AND state = 'live'`, expected, indexed, nowText, revision.ID)
			if err != nil {
				return counts, err
			}
			changed, _ := result.RowsAffected()
			counts.RevisionsDegraded += changed
		} else if _, err := s.sql.ExecContext(ctx, `UPDATE derived_index_revisions SET expected_count = ?, indexed_count = ?, last_error_code = '', updated_at = ? WHERE id = ?`, expected, indexed, nowText, revision.ID); err != nil {
			return counts, err
		}
	}

	dropped, err := s.cleanupRetiredIndexTables(ctx, now, retiredRetention)
	counts.TablesDropped = dropped
	return counts, err
}

func (s *Store) liveIndexRevisions(ctx context.Context) ([]DerivedIndexRevision, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, revision, index_kind, model, dimension, schema_version, table_name, state, expected_count, indexed_count, created_at, updated_at FROM derived_index_revisions WHERE state = 'live' ORDER BY index_kind, revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []DerivedIndexRevision
	for rows.Next() {
		revision, err := scanIndexRevision(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) markLiveIndexUnhealthy(ctx context.Context, id int64, code, now string) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE derived_index_revisions SET last_error_code = ?, updated_at = ? WHERE id = ? AND state = 'live'`, safeErrorCode(code), now, id)
	return err
}

func (s *Store) cleanupRetiredIndexTables(ctx context.Context, now time.Time, retiredRetention time.Duration) (int64, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT revisions.id, revisions.revision, revisions.index_kind, revisions.table_name FROM derived_index_revisions revisions JOIN sqlite_master tables ON tables.type = 'table' AND tables.name = revisions.table_name WHERE revisions.state IN ('retired', 'failed') AND julianday(revisions.updated_at) <= julianday(?)`, formatTime(now.Add(-retiredRetention)))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type stale struct {
		id       int64
		revision int64
		kind     string
		table    string
	}
	var tables []stale
	for rows.Next() {
		var item stale
		if err := rows.Scan(&item.id, &item.revision, &item.kind, &item.table); err != nil {
			return 0, err
		}
		tables = append(tables, item)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var dropped int64
	for _, item := range tables {
		if err := validateRevisionTableIdentity(DerivedIndexRevision{ID: item.id, Revision: item.revision, Kind: item.kind, TableName: item.table}); err != nil || !generatedIndexTable.MatchString(item.table) {
			continue
		}
		if _, err := s.sql.ExecContext(ctx, `DROP TABLE IF EXISTS `+item.table); err != nil {
			return 0, err
		}
		dropped++
	}
	return dropped, nil
}

func enqueueDerivedChangeTx(ctx context.Context, tx *sql.Tx, userID, entityKind string, entityID int64, operation, token string) error {
	if entityID <= 0 || (entityKind != "memory" && entityKind != "session_turn") || (operation != "upsert" && operation != "delete") {
		return fmt.Errorf("invalid derived index change")
	}
	now := formatTime(time.Now().UTC())
	key := formationKey("derived-index", entityKind, entityID, operation, token)
	_, err := tx.ExecContext(ctx, `INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at) VALUES ('derived_index', ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(job_kind, idempotency_key) DO NOTHING`, key, userID, entityKind, entityID, operation, now, now)
	if err != nil {
		return fmt.Errorf("enqueue derived index change: %w", err)
	}
	return nil
}
