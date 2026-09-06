package agent

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

// Agent tests inspect pending writes without pretending gateway delivery occurred.
type agentMemoryFixture struct {
	*memory.Store
	sql *sql.DB
}

func (s *agentMemoryFixture) RecentSessionTurns(userID, sessionID string, offset, count int) ([]memory.SessionTurn, error) {
	rows, err := s.sql.Query(`SELECT id, user_text, assistant_text, tool_names, tool_trace
		FROM session_turns WHERE canonical_user_id = ? AND session_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`, userID, sessionID, count, offset-1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []memory.SessionTurn
	for rows.Next() {
		turn := memory.SessionTurn{UserID: userID, SessionID: sessionID}
		var names, trace string
		if err := rows.Scan(&turn.ID, &turn.UserText, &turn.AssistantText, &names, &trace); err != nil {
			return nil, err
		}
		if names != "" {
			turn.ToolNames = strings.Split(names, ",")
		}
		turn.ToolHistory, err = memory.DecodeToolHistory(trace)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

func (s *agentMemoryFixture) AppendSessionTurn(ctx context.Context, sessionID, userID, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	profile, err := s.ResolveSessionProfile(ctx, userID, sessionID, ttl)
	if err != nil {
		return err
	}
	return memorytest.AppendDeliveredTurn(ctx, s.Store, sessionID, userID, profile.Generation, userText, assistantText, toolNames, ttl)
}

func (s *agentMemoryFixture) AppendSessionTurnForGeneration(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	return memorytest.AppendDeliveredTurn(ctx, s.Store, sessionID, userID, generation, userText, assistantText, toolNames, ttl)
}
