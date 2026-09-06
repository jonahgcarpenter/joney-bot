package broker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// runOperationForUsers evaluates Done only after enqueue succeeds. This context
// exposes that boundary without canceling accepted work or polling broker state.
type operationAcceptedContext struct {
	context.Context
	accepted chan struct{}
	once     sync.Once
}

func (c *operationAcceptedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.accepted) })
	return c.Context.Done()
}

func startAcceptedOperation(t *testing.T, run func(context.Context) error) <-chan error {
	t.Helper()
	ctx := &operationAcceptedContext{Context: context.Background(), accepted: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	awaitBrokerEvent(t, ctx.accepted)
	return done
}

func awaitBrokerEvent[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for broker event")
		var zero T
		return zero
	}
}

func TestRunUsersExclusiveRejectionReleasesAllWriterReservations(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "queue_full"
		if shutdown {
			name = "shutdown"
		}
		t.Run(name, func(t *testing.T) {
			capacity := requestQueueSize + 1
			processor := &captureProcessor{requests: make(chan agent.Request, capacity)}
			b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
			defer b.Shutdown()
			user := identity.Principal{CanonicalUserID: "a", ExternalID: "a", Gateway: "homeassistant", Assurance: identity.AssuranceHomeAssistantToken}
			var requests []*Request
			for range capacity {
				req := &Request{Principal: user, SessionKey: "filler", ResponseChan: make(chan Result, 1)}
				if err := b.Submit(req); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, req)
			}
			wantErr, wantReaders := ErrQueueFull, capacity
			if shutdown {
				b.Shutdown()
				wantErr, wantReaders = ErrShuttingDown, 0
			}
			called := false
			err := b.RunUsersExclusive(context.Background(), []string{"c", "a", "b", "a"}, func() error {
				called = true
				return nil
			})
			if !errors.Is(err, wantErr) || called {
				t.Fatalf("exclusive rejection: err=%v called=%t, want %v without execution", err, called, wantErr)
			}
			for _, userID := range []string{"a", "b", "c"} {
				b.mu.Lock()
				fence := b.userFences[userID]
				b.mu.Unlock()
				if fence == nil {
					t.Fatalf("missing fence for %s", userID)
				}
				fence.mu.Lock()
				readers, pending, writer := fence.readers, fence.pendingWriters, fence.writer
				fence.mu.Unlock()
				expectedReaders := 0
				if userID == "a" {
					expectedReaders = wantReaders
				}
				if readers != expectedReaders || pending != 0 || writer {
					t.Fatalf("%s fence: readers=%d pending=%d writer=%t, want readers=%d and no writers", userID, readers, pending, writer, expectedReaders)
				}
			}
			if shutdown {
				return
			}
			b.Start()
			for _, req := range requests {
				result := awaitBrokerEvent(t, req.ResponseChan)
				if result.Err != nil || !result.ExecutionComplete || result.Response == nil || result.Response.Response != "ok" {
					t.Fatalf("filler result = %+v", result)
				}
			}
			b.workWG.Wait()
			for _, userID := range []string{"a", "b", "c"} {
				user.CanonicalUserID, user.ExternalID = userID, userID
				resumed := false
				done := startAcceptedOperation(t, func(ctx context.Context) error {
					return b.RunInLane(ctx, user, "after-rejection", func() error { resumed = true; return nil })
				})
				if err := awaitBrokerEvent(t, done); err != nil || !resumed {
					t.Fatalf("normal work for %s after rejection: err=%v resumed=%t", userID, err, resumed)
				}
			}
		})
	}
}

func TestShutdownBeforeStartReleasesPendingMultiUserWriterReservations(t *testing.T) {
	b := NewBroker(nil, 1, config.NewLogger(config.LevelError))
	defer b.Shutdown()
	called := make(chan struct{}, 1)
	done := startAcceptedOperation(t, func(ctx context.Context) error {
		return b.RunUsersExclusive(ctx, []string{"b", "a", "b"}, func() error {
			called <- struct{}{}
			return nil
		})
	})
	for _, userID := range []string{"a", "b"} {
		fence := b.userFences[userID]
		fence.mu.Lock()
		pending := fence.pendingWriters
		fence.mu.Unlock()
		if pending != 1 {
			t.Fatalf("%s pending writers before shutdown = %d, want 1", userID, pending)
		}
	}
	b.Shutdown()
	if err := awaitBrokerEvent(t, done); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("pending exclusive result = %v, want shutdown rejection", err)
	}
	select {
	case <-called:
		t.Fatal("pending exclusive operation executed without workers")
	default:
	}
	for _, userID := range []string{"a", "b"} {
		fence := b.userFences[userID]
		fence.mu.Lock()
		readers, pending, writer := fence.readers, fence.pendingWriters, fence.writer
		fence.mu.Unlock()
		if readers != 0 || pending != 0 || writer {
			t.Fatalf("%s fence after shutdown: readers=%d pending=%d writer=%t", userID, readers, pending, writer)
		}
	}
	if snapshot := b.Snapshot(); snapshot.IsAccepting || snapshot.OutstandingCount != 0 {
		t.Fatalf("shutdown left pending work: %+v", snapshot)
	}
}

func TestCancelAllAgentWorkPreservesCommandsAndFencesUntilProcessorReturns(t *testing.T) {
	processor := &telemetryProcessor{started: make(chan context.Context, 2), release: make(chan struct{})}
	b := NewBroker(processor, 3, config.NewLogger(config.LevelError))
	b.Start()
	releaseCommand, releaseExclusive := make(chan struct{}), make(chan struct{})
	var processorOnce, commandOnce, exclusiveOnce sync.Once
	defer func() {
		processorOnce.Do(func() { close(processor.release) })
		commandOnce.Do(func() { close(releaseCommand) })
		exclusiveOnce.Do(func() { close(releaseExclusive) })
		b.Shutdown()
	}()
	user := identity.Principal{CanonicalUserID: "a", ExternalID: "a", Gateway: "homeassistant", Assurance: identity.AssuranceHomeAssistantToken}
	active := &Request{RequestID: "active", Principal: user, SessionKey: "session", ResponseChan: make(chan Result, 2)}
	queued := &Request{RequestID: "queued", Principal: user, SessionKey: "session", ResponseChan: make(chan Result, 2)}
	if err := b.Submit(active); err != nil {
		t.Fatal(err)
	}
	processorCtx := awaitBrokerEvent(t, processor.started)
	if err := b.Submit(queued); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	activeWork := b.lanes[laneKey(user, "session")][0]
	b.mu.Unlock()
	order := make(chan string, 3)
	commandDone := startAcceptedOperation(t, func(ctx context.Context) error {
		return b.RunInLane(ctx, user, "session", func() error {
			b.mu.Lock()
			finished := activeWork.executionFinished
			b.mu.Unlock()
			if !finished {
				t.Error("scheduled command started before canceled processor returned")
			}
			order <- "command"
			<-releaseCommand
			return nil
		})
	})
	b.mu.Lock()
	commandWork := b.lanes[laneKey(user, "session")][2]
	b.mu.Unlock()
	exclusiveDone := startAcceptedOperation(t, func(ctx context.Context) error {
		return b.RunUsersExclusive(ctx, []string{"b", "a"}, func() error {
			b.mu.Lock()
			finished := commandWork.executionFinished
			b.mu.Unlock()
			if !finished {
				t.Error("exclusive work overtook the accepted scheduled command")
			}
			order <- "exclusive"
			<-releaseExclusive
			return nil
		})
	})
	b.mu.Lock()
	exclusiveWork := b.lanes[LaneKey{CanonicalUserID: "a", UserExclusive: true}][0]
	b.mu.Unlock()
	other := user
	other.CanonicalUserID, other.ExternalID = "b", "b"
	laterDone := startAcceptedOperation(t, func(ctx context.Context) error {
		return b.RunInLane(ctx, other, "later", func() error {
			b.mu.Lock()
			finished := exclusiveWork.executionFinished
			b.mu.Unlock()
			if !finished {
				t.Error("later command started before multi-user exclusive work returned")
			}
			order <- "later"
			return nil
		})
	})
	if report := b.CancelAllAgentWork(); report != (CancelReport{ActiveSignaled: 1, QueuedCanceled: 1}) {
		t.Fatalf("cancel report = %+v", report)
	}
	awaitBrokerEvent(t, processorCtx.Done())
	if cause := context.Cause(processorCtx); !errors.Is(cause, ErrAgentWorkCanceled) {
		t.Fatalf("processor cancellation cause = %v", cause)
	}
	for i, req := range []*Request{active, queued} {
		result := awaitBrokerEvent(t, req.ResponseChan)
		if !errors.Is(result.Err, ErrAgentWorkCanceled) || result.Response != nil || result.ExecutionComplete != (i == 1) {
			t.Fatalf("%s immediate cancellation = %+v", req.RequestID, result)
		}
	}
	if report := b.CancelAllAgentWork(); report != (CancelReport{}) {
		t.Fatalf("repeated cancellation affected preserved work: %+v", report)
	}
	// Immediate cancellation must not retire the active lane or its reader fence.
	b.mu.Lock()
	lane := b.lanes[laneKey(user, "session")]
	retained := len(lane) == 3 && lane[0] == activeWork && !activeWork.executionFinished
	outstanding := b.outstanding
	fence := b.userFences["a"]
	b.mu.Unlock()
	fence.mu.Lock()
	readers, pending, writer := fence.readers, fence.pendingWriters, fence.writer
	fence.mu.Unlock()
	if !retained || outstanding != 5 || readers != 3 || pending != 1 || writer {
		t.Fatalf("cancellation released work early: retained=%t outstanding=%d readers=%d pending=%d writer=%t", retained, outstanding, readers, pending, writer)
	}
	select {
	case got := <-order:
		t.Fatalf("%s ran while canceled processor was still held", got)
	default:
	}
	processorOnce.Do(func() { close(processor.release) })
	for _, want := range []string{"command", "exclusive", "later"} {
		if got := awaitBrokerEvent(t, order); got != want {
			t.Fatalf("execution order: got %s, want %s", got, want)
		}
		select {
		case got := <-order:
			t.Fatalf("%s overlapped held %s work", got, want)
		default:
		}
		switch want {
		case "command":
			commandOnce.Do(func() { close(releaseCommand) })
		case "exclusive":
			exclusiveOnce.Do(func() { close(releaseExclusive) })
		}
	}
	for _, done := range []<-chan error{commandDone, exclusiveDone, laterDone} {
		if err := awaitBrokerEvent(t, done); err != nil {
			t.Fatalf("preserved command failed: %v", err)
		}
	}
	b.workWG.Wait()
	select {
	case <-processor.started:
		t.Fatal("canceled queued agent request reached processor")
	default:
	}
	for _, req := range []*Request{active, queued} {
		select {
		case result := <-req.ResponseChan:
			t.Fatalf("%s delivered a second result: %+v", req.RequestID, result)
		default:
		}
	}
	if snapshot := b.Snapshot(); !snapshot.IsAccepting || snapshot.OutstandingCount != 0 {
		t.Fatalf("broker did not remain accepting and drain preserved work: %+v", snapshot)
	}
}
