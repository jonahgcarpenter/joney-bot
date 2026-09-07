package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type discordHTTPError int

type discordRateLimitError struct {
	retryAfter time.Duration
}

func (e discordRateLimitError) Error() string { return discordHTTPError(429).Error() }
func (e discordRateLimitError) Unwrap() error { return discordHTTPError(429) }

func discordNonce() string {
	id := config.NewRequestID()
	return id[:min(len(id), 25)]
}

func (e discordHTTPError) Error() string { return fmt.Sprintf("discord HTTP status %d", e) }

func transientDelivery(err error) bool {
	var status discordHTTPError
	if errors.As(err, &status) && (status == 408 || status == 429 || status >= 500) {
		return true
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

type outboundEntry struct {
	state    *discordStreamState
	response *agent.Response
	ctx      context.Context
	cancel   context.CancelFunc
	result   chan error
	bytes    int
	started  time.Time
}

// deliverFinal waits for actual delivery, preserving runtime's existing acknowledgement.
func (dg *Gateway) deliverFinal(state *discordStreamState, response *agent.Response) error {
	bytes := 0
	for _, a := range response.Attachments {
		bytes += len(a.Data)
	}
	dg.outboundMu.Lock()
	if dg.outboundClosed || len(dg.outbound) >= 20 || bytes > 80<<20-dg.outboundBytes {
		dg.outboundMu.Unlock()
		dg.log().Info("gateway.outbound.rejected", "discord outbound capacity unavailable", config.F("request_id", state.stream.responder.requestID), config.F("status", "rejected"))
		return errors.New("discord outbound capacity unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	e := &outboundEntry{state: state, response: response, ctx: ctx, cancel: cancel, result: make(chan error, 1), bytes: bytes, started: time.Now()}
	dg.outbound = append(dg.outbound, e)
	dg.outboundBytes += bytes
	if dg.outboundDone == nil {
		workerCtx, workerCancel := context.WithCancel(context.Background())
		dg.outboundCancel = workerCancel
		dg.outboundDone = make(chan struct{})
		go dg.runOutbound(workerCtx, dg.outboundDone)
	}
	dg.log().Info("gateway.outbound.admitted", "admitted discord final delivery", config.F("request_id", state.stream.responder.requestID), config.F("pending_count", len(dg.outbound)), config.F("attachment_bytes", dg.outboundBytes))
	dg.outboundMu.Unlock()
	defer cancel()
	select {
	case err := <-e.result:
		return err
	case <-ctx.Done():
		// The worker owns an active entry until its canceled HTTP attempt returns.
		// Waiting entries can expire independently without retaining their payloads.
		dg.completeOutbound(e, ctx.Err(), true)
		return <-e.result
	}
}

func (dg *Gateway) completeOutbound(e *outboundEntry, err error, waitingOnly bool) {
	dg.outboundMu.Lock()
	defer dg.outboundMu.Unlock()
	if waitingOnly && dg.outboundActive == e {
		return
	}
	for i, pending := range dg.outbound {
		if pending != e {
			continue
		}
		copy(dg.outbound[i:], dg.outbound[i+1:])
		dg.outbound[len(dg.outbound)-1] = nil
		dg.outbound = dg.outbound[:len(dg.outbound)-1]
		dg.outboundBytes -= e.bytes
		if dg.outboundActive == e {
			dg.outboundActive = nil
		}
		status := "ok"
		outcome := "delivered"
		if err != nil {
			status = "error"
			outcome = "failed"
		}
		if errors.Is(err, context.Canceled) {
			status, outcome = "ok", "canceled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = "expired"
		}
		dg.log().Info("gateway.outbound.complete", "completed discord final delivery", config.F("record_kind", "measurement"), config.F("request_id", e.state.stream.responder.requestID), config.F("status", status), config.F("outcome", outcome), config.F("duration_ms", time.Since(e.started).Milliseconds()), config.F("pending_count", len(dg.outbound)), config.ErrorField(err))
		e.result <- err
		return
	}
}

func (dg *Gateway) runOutbound(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		dg.outboundMu.Lock()
		if len(dg.outbound) == 0 {
			dg.outbound = nil
			dg.outboundDone = nil
			dg.outboundMu.Unlock()
			return
		}
		e := dg.outbound[0]
		dg.outboundActive = e
		dg.outboundMu.Unlock()
		delay := time.Second
		var err error
		for {
			attemptCtx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			stop := context.AfterFunc(ctx, cancel)
			e.state.rest = &Gateway{Token: dg.Token, APIBaseURL: dg.APIBaseURL, HTTPClient: dg.HTTPClient, Log: dg.Log, deliveryContext: attemptCtx}
			err = attemptCtx.Err()
			if err == nil {
				err = e.state.finish(e.response)
			}
			stop()
			cancel()
			if e.ctx.Err() != nil {
				err = e.ctx.Err()
			}
			if err == nil || !transientDelivery(err) || e.ctx.Err() != nil || ctx.Err() != nil {
				break
			}
			wait := delay
			var rateLimit discordRateLimitError
			if errors.As(err, &rateLimit) {
				wait = max(wait, rateLimit.retryAfter)
			}
			deadline, _ := e.ctx.Deadline()
			wait = min(wait, max(0, time.Until(deadline)))
			dg.log().Info("gateway.outbound.retry", "retrying discord final delivery", config.F("request_id", e.state.stream.responder.requestID), config.F("status", "retry"), config.F("delay_ms", wait.Milliseconds()))
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-e.ctx.Done():
			case <-ctx.Done():
			}
			timer.Stop()
			if e.ctx.Err() != nil {
				err = e.ctx.Err()
				break
			}
			if ctx.Err() != nil {
				err = ctx.Err()
				break
			}
			// The wait timer and context deadline can fire together, before Err updates.
			if !time.Now().Before(deadline) {
				err = context.DeadlineExceeded
				break
			}
			delay = min(delay*2, 30*time.Second)
		}
		dg.completeOutbound(e, err, false)
	}
}

// StopOutbound cancels queued delivery and joins its worker; it does not stop the websocket.
func (dg *Gateway) StopOutbound() {
	dg.outboundMu.Lock()
	dg.outboundClosed = true
	for _, e := range dg.outbound {
		e.cancel()
	}
	if dg.outboundCancel != nil {
		dg.outboundCancel()
	}
	done := dg.outboundDone
	dg.outboundMu.Unlock()
	if done != nil {
		<-done
	}
}
