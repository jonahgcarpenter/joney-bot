// Package testutil provides fixtures for tests outside the memory package.
package testutil

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// NewMemoryStore opens an isolated test-owned store and registers its cleanup.
func NewMemoryStore(t testing.TB, path string, log *config.Logger) *usermemory.Store {
	t.Helper()
	store, err := usermemory.NewSQLiteStore(path, nil, "", log)
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
	Provenance                           memoryformation.Provenance
}

var memorySequence atomic.Uint64

// AppendPendingTurn writes a generation-fenced exchange with an inert fixture pressure snapshot.
func AppendPendingTurn(ctx context.Context, store *usermemory.Store, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) (usermemory.StoredSessionTurn, error) {
	return store.AppendSessionTurnForGenerationResultWithPressureHistoryAndForegroundMemory(ctx, sessionID, userID, generation, userText, assistantText, toolNames, usermemory.EmptyToolHistory(), nil, ttl, usermemory.SessionPromptPressure{Tokens: 0, Limit: 100000, Version: "test-fixture"})
}

// AppendDeliveredTurn records delivery explicitly after the generation-fenced write.
func AppendDeliveredTurn(ctx context.Context, store *usermemory.Store, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	turn, err := AppendPendingTurn(ctx, store, sessionID, userID, generation, userText, assistantText, toolNames, ttl)
	if err != nil || turn.ID == 0 {
		return err
	}
	return store.MarkSessionTurnDelivered(ctx, userID, turn.ID)
}

// PublishMemory uses canonical candidate publication, not a parallel SQL writer.
func PublishMemory(ctx context.Context, store *usermemory.Store, userID string, req MemoryFixture) (usermemory.MemoryEntry, error) {
	if req.Category == "" {
		req.Category = "notes"
	}
	if req.Scope == "" {
		req.Scope = usermemory.ScopeLongTerm
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
		req.Provenance = memoryformation.ProvenanceUserStatement
	}
	slot, value := memoryformation.NormalizeClaimIdentity(memoryformation.Category(req.Category), "", "", req.Statement)
	candidate, _, err := store.ProposeCandidate(ctx, userID, usermemory.CandidateProposal{
		IdempotencyKey: fmt.Sprintf("fixture:%d", memorySequence.Add(1)),
		Source:         usermemory.FormationSource{ExtractorVersion: usermemory.AgentSaveExtractorVersion},
		Output: memoryformation.CandidateOutput{
			Scope: memoryformation.Scope(req.Scope), Category: memoryformation.Category(req.Category),
			Statement: req.Statement, Evidence: req.Evidence, Confidence: req.Confidence,
			Importance: req.Importance, TTL: req.TTL, Provenance: req.Provenance,
			Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeAgentSave,
			Approval: memoryformation.ApprovalApproved, Decision: memoryformation.DecisionAutomatic,
			ClaimSlot: slot, ClaimValue: value,
		},
	})
	if err != nil {
		return usermemory.MemoryEntry{}, err
	}
	if candidate.PublishedMemoryID == 0 {
		return usermemory.MemoryEntry{}, fmt.Errorf("fixture was not published: %s", candidate.DecisionReason)
	}
	return store.EntryByID(candidate.PublishedMemoryID)
}
