package memory

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSessionCompactionArtifactPublicationAndIncrementalSources(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	second := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")

	jobID, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, second, second, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || job.ID != jobID {
		t.Fatalf("claim = %+v, err = %v", job, err)
	}
	artifact := SummaryArtifact{Narrative: "First checkpoint", OpenTasks: []string{"ship it"}, Commitments: []string{"follow up"}, Entities: []string{"Atlas"}, Decisions: []string{"use Go"}, TopicTags: []string{"project"}, GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, artifact); err != nil {
		t.Fatal(err)
	}
	storedArtifact, err := store.SessionCompactionArtifact(context.Background(), job)
	if err != nil || storedArtifact.GenerationModel != compactionTestModel || storedArtifact.GeneratorVersion != compactionTestGeneratorVersion {
		t.Fatalf("stored artifact = %+v, err = %v", storedArtifact, err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: "changed", GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}); err == nil {
		t.Fatal("expected immutable artifact mismatch")
	}
	summary, err := store.PublishSessionSummary(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.SourceTurnIDs, []int64{first, second}) || !reflect.DeepEqual(summary.OpenTasks, []string{"ship it"}) {
		t.Fatalf("published summary = %+v", summary)
	}
	replayed, err := store.PublishSessionSummary(context.Background(), job)
	if err != nil || replayed.ID != summary.ID {
		t.Fatalf("replayed summary = %+v, err = %v", replayed, err)
	}
	if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET user_text = 'changed' WHERE id = ?`, first); err == nil {
		t.Fatal("updated immutable summary source text")
	}
	if _, err := store.sql.Exec(`DELETE FROM session_turns WHERE id = ?`, first); err == nil {
		t.Fatal("deleted immutable summary source")
	}

	third := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "three")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, third, third, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	incrementalJob, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), incrementalJob, SummaryArtifact{Narrative: "Incremental checkpoint", GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	incremental, err := store.PublishSessionSummary(context.Background(), incrementalJob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental.SourceTurnIDs, []int64{first, second, third}) {
		t.Fatalf("incremental sources = %v", incremental.SourceTurnIDs)
	}
	latest, err := store.LatestSessionSummary(context.Background(), "user", "session", generation)
	if err != nil || latest.ID != incremental.ID {
		t.Fatalf("latest = %+v, err = %v", latest, err)
	}
	if _, err := store.ResetSession(context.Background(), "user", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'user' AND session_id = 'session'`, 0)
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction' AND canonical_user_id = 'user' AND session_id = 'session'`, 0)
}

func TestSessionCompactionPublicationRollsBackAndRejectsStaleGeneration(t *testing.T) {
	store := newSessionCompactionTestStore(t)
	seedAccountUsers(t, store, "user")
	generation := activateCompactionSession(t, store, "user", "session")
	first := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "one")
	second := appendDeliveredCompactionTurn(t, store, "user", "session", generation, "two")
	if _, err := store.EnqueueSessionCompactionJob(context.Background(), "user", "session", generation, first, second, second, compactionTestModel, compactionTestGeneratorVersion); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "worker", time.Minute, compactionTestModel, compactionTestGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: "checkpoint", GenerationModel: compactionTestModel, GeneratorVersion: compactionTestGeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = NULL WHERE id = ?`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(context.Background(), job); err == nil {
		t.Fatal("expected incomplete source range rejection")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM session_summaries`, 0)
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction' AND artifact_summary_id IS NOT NULL`, 0)

	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetSession(context.Background(), "user", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishSessionSummary(context.Background(), job); err == nil {
		t.Fatal("expected stale generation rejection")
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM session_summaries`, 0)
	changed, err := store.ReconcileSessionCompactionJobs(context.Background(), compactionTestModel, compactionTestGeneratorVersion)
	if err != nil || changed != 0 {
		t.Fatalf("reconciled = %d, err = %v", changed, err)
	}
	assertCompactionCount(t, store, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'session_compaction'`, 0)
}
