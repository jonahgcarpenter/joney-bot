package websearch

// SearchResult is one validated, normalized web search result.
type SearchResult struct {
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	Domain      string   `json:"domain"`
	Snippet     string   `json:"snippet"`
	Engines     []string `json:"engines"`
	PublishedAt string   `json:"published_at,omitempty"`
	Score       float64  `json:"score"`

	Category  string `json:"-"`
	Positions []int  `json:"-"`
}

// CandidateStats describes how much of a backend response was inspected and
// why candidates did not become results.
type CandidateStats struct {
	CandidateCount int
	InspectedCount int
	FilteredCount  int
	DuplicateCount int
}

// SearchResponse is the typed result of a search and the JSON tool envelope.
type SearchResponse struct {
	Notice              string         `json:"notice"`
	Degraded            bool           `json:"degraded"`
	UnresponsiveEngines []string       `json:"unresponsive_engines"`
	Results             []SearchResult `json:"results"`
	Stats               CandidateStats `json:"-"`
}
