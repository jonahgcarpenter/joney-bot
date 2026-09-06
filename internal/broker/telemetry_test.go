package broker

import (
	"context"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type telemetryProcessor struct {
	started chan context.Context
	release chan struct{}
}

func (p *telemetryProcessor) Process(ctx context.Context, _ agent.Request) (*agent.Response, error) {
	p.started <- ctx
	<-p.release
	return &agent.Response{Response: "ok"}, nil
}

func TestSnapshotAndExecutionTelemetry(t *testing.T) {
	p := &telemetryProcessor{started: make(chan context.Context, 1), release: make(chan struct{})}
	b := NewBroker(p, 1, config.NewLogger(config.LevelError))
	usage := requestctx.NewUsageCollector()
	req := &Request{RequestID: "req", Usage: usage, Metadata: requestctx.Metadata{OperationID: "operation", ParentOperationID: "parent"}, Principal: identity.Principal{CanonicalUserID: "user", ExternalID: "external", Gateway: "homeassistant", Assurance: identity.AssuranceHomeAssistantToken}, ResponseChan: make(chan Result, 1)}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	// Pin an old enqueue timestamp instead of sleeping to test the millisecond metric.
	b.mu.Lock()
	b.lanes[laneKey(req.Principal, "")][0].queuedAt = time.Now().Add(-time.Second)
	b.mu.Unlock()
	s := b.Snapshot()
	if s.WorkerCount != 1 || s.QueuedCount != 1 || s.ActiveCount != 0 || s.OutstandingCount != 1 || s.Capacity != 11 || s.OldestQueuedAgeMS < 1000 || !s.IsAccepting {
		t.Fatalf("queued snapshot: %+v", s)
	}
	b.Start()
	ctx := <-p.started
	meta := requestctx.MetadataFromContext(ctx)
	if meta.RequestID != "req" || meta.OperationID != "operation" || meta.ParentOperationID != "parent" || requestctx.UsageCollectorFromContext(ctx) != usage {
		t.Fatal("broker lost telemetry context")
	}
	s = b.Snapshot()
	if s.ActiveCount != 1 || s.QueuedCount != 0 || s.OldestQueuedAgeMS != 0 {
		t.Fatalf("active snapshot: %+v", s)
	}
	close(p.release)
	result := <-req.ResponseChan
	if result.QueueWaitMS < 1000 || result.AgentDurationMS < 0 {
		t.Fatalf("timing: %+v", result)
	}
	b.Shutdown()
	s = b.Snapshot()
	if s.IsAccepting || s.ActiveCount != 0 || s.QueuedCount != 0 || s.OutstandingCount != 0 {
		t.Fatalf("stopped snapshot: %+v", s)
	}
}
