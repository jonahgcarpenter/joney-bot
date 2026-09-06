package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

			var failedAt, payload string
			var summaryID int64
			var outboxCount int
			outboxQuery := fmt.Sprintf(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind = 'session_turn' AND entity_id = %d`, middle.ID)
			if err := store.sql.QueryRow(outboxQuery).Scan(&outboxCount); err != nil {
				t.Fatal(err)
			}
			if err := store.sql.QueryRow(`SELECT delivery_failed_at FROM session_turns WHERE id = ?`, middle.ID).Scan(&failedAt); err != nil {
				t.Fatal(err)
			}
			if err := store.sql.QueryRow(`SELECT artifact_payload, artifact_summary_id FROM durable_jobs WHERE id = ?`, job.ID).Scan(&payload, &summaryID); err != nil {
				t.Fatal(err)
			}
			// Fail only after both crossing checkpoint records have been deleted.
			if _, err := store.sql.Exec(fmt.Sprintf(`CREATE TRIGGER fail_late_delivery_outbox BEFORE INSERT ON durable_jobs
WHEN NEW.job_kind = 'derived_index' AND NEW.entity_id = %d AND NEW.entity_kind = 'session_turn'
BEGIN
 SELECT CASE WHEN EXISTS (SELECT 1 FROM durable_jobs WHERE id = %d)
 OR EXISTS (SELECT 1 FROM session_summaries WHERE id = %d)
 THEN RAISE(ABORT, 'checkpoint deletion not reached') END;
 SELECT RAISE(ABORT, 'injected late delivery outbox failure');
END`, middle.ID, job.ID, summaryID)); err != nil {
				t.Fatal(err)
			}
			if err := markDelivered(store, middle.ID); err == nil || !strings.Contains(err.Error(), "injected late delivery outbox failure") {
				t.Fatalf("outbox failure after checkpoint deletion: %v", err)
			}
			var delivered sql.NullString
			var restoredFailure, restoredPayload, state string
			var restoredSummaryID int64
			if err := store.sql.QueryRow(`SELECT delivered_at, delivery_failed_at FROM session_turns WHERE id = ?`, middle.ID).Scan(&delivered, &restoredFailure); err != nil || delivered.Valid || restoredFailure != failedAt {
				t.Fatalf("delivery rollback: delivered=%v failure=%q err=%v", delivered, restoredFailure, err)
			}
			if err := store.sql.QueryRow(`SELECT state, artifact_payload, artifact_summary_id FROM durable_jobs WHERE id = ?`, job.ID).Scan(&state, &restoredPayload, &restoredSummaryID); err != nil || state != "succeeded" || restoredPayload != payload || restoredSummaryID != summaryID {
				t.Fatalf("checkpoint job rollback: state=%q summary=%d err=%v", state, restoredSummaryID, err)
			}
			assertCompactionCount(t, store, fmt.Sprintf(`SELECT COUNT(*) FROM session_summaries WHERE id = %d AND narrative = 'Skipped late turn'`, summaryID), 1)
			assertCompactionCount(t, store, outboxQuery, outboxCount)
			if _, err := store.sql.Exec(`DROP TRIGGER fail_late_delivery_outbox`); err != nil {
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
