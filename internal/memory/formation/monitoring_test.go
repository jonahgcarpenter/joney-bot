package formation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func TestFormationCompletionLogRequiresCommittedJob(t *testing.T) {
	for _, purpose := range []string{"empty", "agent_save", "replay", "complete_failure"} {
		t.Run(purpose, func(t *testing.T) {
			store, db := formationTestStoreWithDB(t)
			turn := formationTestTurn(t, store, "synthetic private source", "source-request")
			if purpose == "agent_save" {
				turn = formationTestForegroundTurn(t, store, "I prefer concise replies.", "source-request-2")
				if err := store.MarkFormationEligible(context.Background(), "user-1", turn); err != nil {
					t.Fatal(err)
				}
			}
			source := memory.FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: turn, Model: "model", ExtractorVersion: memory.FormationExtractorVersion}
			var id int64
			var err error
			if purpose == "agent_save" {
				id, _, err = store.EnqueueAgentSaveFormationJob(context.Background(), source, "user-1")
			} else {
				id, err = store.EnqueueFormationJob(context.Background(), source, "user-1")
			}
			if err != nil {
				t.Fatal(err)
			}
			if purpose == "complete_failure" {
				if _, err := db.SQL().Exec(`CREATE TRIGGER fail_complete BEFORE UPDATE OF state ON durable_jobs WHEN NEW.state = 'succeeded' AND NEW.job_kind = 'memory_formation' BEGIN SELECT RAISE(ABORT, 'private failure sentinel'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			log := config.NewLogger(config.LevelInfo)
			log.SetOutput(&output)
			extractor := &fakeExtractor{}
			service := NewService(store, extractor, "model", log)
			if purpose == "replay" {
				job, err := store.ClaimFormationJob(context.Background(), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err := service.process(context.Background(), &job); err != nil {
					t.Fatal(err)
				}
				if err := store.DeferFormationJob(context.Background(), job, time.Second); err != nil {
					t.Fatal(err)
				}
				if _, err := db.SQL().Exec(`UPDATE durable_jobs SET available_at = '2000-01-01T00:00:00Z' WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
			}
			service.drain(context.Background())
			if purpose == "replay" && extractor.calls != 1 {
				t.Fatalf("replay extraction calls=%d", extractor.calls)
			}
			state, err := store.FormationJobState(context.Background(), "user-1", id)
			if err != nil {
				t.Fatal(err)
			}
			completed := 0
			for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record["job_id"] != float64(id) || record["job_kind"] != "memory_formation" || record["workload"] != "formation" || record["operation_id"] == nil {
					t.Fatalf("missing attempt scope: %s", line)
				}
				if record["event"] == "user_memory.formation.job.complete" {
					completed++
					if record["record_kind"] != "summary" || record["is_replay"] != (purpose == "replay") || record["input_turn_count"] != float64(1) {
						t.Fatalf("missing result metrics: %s", line)
					}
					wantCandidates := float64(0)
					if purpose == "agent_save" {
						wantCandidates = 1
					}
					if record["candidate_count"] != wantCandidates || record["published_outcome_count"] != wantCandidates {
						t.Fatalf("wrong result counts: %s", line)
					}
					if state != "succeeded" || record["job_state"] != "succeeded" || record["duration_ms"] == nil || record["model_submission_count"] == nil {
						t.Fatalf("invalid completion: %s state=%s", line, state)
					}
				}
			}
			want := 1
			if purpose == "complete_failure" {
				want = 0
			}
			if completed != want {
				t.Fatalf("completion count=%d want=%d logs=%s", completed, want, output.String())
			}
			if strings.Contains(output.String(), "private failure sentinel") || strings.Contains(output.String(), "synthetic private source") || strings.Contains(output.String(), "session_id") {
				t.Fatalf("private log content: %s", output.String())
			}
		})
	}
}

func TestPatternSummarySeparatesAcceptanceAndPublication(t *testing.T) {
	for _, tc := range []struct {
		name         string
		confidence   float64
		replay       bool
		publications float64
	}{
		{"proposed", 0.2, false, 0}, {"published", 0.8, false, 1}, {"replayed", 0.8, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := formationTestStoreWithDB(t)
			first := formationTestTurn(t, store, "I keep making review notes concise.", "first")
			second := formationTestTurn(t, store, "I repeatedly make review summaries concise.", "second")
			extractor := &fakePatternExtractor{patterns: memory.MemoryPatternBatch{Patterns: []memory.MemoryPattern{{
				Statement: "The user may favor concise review communication.", Category: "communication_preferences", ClaimSlot: "communication.review_style", ClaimValue: "concise", Sensitivity: "low", Confidence: tc.confidence,
				Observations: []memory.PatternObservation{{SourceTurnID: first, Evidence: "I keep making review notes concise."}, {SourceTurnID: second, Evidence: "I repeatedly make review summaries concise."}},
			}}}}
			id, _, err := store.EnqueuePatternFormationJob(context.Background(), memory.FormationSource{SessionID: "session", SessionGeneration: 1, TurnID: second, Model: "model"}, "user-1")
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			log := config.NewLogger(config.LevelInfo)
			log.SetOutput(&output)
			service := NewService(store, extractor, "model", log)
			if tc.replay {
				job, err := store.ClaimFormationJob(context.Background(), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err := service.process(context.Background(), &job); err != nil {
					t.Fatal(err)
				}
				if err := store.DeferFormationJob(context.Background(), job, time.Second); err != nil {
					t.Fatal(err)
				}
				if _, err := db.SQL().Exec(`UPDATE durable_jobs SET available_at = '2000-01-01T00:00:00Z' WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
			}
			service.drain(context.Background())
			found := false
			for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] != "user_memory.formation.job.complete" {
					continue
				}
				found = true
				for key, want := range map[string]any{"record_kind": "summary", "is_replay": tc.replay, "input_turn_count": float64(2), "pattern_count": float64(1), "observation_count": float64(2), "candidate_count": float64(2), "accepted_pattern_count": float64(1), "rejected_pattern_count": float64(0), "publication_count": tc.publications} {
					if record[key] != want {
						t.Fatalf("%s=%v want=%v: %s", key, record[key], want, line)
					}
				}
			}
			if !found {
				t.Fatalf("missing summary: %s", output.String())
			}
		})
	}
}
