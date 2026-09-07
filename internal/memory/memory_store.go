package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// SyncSpeakerIntro creates or updates the account-derived speaker intro.
func (s *Store) SyncSpeakerIntro(userID, intro string) error {
	if err := s.ensureAccountUser(userID); err != nil {
		return err
	}
	_, err := s.sql.Exec(`UPDATE account_users SET speaker_intro = ? WHERE canonical_user_id = ?`, strings.TrimSpace(intro), userID)
	if err != nil {
		return fmt.Errorf("failed to sync user memory intro for %q: %w", userID, err)
	}
	return nil
}

// ReadIntro returns the current speaker intro for a user.
func (s *Store) ReadIntro(userID string) (string, error) {
	var intro string
	err := s.sql.QueryRow(`SELECT speaker_intro FROM account_users WHERE canonical_user_id = ?`, userID).Scan(&intro)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read user memory intro for %q: %w", userID, err)
	}
	intro = strings.TrimSpace(intro)
	if !strings.HasPrefix(intro, "You are speaking with ") {
		return "", nil
	}
	return intro, nil
}

// EntryByID reads a memory entry by ID.
func (s *Store) EntryByID(id int64) (MemoryEntry, error) {
	rows, err := s.sql.Query(memoryEntrySelect+` WHERE memory.id = ?`, id)
	if err != nil {
		return MemoryEntry{}, err
	}
	defer rows.Close()
	if rows.Next() {
		return scanMemoryEntry(rows)
	}
	return MemoryEntry{}, sql.ErrNoRows
}

// Search returns active memories matching the requested filters.
func (s *Store) Search(ctx context.Context, userID, scope, category, query string, limit int) ([]MemoryEntry, error) {
	query = strings.TrimSpace(query)
	if query != "" {
		results, stats := s.Recall(ctx, userID, query, RecallRequest{Scope: scope, Category: category, TopK: limit, MinRelevance: defaultRecallMinRelevance, ExplicitSearch: true})
		if !stats.LexicalAvailable && !stats.SemanticAvailable {
			return nil, fmt.Errorf("durable memory retrieval indexes unavailable")
		}
		s.RecordRecallUsage(ctx, userID, results)
		return recallResultsToEntries(results), nil
	}
	return s.listActiveMemories(userID, scope, category, limit)
}

func (s *Store) listActiveMemories(userID, scope, category string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		limit = 8
	}
	if limit > 25 {
		limit = 25
	}
	normalizedScope := normalizeOptionalScope(scope)
	normalizedCategory := normalizeOptionalCategory(category)
	entries, err := s.activeEntries(userID, normalizedScope, normalizedCategory)
	if err != nil || len(entries) == 0 {
		return entries, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Importance == entries[j].Importance {
			return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
		}
		return entries[i].Importance > entries[j].Importance
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// ListMemories returns active memories without semantic ranking.
func (s *Store) ListMemories(userID, scope, category string, limit int) ([]MemoryEntry, error) {
	return s.Search(context.Background(), userID, scope, category, "", limit)
}

func memoryIDsTx(tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) activeEntries(userID, scope, category string) ([]MemoryEntry, error) {
	query := memoryEntrySelect + ` WHERE memory.canonical_user_id = ? AND memory.status = 'active' AND (memory.expires_at IS NULL OR memory.expires_at > ?)`
	args := []any{userID, formatTime(time.Now().UTC())}
	if scope != "" {
		query += ` AND scope = ?`
		args = append(args, scope)
	}
	if category != "" {
		query += ` AND category = ?`
		args = append(args, category)
	}
	query += ` ORDER BY importance DESC, updated_at DESC`
	rows, err := s.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read memories: %w", err)
	}
	defer rows.Close()
	entries := []MemoryEntry{}
	for rows.Next() {
		entry, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

const memoryEntrySelect = `SELECT memory.id, memory.canonical_user_id, memory.scope, memory.category, memory.statement,
	COALESCE((SELECT candidate.evidence FROM memory_candidates candidate WHERE candidate.canonical_user_id = memory.canonical_user_id AND candidate.published_memory_id = memory.id AND candidate.evidence != '' AND (memory.assessed_source_turn_id=0 OR candidate.source_turn_id=memory.assessed_source_turn_id) ORDER BY CASE candidate.provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, candidate.confidence DESC, candidate.id LIMIT 1), ''),
	memory.confidence, memory.importance, memory.status, memory.created_at, memory.updated_at, memory.expires_at, COALESCE(memory.supersedes_id, 0),
	memory.provenance_type, memory.sensitivity, memory.claim_slot, memory.claim_value,
	(SELECT COUNT(*) FROM memory_candidates candidate WHERE candidate.canonical_user_id = memory.canonical_user_id AND candidate.published_memory_id = memory.id), memory.revision,memory.assessment_context,memory.retired_at,memory.retirement_reason
FROM memory_entries memory`

func scanMemoryEntry(rows interface{ Scan(...any) error }) (MemoryEntry, error) {
	var entry MemoryEntry
	var created, updated string
	var expires sql.NullString
	var retired sql.NullString
	if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Scope, &entry.Category, &entry.Statement, &entry.Evidence, &entry.Confidence, &entry.Importance, &entry.Status, &created, &updated, &expires, &entry.SupersedesID, &entry.ProvenanceType, &entry.Sensitivity, &entry.ClaimSlot, &entry.ClaimValue, &entry.EvidenceCount, &entry.Revision, &entry.Context, &retired, &entry.RetirementReason); err != nil {
		return MemoryEntry{}, fmt.Errorf("failed to scan memory entry: %w", err)
	}
	if retired.Valid {
		entry.RetiredAt = parseTime(retired.String)
	}
	entry.CreatedAt = parseTime(created)
	entry.UpdatedAt = parseTime(updated)
	if expires.Valid {
		entry.ExpiresAt = parseTime(expires.String)
	}
	entry.SourceAuthority = sourceAuthorityForProvenance(entry.ProvenanceType)
	return entry, nil
}

func scanMemoryEntryWithDistance(rows interface{ Scan(...any) error }) (MemoryEntry, float64, error) {
	var entry MemoryEntry
	var created, updated string
	var expires sql.NullString
	var distance float64
	if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Scope, &entry.Category, &entry.Statement, &entry.Evidence, &entry.Confidence, &entry.Importance, &entry.Status, &created, &updated, &expires, &entry.SupersedesID, &entry.ProvenanceType, &entry.Sensitivity, &entry.ClaimSlot, &entry.ClaimValue, &entry.EvidenceCount, &entry.Revision, &entry.Context, &distance); err != nil {
		return MemoryEntry{}, 0, fmt.Errorf("failed to scan memory vector result: %w", err)
	}
	entry.CreatedAt = parseTime(created)
	entry.UpdatedAt = parseTime(updated)
	if expires.Valid {
		entry.ExpiresAt = parseTime(expires.String)
	}
	entry.SourceAuthority = sourceAuthorityForProvenance(entry.ProvenanceType)
	return entry, distance, nil
}

func vectorDimensionFromSQL(sqlText string) (int, bool) {
	if !strings.Contains(sqlText, "float[") {
		return 0, false
	}
	start := strings.Index(sqlText, "float[") + len("float[")
	end := strings.Index(sqlText[start:], "]")
	if end < 0 {
		return 0, false
	}
	var dim int
	if _, err := fmt.Sscanf(sqlText[start:start+end], "%d", &dim); err != nil || dim <= 0 {
		return 0, false
	}
	return dim, true
}

func serializeVector(values []float64) ([]byte, error) {
	vector := make([]float32, 0, len(values))
	for _, value := range values {
		vector = append(vector, float32(value))
	}
	serialized, err := sqlite_vec.SerializeFloat32(vector)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize embedding vector: %w", err)
	}
	return serialized, nil
}

func distanceToSimilarity(distance float64) float64 {
	if distance < 0 {
		distance = 0
	}
	return 1 / (1 + distance)
}

func (s *Store) embedWithModel(ctx context.Context, model, text string) ([]float64, error) {
	if s == nil || s.embedder == nil || strings.TrimSpace(model) == "" || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	resp, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: strings.TrimSpace(model), Input: strings.TrimSpace(text)})
	if err != nil {
		return nil, fmt.Errorf("embed durable memory query: %w", err)
	}
	if resp == nil || len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embed durable memory query: provider returned no vector")
	}
	return append([]float64(nil), resp.Embeddings[0]...), nil
}

func normalizeScope(scope string) string {
	scope = strings.TrimSpace(strings.ToLower(scope))
	scope = strings.ReplaceAll(scope, "-", "_")
	scope = strings.ReplaceAll(scope, " ", "_")
	if scope == ScopeLongTerm || scope == "long" || scope == "persistent" {
		return ScopeLongTerm
	}
	return ScopeShortTerm
}

func normalizeOptionalScope(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return ""
	}
	return normalizeScope(scope)
}

func normalizeCategory(cat string) string {
	cat = strings.TrimSpace(strings.ToLower(cat))
	cat = strings.ReplaceAll(cat, "-", "_")
	cat = strings.ReplaceAll(cat, " ", "_")
	for _, valid := range ValidCategories {
		if cat == valid {
			return cat
		}
	}
	return "notes"
}

func normalizeOptionalCategory(cat string) string {
	if strings.TrimSpace(cat) == "" {
		return ""
	}
	return normalizeCategory(cat)
}
