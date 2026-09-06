package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	defaultRecallCandidateCap = 32
	maxRecallCandidateCap     = 100
)

var errVectorIndexIncompatible = errors.New("memory vector index has an incompatible dimension")

// RecallRequest controls tenant-scoped hybrid durable-memory retrieval.
type RecallRequest struct {
	Scope          string
	Category       string
	CandidateLimit int
	TopK           int
	MinRelevance   float64
	ExplicitSearch bool
}

// RecallStats summarizes candidate generation without exposing private text.
type RecallStats struct {
	LexicalCandidateCount  int
	SemanticCandidateCount int
	MergedCandidateCount   int
	BelowThresholdCount    int
	SelectedCount          int
	MinSelectedScore       float64
	MaxSelectedScore       float64
	LexicalAvailable       bool
	SemanticAvailable      bool
	LexicalError           error
	SemanticError          error
}

// Recall retrieves and ranks current-tenant durable memory. Lexical and
// semantic indexes are optional and fail independently.
func (s *Store) Recall(ctx context.Context, userID, query string, req RecallRequest) ([]RecallResult, RecallStats) {
	var stats RecallStats
	userID = strings.TrimSpace(userID)
	query = strings.TrimSpace(query)
	if userID == "" || query == "" {
		return nil, stats
	}
	candidateLimit := req.CandidateLimit
	if candidateLimit <= 0 {
		candidateLimit = defaultRecallCandidateCap
	}
	if candidateLimit > maxRecallCandidateCap {
		candidateLimit = maxRecallCandidateCap
	}
	scope := normalizeOptionalScope(req.Scope)
	category := normalizeOptionalCategory(req.Category)

	lexical, err := s.lexicalRecallCandidates(ctx, userID, scope, category, query, candidateLimit)
	if err != nil {
		stats.LexicalError = err
	} else {
		stats.LexicalAvailable = true
		stats.LexicalCandidateCount = len(lexical)
		if degraded, healthErr := s.LiveIndexDegraded(ctx, IndexKindMemoryFTS); healthErr != nil {
			stats.LexicalError = healthErr
		} else if degraded {
			stats.LexicalError = ErrDerivedIndexDegraded
		}
	}

	var semantic []RecallCandidate
	if s.embedModel != "" && s.embedder != nil {
		vectorRevision, revisionErr := s.LiveIndexRevision(ctx, IndexKindMemoryVector)
		if revisionErr != nil {
			stats.SemanticError = revisionErr
		} else {
			queryVector, embedErr := s.embedWithModel(ctx, vectorRevision.Model, query)
			if embedErr != nil {
				stats.SemanticError = embedErr
			} else if len(queryVector) > 0 {
				semantic, err = s.semanticRecallCandidates(ctx, vectorRevision, userID, scope, category, queryVector, candidateLimit)
				if err != nil {
					stats.SemanticError = err
				} else {
					stats.SemanticAvailable = true
					stats.SemanticCandidateCount = len(semantic)
					if degraded, healthErr := s.LiveIndexDegraded(ctx, IndexKindMemoryVector); healthErr != nil {
						stats.SemanticError = healthErr
					} else if degraded {
						stats.SemanticError = ErrDerivedIndexDegraded
					}
				}
			}
		}
	}

	candidates := append(lexical, semantic...)
	now := time.Now().UTC()
	beforeThreshold := RankDurableMemories(candidates, RecallOptions{
		Now:          now,
		TopK:         candidateLimit,
		MinRelevance: 0.000001,
	})
	ranked := RankDurableMemories(candidates, RecallOptions{
		Now:              now,
		TopK:             candidateLimit,
		MinRelevance:     req.MinRelevance,
		UncertaintyAware: !req.ExplicitSearch,
	})
	stats.MergedCandidateCount = len(beforeThreshold)
	stats.BelowThresholdCount = max(0, len(beforeThreshold)-len(ranked))
	topK := req.TopK
	if topK <= 0 {
		topK = defaultRecallTopK
	}
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}
	results := ranked
	stats.SelectedCount = len(results)
	if len(results) > 0 {
		stats.MaxSelectedScore = results[0].Score
		stats.MinSelectedScore = results[len(results)-1].Score
	}
	return results, stats
}

func (s *Store) lexicalRecallCandidates(ctx context.Context, userID, scope, category, queryText string, limit int) ([]RecallCandidate, error) {
	revision, err := s.LiveIndexRevision(ctx, IndexKindMemoryFTS)
	if err != nil {
		return nil, err
	}
	if err := validateGeneratedTable(revision.TableName); err != nil {
		return nil, err
	}
	terms := ftsRecallTerms(queryText)
	match := ftsTenantRecallQuery(userID, terms)
	if match == "" {
		return nil, nil
	}
	table := revision.TableName
	query := `
SELECT e.id, e.canonical_user_id, e.scope, e.category, e.statement, COALESCE((SELECT candidate.evidence FROM memory_candidates candidate WHERE candidate.canonical_user_id = e.canonical_user_id AND candidate.published_memory_id = e.id AND candidate.evidence != '' ORDER BY CASE candidate.provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, candidate.confidence DESC, candidate.id LIMIT 1), ''),
	e.confidence, e.importance, e.status, e.created_at,
	e.updated_at, e.expires_at, COALESCE(e.supersedes_id, 0),
	e.provenance_type, e.sensitivity,
	e.claim_slot, e.claim_value, (SELECT COUNT(*) FROM memory_candidates candidate WHERE candidate.canonical_user_id = e.canonical_user_id AND candidate.published_memory_id = e.id),
	bm25(` + table + `, 0.0, 1.0, 0.5)
FROM ` + table + `
JOIN memory_entries e ON e.id = ` + table + `.rowid
WHERE ` + table + ` MATCH ?
	AND ` + table + `.canonical_user_id = ?
	AND e.canonical_user_id = ?
	AND e.status = 'active'
	AND (e.expires_at IS NULL OR e.expires_at > ?)`
	args := []any{match, userID, userID, formatTime(time.Now().UTC())}
	if scope != "" {
		query += ` AND e.scope = ?`
		args = append(args, scope)
	}
	if category != "" {
		query += ` AND e.category = ?`
		args = append(args, category)
	}
	query += ` ORDER BY bm25(` + table + `, 0.0, 1.0, 0.5), e.id LIMIT ?`
	args = append(args, limit)

	rows, err := s.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search durable memory FTS: %w", err)
	}
	defer rows.Close()
	var candidates []RecallCandidate
	for rows.Next() {
		entry, _, err := scanMemoryEntryWithDistance(rows)
		if err != nil {
			return nil, err
		}
		rank := len(candidates)
		candidates = append(candidates, RecallCandidate{
			Entry: entry, Source: RecallSourceLexical,
			Relevance: lexicalRecallCoverage(entry.Statement+" "+entry.Evidence, terms) / (1 + 0.05*float64(rank)),
			Authority: recallAuthorityForEntry(entry),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read durable memory FTS results: %w", err)
	}
	return candidates, nil
}

func (s *Store) semanticRecallCandidates(ctx context.Context, revision DerivedIndexRevision, userID, scope, category string, queryVector []float64, limit int) ([]RecallCandidate, error) {
	if revision.Dimension != len(queryVector) {
		return nil, fmt.Errorf("%w: index=%d query=%d", errVectorIndexIncompatible, revision.Dimension, len(queryVector))
	}
	if err := validateGeneratedTable(revision.TableName); err != nil {
		return nil, err
	}
	serialized, err := serializeVector(queryVector)
	if err != nil {
		return nil, err
	}
	query := `
SELECT e.id, e.canonical_user_id, e.scope, e.category, e.statement, COALESCE((SELECT candidate.evidence FROM memory_candidates candidate WHERE candidate.canonical_user_id = e.canonical_user_id AND candidate.published_memory_id = e.id AND candidate.evidence != '' ORDER BY CASE candidate.provenance_type WHEN 'user_statement' THEN 3 WHEN 'model_inference' THEN 2 ELSE 1 END DESC, candidate.confidence DESC, candidate.id LIMIT 1), ''),
	e.confidence, e.importance, e.status, e.created_at,
	e.updated_at, e.expires_at, COALESCE(e.supersedes_id, 0),
	e.provenance_type, e.sensitivity,
	e.claim_slot, e.claim_value, (SELECT COUNT(*) FROM memory_candidates candidate WHERE candidate.canonical_user_id = e.canonical_user_id AND candidate.published_memory_id = e.id), v.distance
FROM ` + revision.TableName + ` v
JOIN memory_entries e ON e.id = v.rowid
WHERE v.embedding MATCH ? AND v.k = ?
	AND v.canonical_user_id = ? AND v.embedding_model = ?
	AND e.canonical_user_id = ? AND e.status = 'active'
	AND (e.expires_at IS NULL OR e.expires_at > ?)`
	args := []any{serialized, limit, userID, revision.Model, userID, formatTime(time.Now().UTC())}
	if scope != "" {
		query += ` AND v.scope = ? AND e.scope = ?`
		args = append(args, scope)
		args = append(args, scope)
	}
	if category != "" {
		query += ` AND v.category = ? AND e.category = ?`
		args = append(args, category)
		args = append(args, category)
	}
	query += ` ORDER BY v.distance, e.id LIMIT ?`
	args = append(args, limit)
	rows, err := s.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search tenant memory vectors: %w", err)
	}
	defer rows.Close()
	var candidates []RecallCandidate
	for rows.Next() {
		entry, distance, err := scanMemoryEntryWithDistance(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, RecallCandidate{
			Entry: entry, Source: RecallSourceSemantic,
			Relevance: distanceToSimilarity(distance),
			Authority: recallAuthorityForEntry(entry),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tenant memory vector results: %w", err)
	}
	return candidates, nil
}

func ftsRecallTerms(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		field = strings.ToLower(strings.TrimSpace(field))
		if field == "" || recallStopWords[field] {
			continue
		}
		if _, ok := seen[field]; ok {
			continue
		}
		seen[field] = struct{}{}
		terms = append(terms, field)
	}
	return terms
}

func quotedFTSTerms(terms []string) []string {
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return quoted
}

func ftsTenantRecallQuery(userID string, terms []string) string {
	if len(terms) == 0 {
		return ""
	}
	tenant := `"` + strings.ReplaceAll(strings.ToLower(userID), `"`, `""`) + `"`
	return `canonical_user_id : ` + tenant + ` AND {statement evidence} : (` + strings.Join(quotedFTSTerms(terms), " OR ") + `)`
}

func recallAuthorityForEntry(entry MemoryEntry) RecallAuthority {
	if entry.ProvenanceType == "model_inference" {
		return RecallAuthorityInferred
	}
	switch entry.ProvenanceType {
	case "user_statement":
		return RecallAuthorityUserStated
	}
	return RecallAuthorityUnknown
}

func lexicalRecallCoverage(text string, terms []string) float64 {
	if len(terms) == 0 {
		return 0
	}
	tokens := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		tokens[token] = struct{}{}
	}
	matched := 0
	for _, term := range terms {
		if _, ok := tokens[term]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(terms))
}

var recallStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "do": true, "does": true, "for": true, "from": true, "how": true,
	"i": true, "in": true, "is": true, "it": true, "me": true, "my": true,
	"of": true, "on": true, "or": true, "that": true, "the": true, "this": true,
	"to": true, "user": true, "was": true, "what": true, "when": true, "where": true,
	"which": true, "who": true, "why": true, "with": true, "you": true, "your": true,
}

// RecordRecallUsage retains the retrieval API without persisting per-read state.
func (s *Store) RecordRecallUsage(ctx context.Context, userID string, results []RecallResult) {
}

func recallResultsToEntries(results []RecallResult) []MemoryEntry {
	entries := make([]MemoryEntry, 0, len(results))
	for _, result := range results {
		entry := result.Entry
		entry.Score = result.Score
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
	return entries
}
