package memory

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestLateDeliveryInvalidatesCrossingCheckpointAndCanReplan(t *testing.T) {
	for name, markDelivered := range map[string]func(*Store, int64) error{
		"session delivery": func(store *Store, turnID int64) error {
			return store.MarkSessionTurnDelivered(context.Background(), "user", turnID)
		},
		"formation eligibility": func(store *Store, turnID int64) error {
			return store.MarkFormationEligible(context.Background(), "user", turnID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newSessionCompactionTestStore(t)
			seedAccountUsers(t, store, "user")
			generation := activateCompactionSession(t, store, "user", "session")
			first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
			middle, err := store.appendFixturePendingTurn(context.Background(), "session", "user", generation, "late", "answer", nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.MarkSessionTurnDeliveryFailed(context.Background(), "user", middle.ID); err != nil {
				t.Fatal(err)
			}
			last := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "three")
			if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, last, last, compactionTestModel, compactionTestGeneratorVersion); err != nil {
				t.Fatal(err)
			}
			job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: "Skipped late turn", GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishSessionSummary(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
				t.Fatal(err)
			}

			if err := markDelivered(store, middle.ID); err != nil {
				t.Fatal(err)
			}
			assertCompactionCount(t, store, `SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'user' AND session_id = 'session'`, 0)
			assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = 'user' AND session_id = 'session'`, 0)
			available, err := store.CompactionWindowAfter(context.Background(), "user", "session", generation, 0, 10)
			if err != nil || available.TotalCount != 3 || len(available.Turns) != 3 || available.Turns[1].ID != middle.ID {
				t.Fatalf("restored compaction range=%+v err=%v", available, err)
			}
			if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, last, last, compactionTestModel, compactionTestGeneratorVersion); err != nil {
				t.Fatalf("replan restored range: %v", err)
			}
		})
	}
}

func TestSessionPromptPressureBecomesVisibleOnlyAfterDelivery(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	turn, err := store.AppendPendingSessionTurn(context.Background(), SessionTurnWrite{SessionID: "session", UserID: "user", Generation: generation, UserText: "hello", AssistantText: "answer", ToolNames: []string{"web.search"}, History: EmptyToolHistory(), Staged: nil, TTL: time.Hour, Pressure: SessionPromptPressure{Tokens: 7000, Limit: 7000, Version: "pressure-v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LatestDeliveredSessionPromptPressure(context.Background(), "user", "session", generation, "pressure-v1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("pending pressure error=%v", err)
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	pressure, err := store.LatestDeliveredSessionPromptPressure(context.Background(), "user", "session", generation, "pressure-v1")
	if err != nil || pressure.TurnID != turn.ID || pressure.Tokens != 7000 || pressure.Limit != 7000 {
		t.Fatalf("pressure=%+v err=%v", pressure, err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET compaction_pressure_tokens = 7001 WHERE id = ?`, turn.ID); err == nil {
		t.Fatal("immutable pressure update succeeded")
	}
}
