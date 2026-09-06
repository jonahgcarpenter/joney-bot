// Package memorytest provides fixtures for tests outside the memory package.
package memorytest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
)

// NewStore opens an isolated test-owned store and registers its cleanup.
func NewStore(t testing.TB, path string, log *config.Logger) *memory.Store {
	t.Helper()
	store, err := memory.NewSQLiteStore(path, nil, "", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// MemoryFixture describes an already-validated observation for publication tests.
type MemoryFixture struct {
	Scope, Category, Statement, Evidence string
	Confidence                           float64
	Importance                           int
	TTL                                  time.Duration
	Provenance                           policy.Provenance
}

var memorySequence atomic.Uint64

// AppendPendingTurn writes a generation-fenced exchange with an inert fixture pressure snapshot.
func AppendPendingTurn(ctx context.Context, store *memory.Store, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) (memory.StoredSessionTurn, error) {
	return store.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{
		SessionID: sessionID, UserID: userID, Generation: generation,
		UserText: userText, AssistantText: assistantText, ToolNames: toolNames,
		History: memory.EmptyToolHistory(), TTL: ttl,
		Pressure: memory.SessionPromptPressure{Tokens: 0, Limit: 100000, Version: "test-fixture"},
	})
}

// AppendDeliveredTurn records delivery explicitly after the generation-fenced write.
func AppendDeliveredTurn(ctx context.Context, store *memory.Store, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	turn, err := AppendPendingTurn(ctx, store, sessionID, userID, generation, userText, assistantText, toolNames, ttl)
	if err != nil || turn.ID == 0 {
		return err
	}
	return store.MarkSessionTurnDelivered(ctx, userID, turn.ID)
}

// PublishMemory uses canonical candidate publication, not a parallel SQL writer.
func PublishMemory(ctx context.Context, store *memory.Store, userID string, req MemoryFixture) (memory.MemoryEntry, error) {
	if req.Category == "" {
		req.Category = "notes"
	}
	if req.Scope == "" {
		req.Scope = memory.ScopeLongTerm
	}
	if req.Confidence == 0 {
		req.Confidence = 0.8
	}
	if req.Importance == 0 {
		req.Importance = 3
	}
	if req.Evidence == "" {
		req.Evidence = req.Statement
	}
	if req.Provenance == "" {
		req.Provenance = policy.ProvenanceUserStatement
	}
	slot, value := policy.NormalizeClaimIdentity(policy.Category(req.Category), "", "", req.Statement)
	candidate, _, err := store.ProposeCandidate(ctx, userID, memory.CandidateProposal{
		IdempotencyKey: fmt.Sprintf("fixture:%d", memorySequence.Add(1)),
		Source:         memory.FormationSource{ExtractorVersion: memory.AgentSaveExtractorVersion},
		Output: policy.CandidateOutput{
			Scope: policy.Scope(req.Scope), Category: policy.Category(req.Category),
			Statement: req.Statement, Evidence: req.Evidence, Confidence: req.Confidence,
			Importance: req.Importance, TTL: req.TTL, Provenance: req.Provenance,
			Sensitivity: policy.SensitivityLow, Mode: policy.ModeAgentSave,
			Approval: policy.ApprovalApproved, Decision: policy.DecisionAutomatic,
			ClaimSlot: slot, ClaimValue: value,
		},
	})
	if err != nil {
		return memory.MemoryEntry{}, err
	}
	if candidate.PublishedMemoryID == 0 {
		return memory.MemoryEntry{}, fmt.Errorf("fixture was not published: %s", candidate.DecisionReason)
	}
	return store.EntryByID(candidate.PublishedMemoryID)
}
