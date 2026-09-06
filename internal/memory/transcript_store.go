package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	defaultTranscriptSearchLimit = 5
	maxTranscriptSearchLimit     = 10
	maxTranscriptCandidateLimit  = 50
	maxTranscriptSearchChars     = 12000
	maxTranscriptQueryTerms      = 16
	maxTranscriptQueryTermRunes  = 64
)

// ErrTranscriptSearchUnavailable is returned when the transcript FTS index is
// not available in the current SQLite build or database.
var ErrTranscriptSearchUnavailable = errors.New("transcript search unavailable")

// TranscriptRecord is one role-preserving message in a historical exchange.
type TranscriptRecord struct {
	Role       string               `json:"role"`
	Content    string               `json:"content,omitempty"`
	ToolCalls  []TranscriptToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	ToolName   string               `json:"tool_name,omitempty"`
}

// TranscriptToolCall is one historical native tool invocation.
type TranscriptToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// TranscriptExcerpt is one complete historical exchange with source provenance.
type TranscriptExcerpt struct {
	SessionID         string             `json:"session_id"`
	SessionGeneration int                `json:"session_generation"`
	TurnID            int64              `json:"turn_id"`
	CreatedAt         string             `json:"created_at"`
	DeliveredAt       string             `json:"delivered_at"`
	Records           []TranscriptRecord `json:"records"`
}

// SearchTranscript searches delivered turns in exactly the active generation
// of the authorized tenant session. Scope values must come from server context.
func (s *Store) SearchTranscript(ctx context.Context, userID, sessionID string, generation int, queryText string, limit int) ([]TranscriptExcerpt, error) {
	userID = strings.TrimSpace(userID)
	sessionID = strings.TrimSpace(sessionID)
	terms := transcriptMatchQuery(queryText)
	if userID == "" || sessionID == "" || generation <= 0 {
		return nil, fmt.Errorf("transcript search requires user, session, and generation")
	}
	if terms == "" {
		return nil, fmt.Errorf("transcript search query is required")
	}
	revision, err := s.LiveIndexRevision(ctx, IndexKindTranscriptFTS)
	if err != nil || validateGeneratedTable(revision.TableName) != nil {
		return nil, ErrTranscriptSearchUnavailable
	}
	table := revision.TableName
	match := fmt.Sprintf(`canonical_user_id : "%s" AND session_id : "%s" AND session_generation : "%d" AND {user_text assistant_text tool_text} : (%s)`, quoteTranscriptFTSValue(userID), quoteTranscriptFTSValue(sessionID), generation, terms)
	if limit <= 0 {
		limit = defaultTranscriptSearchLimit
	}
	if limit > maxTranscriptSearchLimit {
		limit = maxTranscriptSearchLimit
	}

	now := formatTime(time.Now().UTC())
	rows, err := s.sql.QueryContext(ctx, `
SELECT turns.id, turns.session_id, turns.session_generation, turns.user_text,
	turns.assistant_text, turns.tool_trace, turns.created_at, turns.delivered_at
FROM `+table+`
JOIN session_turns turns ON turns.id = `+table+`.rowid
JOIN sessions
	ON sessions.canonical_user_id = turns.canonical_user_id
	AND sessions.session_id = turns.session_id
WHERE `+table+` MATCH ?
	AND `+table+`.canonical_user_id = ?
	AND `+table+`.session_id = ?
	AND `+table+`.session_generation = ?
	AND turns.canonical_user_id = ?
	AND turns.session_id = ?
	AND turns.session_generation = ?
	AND sessions.canonical_user_id = ?
	AND sessions.session_id = ?
	AND sessions.generation = ?
	AND sessions.is_active = 1
	AND julianday(sessions.expires_at) > julianday(?)
	AND turns.delivered_at IS NOT NULL
	AND turns.delivery_failed_at IS NULL
ORDER BY bm25(`+table+`, 0.0, 0.0, 0.0, 1.0, 1.0, 0.75), turns.created_at DESC, turns.id DESC
LIMIT ?`, match, userID, sessionID, generation, userID, sessionID, generation,
		userID, sessionID, generation, now, maxTranscriptCandidateLimit)
	if err != nil {
		if transcriptFTSUnavailable(err) {
			return nil, ErrTranscriptSearchUnavailable
		}
		return nil, fmt.Errorf("search session transcript: %w", err)
	}
	defer rows.Close()

	results := make([]TranscriptExcerpt, 0, limit)
	usedChars := 2 // JSON array delimiters used by the handler.
	for rows.Next() {
		var excerpt TranscriptExcerpt
		var userText, assistantText, toolTrace string
		if err := rows.Scan(&excerpt.TurnID, &excerpt.SessionID, &excerpt.SessionGeneration, &userText, &assistantText, &toolTrace, &excerpt.CreatedAt, &excerpt.DeliveredAt); err != nil {
			return nil, fmt.Errorf("read session transcript result: %w", err)
		}
		history, err := DecodeToolHistory(toolTrace)
		if err != nil {
			return nil, err
		}
		excerpt.Records = transcriptRecords(excerpt.TurnID, userText, assistantText, history)
		encoded, err := json.Marshal(excerpt)
		if err != nil {
			return nil, fmt.Errorf("measure session transcript result: %w", err)
		}
		separatorChars := 0
		if len(results) > 0 {
			separatorChars = 1
		}
		if usedChars+separatorChars+len(encoded) > maxTranscriptSearchChars {
			encoded, err = boundTranscriptExcerpt(&excerpt, maxTranscriptSearchChars-usedChars-separatorChars)
			if err != nil || len(encoded) == 0 {
				continue
			}
		}
		results = append(results, excerpt)
		usedChars += separatorChars + len(encoded)
		if len(results) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read session transcript results: %w", err)
	}
	return results, nil
}

func boundTranscriptExcerpt(excerpt *TranscriptExcerpt, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, nil
	}
	for i := range excerpt.Records {
		if excerpt.Records[i].Role != "tool" {
			continue
		}
		if len([]rune(excerpt.Records[i].Content)) > 512 {
			excerpt.Records[i].Content = truncateRunes(excerpt.Records[i].Content, 512) + " [truncated]"
		}
	}
	encoded, err := json.Marshal(excerpt)
	if err != nil || len(encoded) <= maxBytes {
		return encoded, err
	}
	for i := range excerpt.Records {
		if excerpt.Records[i].Role == "tool" {
			excerpt.Records[i].Content = "Historical tool result omitted from this bounded transcript excerpt."
		}
		for j := range excerpt.Records[i].ToolCalls {
			excerpt.Records[i].ToolCalls[j].Arguments = map[string]interface{}{}
		}
	}
	encoded, err = json.Marshal(excerpt)
	if err != nil || len(encoded) > maxBytes {
		return nil, err
	}
	return encoded, nil
}

func transcriptRecords(turnID int64, userText, assistantText string, history ToolHistory) []TranscriptRecord {
	records := []TranscriptRecord{{Role: "user", Content: userText}}
	for batchIndex, batch := range history.Batches {
		assistant := TranscriptRecord{Role: "assistant", Content: batch.AssistantContent}
		for callIndex, call := range batch.Calls {
			id := fmt.Sprintf("hist_%d_%d_%d", turnID, batchIndex+1, callIndex+1)
			assistant.ToolCalls = append(assistant.ToolCalls, TranscriptToolCall{ID: id, Name: call.Name, Arguments: call.Arguments})
		}
		records = append(records, assistant)
		for callIndex, call := range batch.Calls {
			id := fmt.Sprintf("hist_%d_%d_%d", turnID, batchIndex+1, callIndex+1)
			records = append(records, TranscriptRecord{Role: "tool", Content: call.Result, ToolCallID: id, ToolName: call.Name})
		}
	}
	return append(records, TranscriptRecord{Role: "assistant", Content: assistantText})
}

func quoteTranscriptFTSValue(value string) string {
	return strings.ReplaceAll(value, `"`, `""`)
}

func transcriptMatchQuery(queryText string) string {
	var terms []string
	var term []rune
	flush := func() {
		if len(term) == 0 || len(terms) == maxTranscriptQueryTerms {
			term = term[:0]
			return
		}
		terms = append(terms, `"`+strings.ReplaceAll(string(term), `"`, `""`)+`"`)
		term = term[:0]
	}
	for _, r := range strings.TrimSpace(queryText) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' {
			if len(term) < maxTranscriptQueryTermRunes {
				term = append(term, r)
			}
			continue
		}
		flush()
		if len(terms) == maxTranscriptQueryTerms {
			break
		}
	}
	flush()
	return strings.Join(terms, " OR ")
}

func transcriptFTSUnavailable(err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such table: derived_index_transcript_fts_") ||
		strings.Contains(message, "no such module: fts5") ||
		strings.Contains(message, "unable to use function match")
}
