package broker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestSubmitRejectsWhenQueueFull(t *testing.T) {
	b := NewBroker(nil, 0, config.NewLogger(config.LevelError))
	defer b.Shutdown()

	for i := 0; i < requestQueueSize+b.workerCount; i++ {
		req := &Request{RequestID: "queued", ResponseChan: make(chan Result, 1)}
		b.Submit(req)
		select {
		case result := <-req.ResponseChan:
			t.Fatalf("queued request %d received unexpected immediate result: %+v", i, result)
		default:
		}
	}

	rejected := &Request{RequestID: "rejected", ResponseChan: make(chan Result, 1)}
	b.Submit(rejected)

	select {
	case result := <-rejected.ResponseChan:
		if result.Response == nil || result.Response.Response != "request rejected: broker queue full" {
			t.Fatalf("rejected result = %+v, want queue-full fallback", result)
		}
	default:
		t.Fatal("full queue did not reject immediately")
	}
}

func TestRejectedLaneOperationReleasesReaderReservation(t *testing.T) {
	b := NewBroker(nil, 1, config.NewLogger(config.LevelError))
	defer b.Shutdown()
	for i := 0; i < requestQueueSize+b.workerCount; i++ {
		principal := identity.Principal{CanonicalUserID: fmt.Sprintf("filler-%d", i), Gateway: "homeassistant", ExternalID: fmt.Sprintf("filler-%d", i), Assurance: identity.AssuranceHomeAssistantToken}
		if err := b.Submit(&Request{Principal: principal, SessionKey: "session", ResponseChan: make(chan Result, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	user := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	if err := b.RunInLane(context.Background(), user, "session", func() error { return nil }); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("rejected lane error = %v", err)
	}
	b.mu.Lock()
	fence := b.userFences["user"]
	b.mu.Unlock()
	fence.mu.Lock()
	readers := fence.readers
	fence.mu.Unlock()
	if readers != 0 {
		t.Fatalf("rejected lane leaked %d reader reservations", readers)
	}
}

func TestNewBrokerUsesAtLeastOneWorker(t *testing.T) {
	b := NewBroker(&captureProcessor{requests: make(chan agent.Request, 1)}, 0, config.NewLogger(config.LevelError))
	if b.workerCount != 1 {
		t.Fatalf("workerCount = %d, want 1", b.workerCount)
	}
}

func TestLowPriorityPermitYieldsToAcceptedForegroundWork(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 1)}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	if _, _, ok := b.TryAcquireLowPriority(context.Background()); ok {
		t.Fatal("low-priority permit acquired before broker start")
	}
	b.Start()
	defer b.Shutdown()

	permitCtx, release, ok := b.TryAcquireLowPriority(context.Background())
	if !ok {
		t.Fatal("low-priority permit was not acquired while idle")
	}
	defer release()
	if _, _, ok := b.TryAcquireLowPriority(context.Background()); ok {
		t.Fatal("second low-priority permit acquired concurrently")
	}

	req := &Request{
		RequestID: "foreground", Principal: identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken},
		SessionKey: "session", ResponseChan: make(chan Result, 1),
	}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-permitCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("accepted foreground work did not preempt low-priority permit")
	}
	if _, _, ok := b.TryAcquireLowPriority(context.Background()); ok {
		t.Fatal("low-priority permit acquired while foreground work or canceled permit remained active")
	}
	release()
	select {
	case <-req.ResponseChan:
	case <-time.After(time.Second):
		t.Fatal("foreground request did not complete")
	}
	if ctx, nextRelease, ok := b.TryAcquireLowPriority(context.Background()); !ok || ctx.Err() != nil {
		t.Fatal("low-priority permit did not recover after foreground completion")
	} else {
		nextRelease()
	}
}

func TestDeliverResultDoesNotBlockOrPanic(t *testing.T) {
	full := make(chan Result, 1)
	full <- Result{}
	if deliverResult(full, Result{}) {
		t.Fatal("delivery to full channel succeeded")
	}

	closed := make(chan Result, 1)
	close(closed)
	if deliverResult(closed, Result{}) {
		t.Fatal("delivery to closed channel succeeded")
	}
}

func TestWorkerForwardsPrincipalToProcessor(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 1)}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()

	principal := identity.Principal{CanonicalUserID: "usr_1", Gateway: "homeassistant", ExternalID: "alice", Assurance: identity.AssuranceHomeAssistantToken}
	req := &Request{
		RequestID:    "req-1",
		Principal:    principal,
		DisplayName:  "Alice",
		SessionKey:   "homeassistant:alice",
		Prompt:       "hello",
		ResponseChan: make(chan Result, 1),
	}
	b.Submit(req)

	select {
	case result := <-req.ResponseChan:
		if result.Err != nil || result.Response == nil || result.Response.Response != "ok" {
			t.Fatalf("unexpected result: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broker result")
	}

	select {
	case got := <-processor.requests:
		if got.Principal != principal || got.RequestID != req.RequestID || got.SessionKey != req.SessionKey || got.DisplayName != req.DisplayName || got.Prompt != req.Prompt {
			t.Fatalf("forwarded request = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for processor request")
	}
}

func TestBrokerSerializesSameLaneFIFO(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- b.RunInLane(context.Background(), principal, "session", func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted
	go func() {
		done <- b.RunInLane(context.Background(), principal, "session", func() error {
			close(secondStarted)
			return nil
		})
	}()
	select {
	case <-secondStarted:
		t.Fatal("second same-lane operation started before first completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second same-lane operation did not start")
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestBrokerRunsDifferentLanesInParallel(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	started := make(chan string, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, sessionID := range []string{"one", "two"} {
		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			_ = b.RunInLane(context.Background(), principal, sessionID, func() error {
				started <- sessionID
				<-release
				return nil
			})
		}(sessionID)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different lanes did not run concurrently")
		}
	}
	close(release)
	wg.Wait()
}

func TestBrokerHotLaneDoesNotOccupyOtherWorker(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), principal, "hot", func() error { close(firstStarted); <-release; return nil })
	}()
	<-firstStarted
	secondHotStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), principal, "hot", func() error { close(secondHotStarted); return nil })
	}()
	otherStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), principal, "other", func() error { close(otherStarted); return nil })
	}()
	select {
	case <-otherStarted:
	case <-time.After(time.Second):
		t.Fatal("hot-lane follower occupied worker needed by another lane")
	}
	select {
	case <-secondHotStarted:
		t.Fatal("hot-lane follower started early")
	default:
	}
	close(release)
}

func TestBrokerSameSessionDifferentUsersRunInParallel(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	started := make(chan string, 2)
	release := make(chan struct{})
	for _, userID := range []string{"one", "two"} {
		principal := identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: userID, Assurance: identity.AssuranceHomeAssistantToken}
		go func() {
			_ = b.RunInLane(context.Background(), principal, "shared", func() error { started <- userID; <-release; return nil })
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different users with shared session ID did not run concurrently")
		}
	}
	close(release)
}

func TestBrokerUserExclusiveFencesAllUserSessions(t *testing.T) {
	b := NewBroker(nil, 4, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	activeStarted := make(chan struct{}, 2)
	releaseActive := make(chan struct{})
	for _, sessionID := range []string{"one", "two"} {
		go func() {
			_ = b.RunInLane(context.Background(), principal, sessionID, func() error {
				activeStarted <- struct{}{}
				<-releaseActive
				return nil
			})
		}()
	}
	<-activeStarted
	<-activeStarted
	exclusiveStarted := make(chan struct{})
	releaseExclusive := make(chan struct{})
	go func() {
		_ = b.RunUsersExclusive(context.Background(), []string{principal.CanonicalUserID}, func() error {
			close(exclusiveStarted)
			<-releaseExclusive
			return nil
		})
	}()
	select {
	case <-exclusiveStarted:
		t.Fatal("exclusive work overlapped active same-user sessions")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseActive)
	select {
	case <-exclusiveStarted:
	case <-time.After(time.Second):
		t.Fatal("exclusive work did not start after active sessions drained")
	}
	laterStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), principal, "three", func() error { close(laterStarted); return nil })
	}()
	select {
	case <-laterStarted:
		t.Fatal("new same-user work overlapped exclusive work")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseExclusive)
	select {
	case <-laterStarted:
	case <-time.After(time.Second):
		t.Fatal("same-user work did not resume after exclusive work")
	}
}

func TestBrokerUserExclusiveDoesNotOvertakeAcceptedReader(t *testing.T) {
	b := NewBroker(nil, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	blocker := identity.Principal{CanonicalUserID: "blocker", Gateway: "homeassistant", ExternalID: "blocker", Assurance: identity.AssuranceHomeAssistantToken}
	user := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	releaseBlocker := make(chan struct{})
	blockerStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), blocker, "session", func() error {
			close(blockerStarted)
			<-releaseBlocker
			return nil
		})
	}()
	<-blockerStarted
	order := make(chan string, 2)
	go func() {
		_ = b.RunInLane(context.Background(), user, "accepted-first", func() error { order <- "reader"; return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		accepted := b.outstanding >= 2
		b.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		_ = b.RunUsersExclusive(context.Background(), []string{user.CanonicalUserID}, func() error { order <- "exclusive"; return nil })
	}()
	deadline = time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		fence := b.userFences["user"]
		b.mu.Unlock()
		fence.mu.Lock()
		writerPending := fence.pendingWriters > 0
		fence.mu.Unlock()
		if writerPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exclusive writer was not reserved")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseBlocker)
	if first := <-order; first != "reader" {
		t.Fatalf("first work = %q, want accepted reader", first)
	}
	if second := <-order; second != "exclusive" {
		t.Fatalf("second work = %q, want exclusive", second)
	}
}

func TestBrokerUserExclusiveDoesNotOvertakeSameLaneFollower(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	user := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	headStarted := make(chan struct{})
	releaseHead := make(chan struct{})
	order := make(chan string, 2)
	go func() {
		_ = b.RunInLane(context.Background(), user, "same", func() error {
			close(headStarted)
			<-releaseHead
			return nil
		})
	}()
	<-headStarted
	go func() {
		_ = b.RunInLane(context.Background(), user, "same", func() error { order <- "follower"; return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		accepted := b.outstanding >= 2
		b.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lane follower was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		_ = b.RunUsersExclusive(context.Background(), []string{user.CanonicalUserID}, func() error { order <- "exclusive"; return nil })
	}()
	deadline = time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		fence := b.userFences["user"]
		b.mu.Unlock()
		fence.mu.Lock()
		writerPending := fence.pendingWriters > 0
		fence.mu.Unlock()
		if writerPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exclusive writer was not reserved")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseHead)
	if first := <-order; first != "follower" {
		t.Fatalf("first work = %q, want lane follower", first)
	}
	if second := <-order; second != "exclusive" {
		t.Fatalf("second work = %q, want exclusive", second)
	}
}

func TestBrokerTransfersRefreshedPrincipalFence(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 1)}
	b := NewBroker(processor, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	winner := identity.Principal{CanonicalUserID: "winner", Gateway: "homeassistant", ExternalID: "winner", Assurance: identity.AssuranceHomeAssistantToken}
	loser := identity.Principal{CanonicalUserID: "loser", Gateway: "homeassistant", ExternalID: "loser", Assurance: identity.AssuranceHomeAssistantToken}
	exclusiveStarted := make(chan struct{})
	releaseExclusive := make(chan struct{})
	go func() {
		_ = b.RunUsersExclusive(context.Background(), []string{winner.CanonicalUserID}, func() error {
			close(exclusiveStarted)
			<-releaseExclusive
			return nil
		})
	}()
	<-exclusiveStarted
	req := &Request{
		Principal:  loser,
		SessionKey: "session",
		RefreshPrincipal: func(principal identity.Principal) (identity.Principal, error) {
			principal.CanonicalUserID = "winner"
			return principal, nil
		},
		ResponseChan: make(chan Result, 1),
	}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.requests:
		t.Fatal("refreshed request overlapped winner-exclusive work")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseExclusive)
	select {
	case processed := <-processor.requests:
		if processed.Principal.CanonicalUserID != "winner" {
			t.Fatalf("processed principal = %q", processed.Principal.CanonicalUserID)
		}
	case <-time.After(time.Second):
		t.Fatal("refreshed request did not resume")
	}
}

func TestBrokerRechecksPrincipalAfterWaitingForRefreshedFence(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 1)}
	b := NewBroker(processor, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	winner := identity.Principal{CanonicalUserID: "winner", Gateway: "homeassistant", ExternalID: "winner", Assurance: identity.AssuranceHomeAssistantToken}
	loser := identity.Principal{CanonicalUserID: "loser", Gateway: "homeassistant", ExternalID: "loser", Assurance: identity.AssuranceHomeAssistantToken}
	exclusiveStarted := make(chan struct{})
	releaseExclusive := make(chan struct{})
	go func() {
		_ = b.RunUsersExclusive(context.Background(), []string{winner.CanonicalUserID}, func() error {
			close(exclusiveStarted)
			<-releaseExclusive
			return nil
		})
	}()
	<-exclusiveStarted
	refreshCount := 0
	req := &Request{
		Principal:  loser,
		SessionKey: "session",
		RefreshPrincipal: func(principal identity.Principal) (identity.Principal, error) {
			refreshCount++
			if refreshCount == 1 {
				principal.CanonicalUserID = "winner"
				return principal, nil
			}
			return identity.Principal{}, errors.New("account erased while queued")
		},
		ResponseChan: make(chan Result, 1),
	}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	close(releaseExclusive)
	select {
	case result := <-req.ResponseChan:
		if result.Err == nil {
			t.Fatal("stale refreshed principal reached processor")
		}
	case <-time.After(time.Second):
		t.Fatal("stale refreshed request did not finish")
	}
	select {
	case <-processor.requests:
		t.Fatal("processor received erased principal")
	default:
	}
}

func TestBrokerUserExclusiveDoesNotFenceOtherUsersAndReleasesAfterPanic(t *testing.T) {
	b := NewBroker(nil, 3, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	user := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	other := identity.Principal{CanonicalUserID: "other", Gateway: "homeassistant", ExternalID: "other", Assurance: identity.AssuranceHomeAssistantToken}
	exclusiveStarted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = b.RunUsersExclusive(context.Background(), []string{user.CanonicalUserID}, func() error { close(exclusiveStarted); <-release; return nil })
	}()
	<-exclusiveStarted
	otherStarted := make(chan struct{})
	go func() {
		_ = b.RunInLane(context.Background(), other, "session", func() error { close(otherStarted); return nil })
	}()
	select {
	case <-otherStarted:
	case <-time.After(time.Second):
		t.Fatal("exclusive work blocked a different user")
	}
	close(release)
	if err := b.RunUsersExclusive(context.Background(), []string{user.CanonicalUserID}, func() error { panic("boom") }); err == nil {
		t.Fatal("exclusive panic was not returned as an error")
	}
	if err := b.RunInLane(context.Background(), user, "after", func() error { return nil }); err != nil {
		t.Fatalf("user fence remained locked after panic: %v", err)
	}
}

func TestBrokerUsersExclusiveUsesStableOrderAndFencesEveryUser(t *testing.T) {
	b := NewBroker(nil, 4, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	a := identity.Principal{CanonicalUserID: "a", Gateway: "homeassistant", ExternalID: "a", Assurance: identity.AssuranceHomeAssistantToken}
	bUser := identity.Principal{CanonicalUserID: "b", Gateway: "homeassistant", ExternalID: "b", Assurance: identity.AssuranceHomeAssistantToken}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	go func() {
		_ = b.RunUsersExclusive(context.Background(), []string{"b", "a", "b"}, func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted

	blocked := make(chan struct{}, 2)
	go func() {
		_ = b.RunInLane(context.Background(), a, "a-session", func() error { blocked <- struct{}{}; return nil })
	}()
	go func() {
		_ = b.RunInLane(context.Background(), bUser, "b-session", func() error { blocked <- struct{}{}; return nil })
	}()
	select {
	case <-blocked:
		t.Fatal("normal work overlapped a multi-user exclusive operation")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		select {
		case <-blocked:
		case <-time.After(time.Second):
			t.Fatal("fenced work did not resume")
		}
	}

	done := make(chan error, 2)
	go func() {
		done <- b.RunUsersExclusive(context.Background(), []string{"a", "b"}, func() error { return nil })
	}()
	go func() {
		done <- b.RunUsersExclusive(context.Background(), []string{"b", "a"}, func() error { return nil })
	}()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("opposite target order deadlocked")
		}
	}
}

func TestBrokerUsersExclusiveReleasesAllFencesAfterPanic(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	if err := b.RunUsersExclusive(context.Background(), []string{"b", "a"}, func() error { panic("boom") }); err == nil {
		t.Fatal("exclusive panic was not returned")
	}
	for _, userID := range []string{"a", "b"} {
		principal := identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: userID, Assurance: identity.AssuranceHomeAssistantToken}
		if err := b.RunInLane(context.Background(), principal, "after", func() error { return nil }); err != nil {
			t.Fatalf("fence %s remained locked: %v", userID, err)
		}
	}
}

func TestBrokerShutdownDrainsLaneFollowers(t *testing.T) {
	b := NewBroker(nil, 2, config.NewLogger(config.LevelError))
	b.Start()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan string, 2)
	go func() {
		_ = b.RunInLane(context.Background(), principal, "session", func() error { close(firstStarted); <-release; completed <- "first"; return nil })
	}()
	<-firstStarted
	go func() {
		_ = b.RunInLane(context.Background(), principal, "session", func() error { completed <- "second"; return nil })
	}()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		outstanding := b.outstanding
		b.mu.Unlock()
		if outstanding == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second lane operation was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	shutdownDone := make(chan struct{})
	go func() { b.Shutdown(); close(shutdownDone) }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before accepted lane work drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain lane followers")
	}
	if first, second := <-completed, <-completed; first != "first" || second != "second" {
		t.Fatalf("completion order=%q,%q", first, second)
	}
}

func TestBrokerRejectsAfterShutdown(t *testing.T) {
	b := NewBroker(nil, 1, config.NewLogger(config.LevelError))
	b.Start()
	b.Shutdown()
	req := &Request{RequestID: "late", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(req); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("late submit error=%v", err)
	}
	select {
	case result := <-req.ResponseChan:
		if result.Response == nil || result.Response.Response != "request rejected: broker shutting down" {
			t.Fatalf("late result=%+v", result)
		}
	default:
		t.Fatal("late submit did not receive immediate result")
	}
}

func TestBrokerShutdownCancelsActiveAgentRequest(t *testing.T) {
	processor := &cancelProcessor{started: make(chan struct{})}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	req := &Request{RequestID: "active", Principal: principal, SessionKey: "session", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	<-processor.started
	done := make(chan struct{})
	go func() { b.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel active agent request")
	}
	result := <-req.ResponseChan
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("result error=%v, want context cancellation", result.Err)
	}
}

func TestCancelActiveAgentWorkPreservesQueuedRequest(t *testing.T) {
	processor := &sequencedCancelProcessor{started: make(chan string, 2)}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	active := &Request{RequestID: "active", Principal: principal, SessionKey: "session", ResponseChan: make(chan Result, 1)}
	queued := &Request{RequestID: "queued", Principal: principal, SessionKey: "session", ResponseChan: make(chan Result, 1)}
	if err := b.Submit(active); err != nil {
		t.Fatal(err)
	}
	if err := b.Submit(queued); err != nil {
		t.Fatal(err)
	}
	if got := <-processor.started; got != "active" {
		t.Fatalf("first request = %q", got)
	}
	report := b.CancelActiveAgentWork("user", "session")
	if report.ActiveSignaled != 1 || report.QueuedCanceled != 0 {
		t.Fatalf("report = %+v", report)
	}
	if result := <-active.ResponseChan; !errors.Is(result.Err, ErrAgentWorkCanceled) {
		t.Fatalf("active error = %v", result.Err)
	}
	if got := <-processor.started; got != "queued" {
		t.Fatalf("second request = %q", got)
	}
	select {
	case result := <-queued.ResponseChan:
		if result.Err != nil || result.Response == nil || result.Response.Response != "queued complete" {
			t.Fatalf("queued result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request did not run after active cancellation")
	}
}

func TestCancelAllAgentWorkImmediatelyCompletesQueuedRequests(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 2)}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	defer b.Shutdown()
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	requests := []*Request{
		{RequestID: "one", Principal: principal, SessionKey: "one", ResponseChan: make(chan Result, 1)},
		{RequestID: "two", Principal: principal, SessionKey: "two", ResponseChan: make(chan Result, 1)},
	}
	for _, req := range requests {
		if err := b.Submit(req); err != nil {
			t.Fatal(err)
		}
	}
	report := b.CancelAllAgentWork()
	if report.ActiveSignaled != 0 || report.QueuedCanceled != 2 {
		t.Fatalf("report = %+v", report)
	}
	for _, req := range requests {
		if result := <-req.ResponseChan; !errors.Is(result.Err, ErrAgentWorkCanceled) {
			t.Fatalf("%s error = %v", req.RequestID, result.Err)
		}
	}
	b.Start()
	select {
	case req := <-processor.requests:
		t.Fatalf("canceled queued request reached processor: %+v", req)
	case <-time.After(50 * time.Millisecond):
	}
}

type captureProcessor struct {
	requests chan agent.Request
}

type cancelProcessor struct {
	started chan struct{}
}

type sequencedCancelProcessor struct {
	started chan string
}

func (p *sequencedCancelProcessor) Process(ctx context.Context, req agent.Request) (*agent.Response, error) {
	p.started <- req.RequestID
	if req.RequestID == "active" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &agent.Response{Response: "queued complete"}, nil
}

func (p *cancelProcessor) Process(ctx context.Context, _ agent.Request) (*agent.Response, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *captureProcessor) Process(_ context.Context, req agent.Request) (*agent.Response, error) {
	p.requests <- req
	return &agent.Response{Response: "ok"}, nil
}
