package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestStoreRecentExchangesKeepExactRoles(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fakeMemoryEmbedder{}, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.appendFixtureOrphanTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.publishFixtureMemory(context.Background(), "usr_test", memoryFixture{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}

	turns, err := store.RecentCompletedExchanges(context.Background(), "usr_test", "session-1", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("unexpected exchanges: %+v", turns)
	}
	messages := SessionTurnMessages(turns[0])
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Content != "I like purple" || messages[1].Role != "assistant" || messages[1].Content != "Noted." {
		t.Fatalf("unexpected messages: %+v", messages)
	}
}

func TestStoreSessionContextKeepsToolMetadataOutOfProseAndScopesUser(t *testing.T) {
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")

	ctx := context.Background()
	if err := store.appendFixtureOrphanTurn(ctx, "shared-session", "user-1", "first question", "first answer", []string{"github.get_issue", "web.search"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.appendFixtureOrphanTurn(ctx, "shared-session", "user-2", "private question", "private answer", []string{"other.secret"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	retrieved, err := store.RecentCompletedExchanges(ctx, "user-1", "shared-session", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(retrieved) != 1 || retrieved[0].UserText != "first question" || SessionTurnMessages(retrieved[0])[1].Content != "first answer" {
		t.Fatalf("unexpected tenant exchange: %+v", retrieved)
	}
	if strings.Join(retrieved[0].ToolNames, ",") != "github.get_issue,web.search" {
		t.Fatalf("recent tool names = %+v", retrieved[0].ToolNames)
	}

	turns, err := store.RecentCompletedExchanges(ctx, "user-2", "shared-session", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].UserText != "private question" {
		t.Fatalf("unexpected scoped turns: %+v", turns)
	}
}

func TestRecentCompletedExchangesDoNotEmbed(t *testing.T) {
	embedder := &countingMemoryEmbedder{}
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), embedder, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.appendFixtureOrphanTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.publishFixtureMemory(context.Background(), "usr_test", memoryFixture{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	seedEmbeddingCount := len(embedder.inputs)

	_, err = store.RecentCompletedExchanges(context.Background(), "usr_test", "session-1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	queryEmbeddings := embedder.inputs[seedEmbeddingCount:]
	if len(queryEmbeddings) != 0 {
		t.Fatalf("expected no query embeddings from automatic context, got %d: %+v", len(queryEmbeddings), queryEmbeddings)
	}
}

func TestSessionCompactionDoesNotCrossUndeliveredTurn(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	middle := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")
	last := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "three")
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?`, middle); err != nil {
		t.Fatal(err)
	}
	planned, err := store.CompactionWindowAfter(context.Background(), "user", "session", generation, 0, 100)
	if err != nil || planned.TotalCount != 1 || planned.NewestTurnID != first || len(planned.Turns) != 1 || planned.Turns[0].ID != first {
		t.Fatalf("planned across delivery gap: %+v err=%v", planned, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, last, last, compactionTestModel, compactionTestGeneratorVersion); err == nil {
		t.Fatal("enqueued range across undelivered turn")
	}
	if err := store.MarkSessionTurnDeliveryFailed(context.Background(), "user", middle); err != nil {
		t.Fatal(err)
	}
	planned, err = store.CompactionWindowAfter(context.Background(), "user", "session", generation, 0, 100)
	if err != nil || planned.TotalCount != 2 || len(planned.Turns) != 2 || planned.Turns[1].ID != last {
		t.Fatalf("terminal failed delivery still blocked later turns: %+v err=%v", planned, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, last, last, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatalf("enqueue across terminal failed delivery: %v", err)
	}
}

func TestRecentCompletedExchangesExcludePendingAndFailedTurns(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	delivered := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "delivered")
	pending, err := store.appendFixturePendingTurn(context.Background(), "session", "user", generation, "pending", "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := store.appendFixturePendingTurn(context.Background(), "session", "user", generation, "failed", "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDeliveryFailed(context.Background(), "user", failed.ID); err != nil {
		t.Fatal(err)
	}

	turns, err := store.RecentCompletedExchanges(context.Background(), "user", "session", generation, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].ID != delivered {
		t.Fatalf("recent completed turns=%+v, pending=%d failed=%d", turns, pending.ID, failed.ID)
	}
}

func TestAllDeliveredSessionTurnsAfterPagesAcrossPendingGap(t *testing.T) {
	ctx := context.Background()
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user", "other")
	generation := activateCompactionSession(t, store, "user", "session")
	activateCompactionSession(t, store, "other", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "first")
	if _, err := store.appendFixturePendingTurn(ctx, "session", "user", generation, "pending", "answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	failed := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "failed")
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = created_at WHERE id = ?`, failed); err != nil {
		t.Fatal(err)
	}
	appendDeliveredCompactionTurn(t, store, "other", "session", generation, "private")
	last := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "last")
	boundary := int64(0)
	for _, want := range []int64{first, last} {
		page, err := store.PageDeliveredSessionTurnsAfter(ctx, "user", "session", generation, boundary, 1)
		if err != nil || len(page) != 1 || page[0].ID != want {
			t.Fatalf("boundary=%d page=%+v want=%d err=%v", boundary, page, want, err)
		}
		boundary = page[0].ID
	}
	if page, err := store.PageDeliveredSessionTurnsAfter(ctx, "user", "session", generation, boundary, 1); err != nil || len(page) != 0 {
		t.Fatalf("final page=%+v err=%v", page, err)
	}
	if _, err := store.PageDeliveredSessionTurnsAfter(ctx, "user", "session", generation, -1, 1); err == nil {
		t.Fatal("accepted negative boundary")
	}
	if _, err := store.PageDeliveredSessionTurnsAfter(ctx, "user", "session", generation+1, 0, 1); err == nil {
		t.Fatal("accepted inactive generation")
	}
}

func TestSessionCompactionExcludesInconsistentFailedDelivery(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	inconsistent := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = created_at WHERE id = ?`, inconsistent); err != nil {
		t.Fatal(err)
	}

	planned, err := store.CompactionWindowAfter(context.Background(), "user", "session", generation, 0, 10)
	if err != nil || planned.TotalCount != 1 || len(planned.Turns) != 1 || planned.Turns[0].ID != first {
		t.Fatalf("planned inconsistent delivery: %+v err=%v", planned, err)
	}
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, inconsistent, inconsistent, compactionTestModel, compactionTestGeneratorVersion); err == nil {
		t.Fatal("inconsistent failed-delivery endpoint was accepted")
	}
}

func TestLatestDeliveredSessionPromptPressureUsesPartialIndex(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	// Most historical turns have no pressure snapshot. Only a sparse delivered
	// subset belongs in the pressure index.
	if _, err := store.sql.Exec(`WITH RECURSIVE turns(n) AS (VALUES(1) UNION ALL SELECT n + 1 FROM turns WHERE n < 2000)
INSERT INTO session_turns(canonical_user_id, session_id, session_generation, user_text, assistant_text, created_at, delivered_at,
 compaction_pressure_tokens, compaction_pressure_limit, compaction_pressure_version)
SELECT 'user', 'session', ?, 'question', 'answer', '2026-09-01T00:00:00Z', '2026-09-01T00:00:01Z',
 CASE WHEN n % 100 = 0 THEN n END, CASE WHEN n % 100 = 0 THEN 10000 END,
 CASE WHEN n % 100 = 0 THEN 'pressure-v1' END FROM turns`, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		query := latestDeliveredSessionPromptPressureSQL
		if !explicit {
			query = strings.Replace(query, " AND compaction_pressure_tokens IS NOT NULL", "", 1)
		}
		rows, err := store.sql.Query("EXPLAIN QUERY PLAN "+query, "user", "session", generation, "pressure-v1")
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		t.Logf("explicit tokens predicate=%v plan=%s", explicit, plan.String())
		usesIndex := strings.Contains(plan.String(), "idx_session_turns_compaction_pressure")
		if explicit && (!usesIndex || strings.Contains(plan.String(), "TEMP B-TREE")) {
			t.Fatalf("unexpected pressure query plan: %s", plan.String())
		}
	}
	pressure, err := store.LatestDeliveredSessionPromptPressure(context.Background(), "user", "session", generation, "pressure-v1")
	if err != nil || pressure.Tokens != 2000 {
		t.Fatalf("latest pressure=%+v err=%v", pressure, err)
	}
	if _, err := store.LatestDeliveredSessionPromptPressure(context.Background(), "user", "session", generation, "other-version"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong version pressure err=%v", err)
	}
}
