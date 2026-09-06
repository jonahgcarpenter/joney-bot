package lease

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRenewsUntilWorkCompletes(t *testing.T) {
	var renewals atomic.Int32
	err := Run(context.Background(), 9*time.Millisecond, func(context.Context) error {
		renewals.Add(1)
		return nil
	}, func(context.Context) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewals.Load() < 2 {
		t.Fatalf("renewals=%d, want at least 2", renewals.Load())
	}
}

func TestRunCancelsWorkWhenRenewalFails(t *testing.T) {
	renewErr := context.DeadlineExceeded
	err := Run(context.Background(), 3*time.Millisecond, func(context.Context) error {
		return renewErr
	}, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil || err.Error() == context.Canceled.Error() {
		t.Fatalf("error=%v, want lease renewal failure", err)
	}
}
