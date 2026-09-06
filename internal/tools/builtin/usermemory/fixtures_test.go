package usermemory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func newHandlerTestStore(t *testing.T, embedder llm.Embedder) (*memory.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	model := ""
	if embedder != nil {
		model = "test-embed"
	}
	store, err := memory.NewSQLiteStore(path, embedder, model, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := database.Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store, db.SQL()
}

func seedHandlerUser(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?)`, userID); err != nil {
		t.Fatal(err)
	}
}

type fixedRecallEmbedder struct{}

func (fixedRecallEmbedder) Embed(context.Context, llm.EmbedRequest) (*llm.EmbedResponse, error) {
	return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
}

func bindTranscriptTestSession(t *testing.T, store *memory.Store, userID, sessionID string) int {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), userID, sessionID, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return profile.Generation
}

func insertTranscriptTestTurn(t *testing.T, store *memory.Store, userID, sessionID string, generation int, userText, assistantText string, delivered bool, ttl time.Duration) {
	t.Helper()
	turn, err := memorytest.AppendPendingTurn(context.Background(), store, sessionID, userID, generation, userText, assistantText, nil, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		if err := store.MarkSessionTurnDelivered(context.Background(), userID, turn.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func rebuildHandlerIndexes(t *testing.T, store *memory.Store, vector bool) {
	t.Helper()
	ctx := context.Background()
	kinds := []string{memory.IndexKindMemoryFTS, memory.IndexKindTranscriptFTS}
	if vector {
		kinds = append(kinds, memory.IndexKindMemoryVector)
	}
	for _, kind := range kinds {
		provider, model, dimension := "sqlite_fts5", "", 0
		var embedding []float64
		if kind == memory.IndexKindMemoryVector {
			provider, model, dimension, embedding = "llm_gateway", "test-embed", 2, []float64{1, 0}
		}
		revision, err := store.CreateIndexRevision(ctx, kind, provider, model, dimension)
		if err != nil {
			t.Fatal(err)
		}
		if kind == memory.IndexKindTranscriptFTS {
			records, err := store.DeliveredTranscriptIndexRecords(ctx, 0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if err := store.WriteTranscriptIndexRecord(ctx, revision, record); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			records, err := store.ActiveMemoryIndexRecords(ctx, 0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if err := store.WriteMemoryIndexRecord(ctx, revision, record, embedding); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
			t.Fatal(err)
		}
	}
}
