// Package lease keeps durable job ownership live while blocking work runs.
package lease

import (
	"context"
	"fmt"
	"time"
)

// Run renews a durable lease while work is active. Renewal failure cancels the
// work context and takes precedence over the work's resulting cancellation error.
func Run(ctx context.Context, lease time.Duration, renew func(context.Context) error, work func(context.Context) error) error {
	if lease <= 0 {
		return fmt.Errorf("lease heartbeat requires a positive duration")
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	done := make(chan error, 1)
	interval := lease / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				done <- nil
				return
			case <-workCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := renew(workCtx); err != nil {
					cancel()
					done <- fmt.Errorf("renew durable job lease: %w", err)
					return
				}
			}
		}
	}()

	workErr := work(workCtx)
	close(stop)
	renewErr := <-done
	if renewErr != nil {
		return renewErr
	}
	return workErr
}
