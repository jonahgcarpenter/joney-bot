package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// SessionTurnWrite contains one pending exchange and its immutable artifacts.
type SessionTurnWrite struct {
	SessionID     string
	UserID        string
	Generation    int
	UserText      string
	AssistantText string
	ToolNames     []string
	History       ToolHistory
	Staged        []requestctx.StagedMemoryCandidate
	TTL           time.Duration
	Pressure      SessionPromptPressure
}

// AppendPendingSessionTurn atomically stores one pending exchange, native tool
// history, and staged memory inputs.
func (s *Store) AppendPendingSessionTurn(ctx context.Context, input SessionTurnWrite) (StoredSessionTurn, error) {
	pressure := input.Pressure
	if pressure.Tokens < 0 || pressure.Limit <= 0 || strings.TrimSpace(pressure.Version) == "" {
		return StoredSessionTurn{}, fmt.Errorf("append session turn: invalid compaction pressure")
	}
	pressure.Version = strings.TrimSpace(pressure.Version)
	return s.appendSessionTurnWithForegroundMemory(ctx, input.SessionID, input.UserID, input.Generation, input.UserText, input.AssistantText, input.ToolNames, input.History, input.Staged, input.TTL, &pressure)
}

func (s *Store) appendSessionTurnWithForegroundMemory(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, history ToolHistory, staged []requestctx.StagedMemoryCandidate, ttl time.Duration, pressure *SessionPromptPressure) (StoredSessionTurn, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(assistantText) == "" {
		return StoredSessionTurn{}, nil
	}
	if generation <= 0 {
		generation = 1
	}
	ttl = s.sessionTTL(ttl)
	if err := s.ensureAccountUser(userID); err != nil {
		return StoredSessionTurn{}, err
	}
	now := time.Now().UTC()
	requestID := requestctx.MetadataFromContext(ctx).RequestID
	var expires *time.Time
	if ttl > 0 {
		exp := now.Add(ttl).UTC()
		expires = &exp
	}
	var pressureTokens, pressureLimit, pressureVersion any
	if pressure != nil {
		pressureTokens, pressureLimit, pressureVersion = pressure.Tokens, pressure.Limit, pressure.Version
	}
	toolTrace, toolSearchText, err := EncodeToolHistory(history)
	if err != nil {
		return StoredSessionTurn{}, fmt.Errorf("append session turn: %w", err)
	}
	foregroundMemory, err := EncodeForegroundMemory(userID, staged)
	if err != nil {
		return StoredSessionTurn{}, fmt.Errorf("append session turn: %w", err)
	}
	if len(history.Batches) > 0 {
		toolNames = successfulToolHistoryNames(history)
	}
	query := `
INSERT INTO session_turns (session_id, canonical_user_id, session_generation, user_text, assistant_text, tool_names, tool_trace, tool_search_text, foreground_memory, created_at, expires_at, source_request_id, compaction_pressure_tokens, compaction_pressure_limit, compaction_pressure_version)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE EXISTS (
	SELECT 1 FROM sessions WHERE canonical_user_id = ? AND session_id = ? AND generation = ? AND is_active = 1
	)
	RETURNING id`
	args := []any{sessionID, userID, generation, strings.TrimSpace(userText), strings.TrimSpace(assistantText), strings.Join(uniqueStrings(toolNames), ","), toolTrace, toolSearchText, foregroundMemory, formatTime(now), nullableTime(expires), requestID, pressureTokens, pressureLimit, pressureVersion, userID, sessionID, generation}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return StoredSessionTurn{}, fmt.Errorf("begin session turn write: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	var id int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return StoredSessionTurn{}, nil
		}
		return StoredSessionTurn{}, fmt.Errorf("failed to append session turn: %w", err)
	}
	if err := enqueueDerivedChangeTx(ctx, tx, userID, "session_turn", id, "upsert", "append:"+formatTime(now)); err != nil {
		return StoredSessionTurn{}, err
	}
	if err := tx.Commit(); err != nil {
		return StoredSessionTurn{}, fmt.Errorf("commit session turn write: %w", err)
	}
	s.signalDerivedIndex()
	return StoredSessionTurn{ID: id, UserID: userID, SessionID: sessionID, Generation: generation, UserText: strings.TrimSpace(userText)}, nil
}
