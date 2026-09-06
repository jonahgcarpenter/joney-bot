package maintenanceruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type blockingSweeper struct {
	mu         sync.Mutex
	calls      int
	active     int
	overlapped bool
	started    chan struct{}
}

func (s *blockingSweeper) MaintenanceSweep(ctx context.Context, _ time.Time, _ config.RetentionPolicy) (usermemory.MaintenanceCounts, error) {
	s.mu.Lock()
	s.calls++
	s.active++
	if s.active > 1 {
		s.overlapped = true
	}
	if s.calls == 1 {
		close(s.started)
	}
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return usermemory.MaintenanceCounts{}, ctx.Err()
}

func TestServiceImmediateSweepDoesNotOverlapAndStops(t *testing.T) {
	sweeper := &blockingSweeper{started: make(chan struct{})}
	service := NewService(sweeper, config.RetentionPolicy{MaintenanceInterval: time.Millisecond}, config.NewLogger(config.LevelError))
	service.Start(context.Background())
	select {
	case <-sweeper.started:
	case <-time.After(time.Second):
		t.Fatal("immediate maintenance sweep did not start")
	}
	time.Sleep(5 * time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		service.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("maintenance service did not stop")
	}
	sweeper.mu.Lock()
	defer sweeper.mu.Unlock()
	if sweeper.calls != 1 || sweeper.overlapped || sweeper.active != 0 {
		t.Fatalf("calls=%d active=%d overlapped=%v", sweeper.calls, sweeper.active, sweeper.overlapped)
	}
}

type recoveringSweeper struct {
	calls chan int
	count int
}

func (s *recoveringSweeper) MaintenanceSweep(context.Context, time.Time, config.RetentionPolicy) (usermemory.MaintenanceCounts, error) {
	s.count++
	select {
	case s.calls <- s.count:
	default:
	}
	if s.count == 1 {
		return usermemory.MaintenanceCounts{}, errors.New("transient cleanup failure")
	}
	return usermemory.MaintenanceCounts{}, nil
}

func TestServiceRepeatsAfterSweepFailure(t *testing.T) {
	sweeper := &recoveringSweeper{calls: make(chan int, 8)}
	service := NewService(sweeper, config.RetentionPolicy{MaintenanceInterval: time.Millisecond}, config.NewLogger(config.LevelError))
	service.Start(context.Background())
	defer service.Stop()
	for want := 1; want <= 3; want++ {
		select {
		case got := <-sweeper.calls:
			if got != want {
				t.Fatalf("sweep=%d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("maintenance did not reach sweep %d after failure", want)
		}
	}
}
