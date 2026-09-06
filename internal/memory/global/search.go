package global

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

var generatedGlobalIndexTable = regexp.MustCompile(`^derived_index_global_memory_(fts|vector)_r[1-9][0-9]*$`)

var searchStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "does": true, "for": true,
	"how": true, "in": true, "is": true, "of": true, "on": true, "the": true,
	"to": true, "use": true, "uses": true, "what": true, "which": true, "with": true,
}

// Search runs independently degradable lexical and semantic retrieval.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchResult, SearchStats) {
	var stats SearchStats
	query = NormalizeMemory(query)
	if query == "" {
		return nil, stats
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	candidateLimit := max(limit*4, 24)
	lexical, err := s.lexicalCandidates(ctx, query, candidateLimit)
	if err != nil {
		stats.LexicalError = err
	} else {
		stats.LexicalAvailable = true
		stats.LexicalCount = len(lexical)
	}
	semantic, err := s.semanticCandidates(ctx, query, candidateLimit)
	if err != nil {
		stats.SemanticError = err
	} else if s.embedder != nil && s.embedModel != "" {
		stats.SemanticAvailable = true
		stats.SemanticCount = len(semantic)
	}
	if !stats.LexicalAvailable && !stats.SemanticAvailable {
		fallback, fallbackErr := s.canonicalCandidates(ctx, query, candidateLimit)
		if fallbackErr == nil {
			lexical = fallback
			stats.LexicalAvailable = true
			stats.LexicalCount = len(lexical)
		} else {
			stats.LexicalError = errors.Join(stats.LexicalError, fallbackErr)
		}
	}
	merged := make(map[int64]*SearchResult, len(lexical)+len(semantic))
	for _, candidate := range append(lexical, semantic...) {
		current := merged[candidate.Memory.ID]
		if current == nil {
			copy := candidate
			current = &copy
			merged[candidate.Memory.ID] = current
		} else {
			current.LexicalScore = max(current.LexicalScore, candidate.LexicalScore)
			current.SemanticScore = max(current.SemanticScore, candidate.SemanticScore)
		}
	}
	results := make([]SearchResult, 0, len(merged))
	for _, result := range merged {
		switch {
		case stats.LexicalAvailable && stats.SemanticAvailable:
			result.Score = 0.35*result.LexicalScore + 0.65*result.SemanticScore
		case stats.SemanticAvailable:
			result.Score = result.SemanticScore
		default:
			result.Score = result.LexicalScore
		}
		if result.LexicalScore > 0 {
			result.Sources = append(result.Sources, "lexical")
		}
		if result.SemanticScore > 0 {
			result.Sources = append(result.Sources, "semantic")
		}
		if result.Score >= searchMinScore {
			results = append(results, *result)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Memory.ID < results[j].Memory.ID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	stats.SelectedCount = len(results)
	return results, stats
}

func (s *Store) lexicalCandidates(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	revision, err := s.liveRevision(ctx, "global_memory_fts")
	if err != nil {
		return nil, err
	}
	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	match := make([]string, 0, len(terms))
	for _, term := range terms {
		match = append(match, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT memories.id, memories.memory, memories.created_at FROM `+revision.table+` idx JOIN global_memories memories ON memories.id = idx.rowid WHERE `+revision.table+` MATCH ? ORDER BY bm25(`+revision.table+`), memories.id LIMIT ?`, strings.Join(match, " OR "), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SearchResult
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, SearchResult{Memory: memory, LexicalScore: tokenCoverage(memory.Text, terms)})
	}
	return results, rows.Err()
}

func (s *Store) semanticCandidates(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if s.embedder == nil || s.embedModel == "" {
		return nil, nil
	}
	revision, err := s.liveRevision(ctx, "global_memory_vector")
	if err != nil {
		return nil, err
	}
	response, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: revision.model, Input: query})
	if err != nil {
		return nil, err
	}
	if response == nil || len(response.Embeddings) == 0 || len(response.Embeddings[0]) != revision.dimension {
		return nil, fmt.Errorf("global memory query embedding dimension mismatch")
	}
	serialized, err := json.Marshal(response.Embeddings[0])
	if err != nil {
		return nil, err
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT memories.id, memories.memory, memories.created_at, idx.distance FROM `+revision.table+` idx JOIN global_memories memories ON memories.id = idx.rowid WHERE idx.embedding MATCH ? AND idx.k = ? AND idx.embedding_model = ? ORDER BY idx.distance, memories.id LIMIT ?`, string(serialized), limit, revision.model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SearchResult
	for rows.Next() {
		var memory Memory
		var created string
		var distance float64
		if err := rows.Scan(&memory.ID, &memory.Text, &created, &distance); err != nil {
			return nil, err
		}
		memory.CreatedAt = parseTime(created)
		results = append(results, SearchResult{Memory: memory, SemanticScore: 1 / (1 + max(0, distance))})
	}
	return results, rows.Err()
}

func (s *Store) canonicalCandidates(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	terms := searchTerms(query)
	rows, err := s.sql.QueryContext(ctx, `SELECT id, memory, created_at FROM global_memories ORDER BY id DESC LIMIT ?`, canonicalScanLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SearchResult
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		if score := tokenCoverage(memory.Text, terms); score > 0 {
			results = append(results, SearchResult{Memory: memory, LexicalScore: score})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].LexicalScore != results[j].LexicalScore {
			return results[i].LexicalScore > results[j].LexicalScore
		}
		return results[i].Memory.ID < results[j].Memory.ID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, rows.Err()
}

type liveIndexRevision struct {
	table     string
	model     string
	dimension int
}

func (s *Store) liveRevision(ctx context.Context, kind string) (liveIndexRevision, error) {
	var revision liveIndexRevision
	var healthCode string
	if err := s.sql.QueryRowContext(ctx, `SELECT table_name, model, dimension, last_error_code FROM derived_index_revisions WHERE index_kind = ? AND state = 'live'`, kind).Scan(&revision.table, &revision.model, &revision.dimension, &healthCode); err != nil {
		return revision, err
	}
	if healthCode != "" {
		return liveIndexRevision{}, fmt.Errorf("global derived index is degraded: %s", healthCode)
	}
	if !generatedGlobalIndexTable.MatchString(revision.table) {
		return liveIndexRevision{}, fmt.Errorf("invalid global derived-index table")
	}
	return revision, nil
}

func searchTerms(value string) []string {
	fields := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	seen := make(map[string]struct{}, len(fields))
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if len([]rune(field)) < 2 || searchStopWords[field] {
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

func tokenCoverage(text string, terms []string) float64 {
	if len(terms) == 0 {
		return 0
	}
	words := make(map[string]struct{})
	for _, word := range searchTerms(text) {
		words[word] = struct{}{}
	}
	matches := 0
	for _, term := range terms {
		if _, ok := words[term]; ok {
			matches++
		}
	}
	return float64(matches) / float64(len(terms))
}
