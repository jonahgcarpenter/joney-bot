package usermemory

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

// These fixtures submit already-validated observations to canonical publication.
// They do not implement a second writer or emulate retired import behavior.
type SaveRequest struct {
	Scope, Category, Statement, Evidence, SourceSessionID, Supersedes string
	Confidence                                                        float64
	Importance                                                        int
	TTL                                                               time.Duration
	Provenance                                                        memoryformation.Provenance
}

var fixtureSequence atomic.Uint64

func (s *Store) LoadCandidate(ctx context.Context, userID string, id int64) (FormationCandidate, error) {
	return scanFormationCandidate(s.sql.QueryRowContext(ctx, candidateSelect+` WHERE memory_candidates.canonical_user_id = ? AND memory_candidates.id = ?`, userID, id))
}

const DefaultShortTermTTL = 30 * 24 * time.Hour

func (s *Store) vectorTableDimension(name string) (int, bool) {
	var definition string
	if err := s.sql.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&definition); err != nil {
		return 0, false
	}
	return vectorDimensionFromSQL(definition)
}

func memoryEmbeddingText(scope, category, statement, evidence string) string {
	return strings.TrimSpace(scope + "\n" + category + "\n" + statement + "\nEvidence: " + evidence)
}

// AppendSessionTurn seeds a raw delivered exchange without a session binding,
// for retention and account-merge tests that deliberately exercise orphan history.
func (s *Store) AppendSessionTurn(ctx context.Context, sessionID, userID, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	now := time.Now().UTC()
	_, err := s.sql.ExecContext(ctx, `INSERT INTO session_turns
		(session_id, canonical_user_id, session_generation, user_text, assistant_text, tool_names, created_at, expires_at, delivered_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?)`, sessionID, userID, userText, assistantText, strings.Join(toolNames, ","), formatTime(now), formatTime(now.Add(ttl)), formatTime(now))
	return err
}

// AppendSessionTurnForGenerationResult seeds a legacy turn without pressure metadata.
func (s *Store) AppendSessionTurnForGenerationResult(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) (StoredSessionTurn, error) {
	return s.appendSessionTurnWithForegroundMemory(ctx, sessionID, userID, generation, userText, assistantText, toolNames, EmptyToolHistory(), nil, ttl, nil)
}

func (s *Store) AppendSessionTurnForGeneration(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	turn, err := s.AppendSessionTurnForGenerationResult(ctx, sessionID, userID, generation, userText, assistantText, toolNames, ttl)
	if err != nil || turn.ID == 0 {
		return err
	}
	return s.MarkSessionTurnDelivered(ctx, userID, turn.ID)
}

func NewStore(path string, log *config.Logger) *Store {
	store, err := NewSQLiteStore(path, nil, "", log)
	if err != nil {
		panic(err)
	}
	return store
}

func (s *Store) SaveMemory(ctx context.Context, userID string, req SaveRequest) (MemoryEntry, error) {
	if req.Category == "" {
		req.Category = "notes"
	}
	if req.Scope == "" {
		req.Scope = ScopeShortTerm
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
	if req.Scope == ScopeShortTerm && req.TTL == 0 {
		req.TTL = DefaultShortTermTTL
	}
	slot, value := memoryformation.NormalizeClaimIdentity(memoryformation.Category(req.Category), "", "", req.Statement)
	candidate, _, err := s.ProposeCandidate(ctx, userID, CandidateProposal{
		IdempotencyKey:      fmt.Sprintf("fixture:%d", fixtureSequence.Add(1)),
		SupersedesStatement: req.Supersedes,
		Source:              FormationSource{SessionID: req.SourceSessionID, ExtractorVersion: AgentSaveExtractorVersion},
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
		return MemoryEntry{}, err
	}
	if candidate.PublishedMemoryID == 0 {
		return MemoryEntry{}, fmt.Errorf("fixture was not published: %s", candidate.DecisionReason)
	}
	return s.EntryByID(candidate.PublishedMemoryID)
}
