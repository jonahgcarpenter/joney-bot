package memory

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/policy"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// These fixtures submit already-validated observations to canonical publication.
// They do not implement a second writer or emulate retired import behavior.
type memoryFixture struct {
	Scope, Category, Statement, Evidence, SourceSessionID, Supersedes string
	Confidence                                                        float64
	Importance                                                        int
	TTL                                                               time.Duration
	Provenance                                                        policy.Provenance
}

var fixtureSequence atomic.Uint64

func (s *Store) loadFixtureCandidate(ctx context.Context, userID string, id int64) (FormationCandidate, error) {
	return scanFormationCandidate(s.sql.QueryRowContext(ctx, candidateSelect+` WHERE memory_candidates.canonical_user_id = ? AND memory_candidates.id = ?`, userID, id))
}

const fixtureShortTermTTL = 30 * 24 * time.Hour

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

// appendFixtureOrphanTurn seeds a raw delivered exchange without a session binding,
// for retention and account-merge tests that deliberately exercise orphan history.
func (s *Store) appendFixtureOrphanTurn(ctx context.Context, sessionID, userID, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	now := time.Now().UTC()
	_, err := s.sql.ExecContext(ctx, `INSERT INTO session_turns
		(session_id, canonical_user_id, session_generation, user_text, assistant_text, tool_names, created_at, expires_at, delivered_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?)`, sessionID, userID, userText, assistantText, strings.Join(toolNames, ","), formatTime(now), formatTime(now.Add(ttl)), formatTime(now))
	return err
}

// appendFixturePendingTurn seeds a legacy turn without pressure metadata.
func (s *Store) appendFixturePendingTurn(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) (StoredSessionTurn, error) {
	return s.appendSessionTurnWithForegroundMemory(ctx, sessionID, userID, generation, userText, assistantText, toolNames, EmptyToolHistory(), nil, ttl, nil)
}

func (s *Store) appendFixtureDeliveredTurn(ctx context.Context, sessionID, userID string, generation int, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	turn, err := s.appendFixturePendingTurn(ctx, sessionID, userID, generation, userText, assistantText, toolNames, ttl)
	if err != nil || turn.ID == 0 {
		return err
	}
	return s.MarkSessionTurnDelivered(ctx, userID, turn.ID)
}

func newTestStore(path string, log *config.Logger) *Store {
	store, err := NewSQLiteStore(path, nil, "", log)
	if err != nil {
		panic(err)
	}
	return store
}

func (s *Store) publishFixtureMemory(ctx context.Context, userID string, req memoryFixture) (MemoryEntry, error) {
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
		req.Provenance = policy.ProvenanceUserStatement
	}
	if req.Scope == ScopeShortTerm && req.TTL == 0 {
		req.TTL = fixtureShortTermTTL
	}
	slot, value := policy.NormalizeClaimIdentity(policy.Category(req.Category), "", "", req.Statement)
	candidate, _, err := s.ProposeCandidate(ctx, userID, CandidateProposal{
		IdempotencyKey:      fmt.Sprintf("fixture:%d", fixtureSequence.Add(1)),
		SupersedesStatement: req.Supersedes,
		Source:              FormationSource{SessionID: req.SourceSessionID, ExtractorVersion: AgentSaveExtractorVersion},
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
		return MemoryEntry{}, err
	}
	if candidate.PublishedMemoryID == 0 {
		return MemoryEntry{}, fmt.Errorf("fixture was not published: %s", candidate.DecisionReason)
	}
	return s.EntryByID(candidate.PublishedMemoryID)
}

type fakeMemoryEmbedder struct{}

func (fakeMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

type countingMemoryEmbedder struct {
	inputs []string
}

func (f *countingMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.inputs = append(f.inputs, req.Input)
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

func seedAccountUsers(t *testing.T, store *Store, userIDs ...string) {
	t.Helper()
	for _, userID := range userIDs {
		if _, err := store.sql.Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?)`, userID); err != nil {
			t.Fatalf("seed account user %q: %v", userID, err)
		}
	}
}

func newFormationTestStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	seedAccountUsers(t, store, "user")
	return store
}

func evaluatedFormationCandidate(t *testing.T, source, evidence, statement string, category policy.Category) policy.CandidateOutput {
	t.Helper()
	output, err := policy.Evaluate(policy.CandidateInput{SourceUserText: source, Statement: statement, Evidence: evidence, Provenance: policy.ProvenanceUserStatement, ClaimedAuthority: policy.AuthorityUserDirect, Sensitivity: policy.SensitivityLow, Mode: policy.ModeAutomaticExtraction, Scope: policy.ScopeLongTerm, Category: category, Context: policy.ContextDirectAssertion, Confidence: 0.9, Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func evaluatedClaimCandidate(t *testing.T, source, statement string, category policy.Category, provenance policy.Provenance, sensitivity policy.Sensitivity, confidence float64, claimSlot, claimValue string) policy.CandidateOutput {
	t.Helper()
	claimedAuthority := policy.AuthorityModel
	if provenance == policy.ProvenanceUserStatement {
		claimedAuthority = policy.AuthorityUserDirect
	}
	output, err := policy.Evaluate(policy.CandidateInput{SourceUserText: source, Statement: statement, Evidence: source, Provenance: provenance, ClaimedAuthority: claimedAuthority, Sensitivity: sensitivity, Mode: policy.ModeAutomaticExtraction, Scope: policy.ScopeLongTerm, Category: category, Context: policy.ContextDirectAssertion, Confidence: confidence, Importance: 4, ClaimSlot: claimSlot, ClaimValue: claimValue})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func seedFormationTurn(t *testing.T, store *Store, userID, sessionID, userText string, requestID ...string) int64 {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), userID, sessionID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if len(requestID) > 0 {
		ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID[0]})
	}
	turn, err := store.appendFixturePendingTurn(ctx, sessionID, userID, profile.Generation, userText, "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(context.Background(), userID, turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}

const (
	compactionTestModel            = "model"
	compactionTestGeneratorVersion = "session-summary-v1"
)

func newSessionCompactionTestStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func activateCompactionSession(t *testing.T, store *Store, userID, sessionID string) int {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), userID, sessionID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return profile.Generation
}

func appendDeliveredCompactionTurn(t *testing.T, store *Store, userID, sessionID string, generation int, text string) int64 {
	t.Helper()
	turn, err := store.appendFixturePendingTurn(context.Background(), sessionID, userID, generation, text, "answer "+text, nil, time.Hour)
	if err != nil || turn.ID == 0 {
		t.Fatalf("append turn = %+v, err = %v", turn, err)
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), userID, turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}

func assertCompactionCount(t *testing.T, store *Store, query string, want int) {
	t.Helper()
	var got int
	if err := store.sql.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q count = %d, want %d", query, got, want)
	}
}

func rebuildTestIndexes(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	for _, kind := range []string{IndexKindMemoryFTS, IndexKindTranscriptFTS} {
		revision, err := store.CreateIndexRevision(ctx, kind, "sqlite_fts5", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if kind == IndexKindMemoryFTS {
			records, err := store.ActiveMemoryIndexRecords(ctx, 0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			records, err := store.DeliveredTranscriptIndexRecords(ctx, 0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if err := store.WriteTranscriptIndexRecord(ctx, revision, record); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
			t.Fatal(err)
		}
	}
	if store.embedder == nil || store.embedModel == "" {
		return
	}
	probe, err := store.embedder.Embed(ctx, llm.EmbedRequest{Model: store.embedModel, Input: "test probe"})
	if err != nil || probe == nil || len(probe.Embeddings) == 0 || len(probe.Embeddings[0]) == 0 {
		return
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryVector, "llm_gateway", store.embedModel, len(probe.Embeddings[0]))
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ActiveMemoryIndexRecords(ctx, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		response, err := store.embedder.Embed(ctx, llm.EmbedRequest{Model: store.embedModel, Input: memoryEmbeddingText(record.Scope, record.Category, record.Statement, record.Evidence)})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.WriteMemoryIndexRecord(ctx, revision, record, response.Embeddings[0]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
}
