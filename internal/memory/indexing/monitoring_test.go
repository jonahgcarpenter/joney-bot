package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestRebuildFailureDoesNotInventCoverage(t *testing.T) {
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	s := NewService(nil, nil, nil, "", log)
	s.health("index.rebuild.failed", memory.DerivedIndexRevision{Kind: memory.IndexKindMemoryFTS, Revision: 2}, 0, 0, "degraded", time.Second, errors.New("private error sentinel"))
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["level"] != "warn" || record["phase"] == nil || record["coverage"] != nil || record["expected_count"] != nil || strings.Contains(output.String(), "private error sentinel") {
		t.Fatalf("failure record=%s", output.String())
	}
	output.Reset()
	s.health("index.rebuild.complete", memory.DerivedIndexRevision{Kind: memory.IndexKindMemoryFTS, Revision: 2}, 4, 4, "ok", time.Second, nil)
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["level"] != "info" || record["coverage"] != float64(1) || record["expected_count"] != float64(4) {
		t.Fatalf("success record=%s", output.String())
	}
}

type monitoringEmbedder func(context.Context, llm.EmbedRequest) (*llm.EmbedResponse, error)

func (f monitoringEmbedder) Embed(ctx context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	return f(ctx, req)
}

func TestCanceledRebuildReusesBuildingRevision(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := memorytest.PublishMemory(context.Background(), store, "user", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Statement: "Synthetic rebuild source."}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	embedder := monitoringEmbedder(func(ctx context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
		if req.Input == "derived index dimension probe" {
			return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("worker did not join")
		}
	})
	go func() { defer close(finished); done <- NewService(store, nil, embedder, "model", nil).RunOnce(ctx) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("embedding never started")
	}
	building, err := store.BuildingIndexRevision(context.Background(), memory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	retained, err := store.BuildingIndexRevision(context.Background(), memory.IndexKindMemoryVector)
	if err != nil || retained.ID != building.ID {
		t.Fatalf("building revision lost: id=%d err=%v", retained.ID, err)
	}
	restarted := NewService(store, nil, &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}, "model", nil)
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), memory.IndexKindMemoryVector)
	if err != nil || live.ID != building.ID || live.TableName != building.TableName {
		t.Fatalf("restart replaced building revision: id=%d err=%v", live.ID, err)
	}
}

func TestCycleEmbeddingCorrelationAndRetryClassification(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := memorytest.PublishMemory(context.Background(), store, "user", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Statement: "Synthetic rebuild source."}); err != nil {
		t.Fatal(err)
	}
	var calls []requestctx.Metadata
	embedder := monitoringEmbedder(func(ctx context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
		calls = append(calls, requestctx.MetadataFromContext(ctx))
		if req.Input == "derived index dimension probe" {
			return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
		}
		return nil, context.DeadlineExceeded
	})
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	service := NewService(store, nil, embedder, "model", log)
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{OperationID: "parent-cycle"})
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 {
		t.Fatal("missing real cycle embedding calls")
	}
	cycleID := calls[0].OperationID
	if cycleID == "" || cycleID == "parent-cycle" || calls[0].ParentOperationID != "parent-cycle" {
		t.Fatalf("invalid cycle metadata: %+v", calls[0])
	}
	for _, call := range calls {
		if call.Workload != "indexing" {
			t.Fatalf("embedding workload=%q", call.Workload)
		}
		if call.JobID == 0 && (call.OperationID != cycleID || call.ParentOperationID != "parent-cycle") {
			t.Fatalf("rebuild embedding lost cycle correlation: %+v", call)
		}
		if call.JobID != 0 && call.ParentOperationID != cycleID {
			t.Fatalf("outbox embedding lost parent cycle: %+v", call)
		}
	}
	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "index.rebuild.failed" {
			found = true
			if record["operation_id"] != cycleID || record["parent_operation_id"] != "parent-cycle" || record["workload"] != "indexing" || record["expected_count"] != nil || record["coverage"] != nil || record["level"] != "warn" {
				t.Fatalf("invalid correlated failure: %s", line)
			}
		}
	}
	if !found {
		t.Fatalf("no failed rebuild: %s", output.String())
	}
	if service.log != log || service.dimension != 2 {
		t.Fatal("cycle leaked scoped logger or lost cached dimension")
	}
	// A building vector revision makes the next outbox attempt exercise embedding.
	if _, err := store.CreateIndexRevision(context.Background(), memory.IndexKindMemoryVector, "llm_gateway", "model", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileDerivedIndexChanges(context.Background()); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	// Publish another canonical mutation to ensure a fresh outbox job.
	if _, err := memorytest.PublishMemory(context.Background(), store, "user", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Statement: "Another synthetic rebuild source."}); err != nil {
		t.Fatal(err)
	}
	service.drain(ctx)
	found = false
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "index.outbox.attempt.complete" && record["job_state"] == "retry" {
			found = true
			if record["error_code"] != config.ErrorField(context.DeadlineExceeded).Value || record["phase"] != "apply" || record["record_kind"] != "summary" {
				t.Fatalf("missing retry classification: %s", line)
			}
		}
	}
	if !found {
		t.Fatalf("no retry attempt: %s", output.String())
	}
}

func TestSnapshotUnavailableDoesNotEmitZeroJobGauges(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	NewService(store, nil, nil, "", log).snapshot(context.Background())
	if !strings.Contains(output.String(), `"event":"memory.health.failed"`) || strings.Contains(output.String(), `"event":"memory.jobs.health"`) || strings.Contains(output.String(), `"queued_count":0`) {
		t.Fatalf("unavailable gauges: %s", output.String())
	}
	if !strings.Contains(output.String(), `"is_last_maintenance_known":false`) || strings.Contains(output.String(), "last_maintenance_age_ms") {
		t.Fatalf("unknown maintenance timestamp: %s", output.String())
	}
	if !strings.Contains(output.String(), `"record_kind":"snapshot"`) {
		t.Fatalf("missing snapshot tag: %s", output.String())
	}
}
