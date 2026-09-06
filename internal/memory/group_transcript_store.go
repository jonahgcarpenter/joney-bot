package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SearchGroupTranscript searches newly shared public exchanges across participants
// in exactly one gateway/chat. All scope arguments must come from authenticated
// server context, not model input. Caller and source session lifetimes are fenced
// independently; source session identifiers and private tool history are omitted.
func (s *Store) SearchGroupTranscript(ctx context.Context, userID, sessionID string, generation int, gateway, chatID, query string, limit int) ([]TranscriptExcerpt, error) {
	userID, sessionID = strings.TrimSpace(userID), strings.TrimSpace(sessionID)
	if userID == "" || sessionID == "" || generation <= 0 || (gateway != "discord" && gateway != "imessage") || strings.TrimSpace(chatID) == "" || strings.TrimSpace(chatID) != chatID {
		return nil, fmt.Errorf("group transcript search requires valid caller and group scope")
	}
	terms := transcriptMatchQuery(query)
	if terms == "" {
		return nil, fmt.Errorf("transcript search query is required")
	}
	revision, err := s.LiveIndexRevision(ctx, IndexKindTranscriptFTS)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || revision.SchemaVersion != 3 || validateRevisionTableIdentity(revision) != nil {
		return nil, ErrTranscriptSearchUnavailable
	}
	if limit <= 0 {
		limit = defaultTranscriptSearchLimit
	}
	if limit > maxTranscriptSearchLimit {
		limit = maxTranscriptSearchLimit
	}
	table := revision.TableName
	match := fmt.Sprintf(`group_gateway : "%s" AND group_chat_id : "%s" AND {public_user_text assistant_text} : (%s)`, quoteTranscriptFTSValue(gateway), quoteTranscriptFTSValue(chatID), terms)
	now := formatTime(time.Now().UTC())
	rows, err := s.sql.QueryContext(ctx, `
SELECT turns.id, turns.canonical_user_id, turns.public_user_text, turns.assistant_text,
	turns.created_at, turns.delivered_at
FROM `+table+`
JOIN session_turns turns ON turns.id = `+table+`.rowid
JOIN sessions source ON source.canonical_user_id = turns.canonical_user_id
	AND source.session_id = turns.session_id AND source.generation = turns.session_generation
WHERE `+table+` MATCH ?
	AND turns.group_gateway = ? AND turns.group_chat_id = ?
	AND `+table+`.group_gateway = turns.group_gateway
	AND `+table+`.group_chat_id = turns.group_chat_id
	AND `+table+`.canonical_user_id = turns.canonical_user_id
	AND `+table+`.session_id = turns.session_id
	AND `+table+`.session_generation = turns.session_generation
	AND `+table+`.public_user_text = turns.public_user_text
	AND `+table+`.assistant_text = turns.assistant_text
	AND source.is_active = 1 AND julianday(source.expires_at) > julianday(?)
	AND turns.delivered_at IS NOT NULL AND turns.delivery_failed_at IS NULL
	AND EXISTS (SELECT 1 FROM sessions caller WHERE caller.canonical_user_id = ?
		AND caller.session_id = ? AND caller.generation = ? AND caller.is_active = 1
		AND julianday(caller.expires_at) > julianday(?))
ORDER BY bm25(`+table+`, 0.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0), turns.created_at DESC, turns.id DESC
LIMIT ?`, match, gateway, chatID, now, userID, sessionID, generation, now, maxTranscriptCandidateLimit)
	if err != nil {
		if transcriptFTSUnavailable(err) {
			return nil, ErrTranscriptSearchUnavailable
		}
		return nil, fmt.Errorf("search group transcript: %w", err)
	}
	defer rows.Close()
	results := make([]TranscriptExcerpt, 0, limit)
	usedBytes := 2
	for rows.Next() {
		var excerpt TranscriptExcerpt
		var publicText, answer string
		if err := rows.Scan(&excerpt.TurnID, &excerpt.CanonicalUserID, &publicText, &answer, &excerpt.CreatedAt, &excerpt.DeliveredAt); err != nil {
			return nil, fmt.Errorf("read group transcript result: %w", err)
		}
		// Construct only the two public messages, without decoding any private trace.
		excerpt.Records = []TranscriptRecord{{Role: "user", Content: publicText}, {Role: "assistant", Content: answer}}
		encoded, err := json.Marshal(excerpt)
		if err != nil {
			return nil, fmt.Errorf("measure group transcript result: %w", err)
		}
		separator := 0
		if len(results) > 0 {
			separator = 1
		}
		if usedBytes+separator+len(encoded) > maxTranscriptSearchChars {
			continue
		}
		results = append(results, excerpt)
		usedBytes += separator + len(encoded)
		if len(results) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read group transcript results: %w", err)
	}
	return results, nil
}
