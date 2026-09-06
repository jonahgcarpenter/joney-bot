package global

import (
	"time"
)

// Memory is one administrator-curated global fact.
type Memory struct {
	ID        int64     `json:"id"`
	Text      string    `json:"memory"`
	CreatedAt time.Time `json:"created_at"`
}

// AddResult reports whether an add created a row or found an exact duplicate.
type AddResult struct {
	Memory    Memory
	Duplicate bool
}

// Page is one deterministic page of global memories.
type Page struct {
	Memories []Memory
	HasMore  bool
}

// SearchResult is one hybrid global-memory hit.
type SearchResult struct {
	Memory        Memory
	Score         float64
	LexicalScore  float64
	SemanticScore float64
	Sources       []string
}

// SearchStats reports independent retrieval-channel health without content.
type SearchStats struct {
	LexicalAvailable  bool
	SemanticAvailable bool
	LexicalError      error
	SemanticError     error
	LexicalCount      int
	SemanticCount     int
	SelectedCount     int
}
