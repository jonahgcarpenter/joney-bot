package memory

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestSessionCompactionMutationsRequireExactLease(t *testing.T) {
	for _, advance := range []string{"renew", "same_owner_reclaim"} {
		for _, operation := range []string{"save", "publish", "complete", "skip"} {
			t.Run(advance+"/"+operation, func(t *testing.T) {
				ctx := context.Background()
				store := newSessionCompactionTestStore(t)
				seedAccountUsers(t, store, "user")
				generation := activateCompactionSession(t, store, "user", "session")
				turnID := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
				if _, err := store.EnqueueSessionCompactionJob(ctx, "user", "session", generation, turnID, turnID, turnID, compactionTestModel, compactionTestGeneratorVersion); err != nil {
					t.Fatal(err)
				}
				stale, err := store.ClaimSessionCompactionJob(ctx, "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
				if err != nil {
					t.Fatal(err)
				}
				artifact := SummaryArtifact{Narrative: "checkpoint", GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}
				if operation == "publish" || operation == "complete" {
					if err := store.SaveSessionCompactionArtifact(ctx, stale, artifact); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "complete" {
					if _, err := store.PublishSessionSummary(ctx, stale); err != nil {
						t.Fatal(err)
					}
				}
				current := stale
				if advance == "renew" {
					current.LeaseUntil, err = store.RenewSessionCompactionJobLease(ctx, stale, 2*time.Minute)
				} else {
					if _, err := store.sql.Exec(`UPDATE durable_jobs SET lease_until = ? WHERE id = ?`, formatTime(time.Now().Add(-time.Minute)), stale.ID); err != nil {
						t.Fatal(err)
					}
					current, err = store.ClaimSessionCompactionJob(ctx, stale.LeaseOwner, 2*time.Minute, compactionTestModel, compactionTestGeneratorVersion)
				}
				if err != nil || current.ID != stale.ID || current.LeaseOwner != stale.LeaseOwner || current.LeaseUntil.Equal(stale.LeaseUntil) {
					t.Fatalf("lease did not advance for the same owner: current=%+v err=%v", current, err)
				}
				type persisted struct {
					state, payload, owner, updated string
					completed, lease               sql.NullString
					summaryID                      sql.NullInt64
					summaryCount                   int
				}
				snapshot := func() persisted {
					t.Helper()
					var got persisted
					if err := store.sql.QueryRow(`SELECT state, artifact_payload, lease_owner, lease_until, updated_at, completed_at, artifact_summary_id, (SELECT COUNT(*) FROM session_summaries) FROM durable_jobs WHERE id = ?`, current.ID).Scan(&got.state, &got.payload, &got.owner, &got.lease, &got.updated, &got.completed, &got.summaryID, &got.summaryCount); err != nil {
						t.Fatal(err)
					}
					return got
				}
				mutate := func(job SessionCompactionJob) error {
					switch operation {
					case "save":
						return store.SaveSessionCompactionArtifact(ctx, job, artifact)
					case "publish":
						_, err := store.PublishSessionSummary(ctx, job)
						return err
					default:
						return store.CompleteSessionCompactionJob(ctx, job, operation == "skip")
					}
				}
				before := snapshot()
				if err := mutate(stale); err == nil {
					t.Fatal("stale claim mutated work under the same owner's newer lease")
				}
				if after := snapshot(); after != before {
					t.Fatal("rejected stale mutation changed persisted state")
				}
				if err := mutate(current); err != nil {
					t.Fatalf("current claim rejected: %v", err)
				}
				switch operation {
				case "save":
					loaded, err := store.SessionCompactionArtifact(ctx, current)
					if err != nil || loaded.Narrative != artifact.Narrative {
						t.Fatalf("current artifact=%+v err=%v", loaded, err)
					}
				case "publish", "complete":
					// Replaying an existing publication is a read, not a new mutation.
					published := snapshot()
					if !published.summaryID.Valid || published.summaryCount != 1 {
						t.Fatal("current claim did not publish exactly one summary")
					}
					for _, claim := range []SessionCompactionJob{stale, current} {
						replayed, err := store.PublishSessionSummary(ctx, claim)
						if err != nil || replayed.ID != published.summaryID.Int64 || snapshot() != published {
							t.Fatalf("publication replay changed state: summary=%+v err=%v", replayed, err)
						}
					}
					if operation == "complete" && published.state != "succeeded" {
						t.Fatalf("completion state=%q", published.state)
					}
				case "skip":
					if got := snapshot(); got.state != "skipped" || got.owner != "" {
						t.Fatalf("skip state=%q owner=%q", got.state, got.owner)
					}
				}
			})
		}
	}
}
