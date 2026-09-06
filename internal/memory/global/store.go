// Package global owns administrator-curated facts shared across tenants.
package global

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const (
	MaxMemoryRunes     = 1000
	DefaultSearchLimit = 8
	MaxSearchLimit     = 20
	ListPageSize       = 25
	searchMinScore     = 0.30
	canonicalScanLimit = 500
)

// Store manages canonical global memory and its hybrid retrieval surfaces.
type Store struct {
	db         *database.DB
	sql        *sql.DB
	embedder   llm.Embedder
	embedModel string
	notify     func()
}

// NewStore opens the shared global-memory store.
func NewStore(dbPath string, embedder llm.Embedder, embeddingModel string, log *config.Logger) (*Store, error) {
	db, err := database.Open(dbPath, log)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, sql: db.SQL(), embedder: embedder, embedModel: strings.TrimSpace(embeddingModel)}, nil
}

// Close closes the store database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SetDerivedIndexNotifier installs the nonblocking index-worker wakeup.
func (s *Store) SetDerivedIndexNotifier(notify func()) { s.notify = notify }

// Add inserts one exact administrator-curated fact and rejects normalized duplicates.
func (s *Store) Add(ctx context.Context, text string) (AddResult, error) {
	text = NormalizeMemory(text)
	if err := validateMemory(text); err != nil {
		return AddResult{}, err
	}
	key := memoryKey(text)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return AddResult{}, fmt.Errorf("begin add global memory: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	now := formatTime(time.Now().UTC())
	result, err := tx.ExecContext(ctx, `INSERT INTO global_memories(memory, memory_key, created_at) VALUES (?, ?, ?) ON CONFLICT(memory_key) DO NOTHING`, text, key, now)
	if err != nil {
		return AddResult{}, fmt.Errorf("insert global memory: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		existing, err := memoryByKey(ctx, tx, key)
		if err != nil {
			return AddResult{}, err
		}
		return AddResult{Memory: existing, Duplicate: true}, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return AddResult{}, err
	}
	if err := enqueueIndexChange(ctx, tx, id, "upsert", now); err != nil {
		return AddResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AddResult{}, fmt.Errorf("commit global memory: %w", err)
	}
	if s.notify != nil {
		s.notify()
	}
	return AddResult{Memory: Memory{ID: id, Text: text, CreatedAt: parseTime(now)}}, nil
}

// List returns one page ordered by stable ID.
func (s *Store) List(ctx context.Context, page int) (Page, error) {
	if page <= 0 {
		return Page{}, fmt.Errorf("page must be a positive integer")
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT id, memory, created_at FROM global_memories ORDER BY id LIMIT ? OFFSET ?`, ListPageSize+1, (page-1)*ListPageSize)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	var result Page
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return Page{}, err
		}
		result.Memories = append(result.Memories, memory)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(result.Memories) > ListPageSize {
		result.HasMore = true
		result.Memories = result.Memories[:ListPageSize]
	}
	return result, nil
}

// Forget permanently removes one exact global memory.
func (s *Store) Forget(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("global memory ID must be a positive integer")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // nolint:errcheck
	var created string
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM global_memories WHERE id = ?`, id).Scan(&created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if err := enqueueIndexChange(ctx, tx, id, "delete", created); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM global_memories WHERE id = ?`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if s.notify != nil {
		s.notify()
	}
	return true, nil
}

func enqueueIndexChange(ctx context.Context, tx *sql.Tx, id int64, operation, version string) error {
	now := formatTime(time.Now().UTC())
	key := fmt.Sprintf("global_memory:%d:%s:%s", id, operation, version)
	_, err := tx.ExecContext(ctx, `INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at) VALUES ('derived_index', ?, NULL, 'global_memory', ?, ?, ?, ?) ON CONFLICT(job_kind, idempotency_key) DO NOTHING`, key, id, operation, now, now)
	if err != nil {
		return fmt.Errorf("enqueue global memory index change: %w", err)
	}
	return nil
}

func validateMemory(text string) error {
	if text == "" || utf8.RuneCountInString(text) > MaxMemoryRunes {
		return fmt.Errorf("global memory must contain 1..%d characters", MaxMemoryRunes)
	}
	return nil
}

// NormalizeMemory returns the canonical display form used for global facts.
func NormalizeMemory(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if r == utf8.RuneError || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func memoryKey(text string) string { return strings.ToLower(NormalizeMemory(text)) }

func memoryByKey(ctx context.Context, tx *sql.Tx, key string) (Memory, error) {
	return scanMemory(tx.QueryRowContext(ctx, `SELECT id, memory, created_at FROM global_memories WHERE memory_key = ?`, key))
}

func scanMemory(row interface{ Scan(...any) error }) (Memory, error) {
	var memory Memory
	var created string
	err := row.Scan(&memory.ID, &memory.Text, &created)
	memory.CreatedAt = parseTime(created)
	return memory, err
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
