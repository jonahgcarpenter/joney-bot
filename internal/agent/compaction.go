package agent

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

const (
	foregroundCompactionStatus = "Compacting context..."
	foregroundDebtPageSize     = 1000
)

// ForegroundCompactor creates an in-memory session checkpoint for an active request.
type ForegroundCompactor interface {
	CompactForeground(context.Context, *memory.SessionSummary, []memory.SessionTurn, int) (memory.SummaryArtifact, error)
}

type foregroundCompactionState struct {
	compactor     ForegroundCompactor
	inputLimit    int
	prefix        []llm.ChatMessage
	current       llm.ChatMessage
	previous      *memory.SessionSummary
	debt          []memory.SessionTurn
	nextTurnID    int64
	currentInDebt bool
	stream        func(StreamChunk)
	lastArtifact  memory.SummaryArtifact
	hasCheckpoint bool
	imageContext  *llm.ChatMessage
}

type foregroundCompactionStats struct {
	Compacted       bool
	DebtCount       int
	EstimatedBefore int
	EstimatedAfter  int
}

func newForegroundCompactionState(compactor ForegroundCompactor, inputLimit int, deploymentPolicy, profileContent, currentPrompt string, currentImages []llm.InputImage, previous *memory.SessionSummary, debt []memory.SessionTurn, stream func(StreamChunk)) *foregroundCompactionState {
	prefix := []llm.ChatMessage{{Role: "system", Content: deploymentPolicy}}
	if profileContent != "" {
		prefix = append(prefix, llm.ChatMessage{Role: "user", Content: profileContent})
	}
	return &foregroundCompactionState{
		compactor: compactor, inputLimit: inputLimit, prefix: prefix,
		current:  llm.ChatMessage{Role: "user", Content: currentPrompt, Images: append([]llm.InputImage(nil), currentImages...)},
		previous: previous, debt: append([]memory.SessionTurn(nil), debt...), stream: stream, nextTurnID: -1,
	}
}

func (s *foregroundCompactionState) addToolBatch(batch memory.ToolHistoryBatch, currentPrompt string) {
	if s == nil || len(batch.Calls) == 0 {
		return
	}
	userText := "[Continuation of the active request]"
	if !s.currentInDebt {
		userText = currentPrompt
		s.currentInDebt = true
	}
	s.debt = append(s.debt, memory.SessionTurn{
		ID: s.nextTurnID, UserText: userText, ToolHistory: memory.ToolHistory{Version: memory.ToolHistoryVersion, Batches: []memory.ToolHistoryBatch{batch}},
	})
	s.nextTurnID--
}

func (s *foregroundCompactionState) hasDebt() bool {
	return s != nil && len(s.debt) > 0
}

func (s *foregroundCompactionState) prepare(ctx context.Context, messages []llm.ChatMessage, tools []llm.Tool, force bool) ([]llm.ChatMessage, foregroundCompactionStats, error) {
	stats := foregroundCompactionStats{EstimatedBefore: budget.EstimateRequest(messages, tools)}
	if s == nil || !s.hasDebt() || s.compactor == nil || s.inputLimit <= 0 {
		stats.EstimatedAfter = stats.EstimatedBefore
		return messages, stats, nil
	}
	if !force && stats.EstimatedBefore*100 < s.inputLimit*budget.CompactionTriggerPercent {
		stats.EstimatedAfter = stats.EstimatedBefore
		return messages, stats, nil
	}
	if s.stream != nil {
		s.stream(StreamChunk{Type: ChunkStatus, Text: foregroundCompactionStatus})
	}
	debtCount := len(s.debt)
	artifact, err := s.compactor.CompactForeground(ctx, s.previous, s.debt, s.inputLimit)
	if err != nil {
		return messages, stats, err
	}
	rendered := memory.RenderTransientSessionSummary(artifact)
	if rendered == "" {
		return messages, stats, fmt.Errorf("foreground compaction returned an empty checkpoint")
	}
	rebuilt := append([]llm.ChatMessage(nil), s.prefix...)
	rebuilt = append(rebuilt, llm.ChatMessage{Role: "user", Content: rendered}, s.current)
	if s.imageContext != nil {
		rebuilt = append(rebuilt, *s.imageContext)
	}
	s.previous = &memory.SessionSummary{
		Narrative: artifact.Narrative, OpenTasks: artifact.OpenTasks,
		Commitments: artifact.Commitments, Entities: artifact.Entities,
		Decisions: artifact.Decisions, TopicTags: artifact.TopicTags,
	}
	s.lastArtifact = artifact
	s.hasCheckpoint = true
	s.debt = nil
	stats.Compacted = true
	stats.DebtCount = debtCount
	stats.EstimatedAfter = budget.EstimateRequest(rebuilt, tools)
	return rebuilt, stats, nil
}

func loadForegroundDeliveredDebt(ctx context.Context, store *memory.Store, userID, sessionID string, generation int, afterTurnID int64) ([]memory.SessionTurn, error) {
	if store == nil || generation <= 0 {
		return nil, nil
	}
	var debt []memory.SessionTurn
	boundary := afterTurnID
	for {
		page, err := store.PageDeliveredSessionTurnsAfter(ctx, userID, sessionID, generation, boundary, foregroundDebtPageSize)
		if err != nil {
			return nil, err
		}
		debt = append(debt, page...)
		if len(page) == 0 {
			return debt, nil
		}
		boundary = page[len(page)-1].ID
	}
}
