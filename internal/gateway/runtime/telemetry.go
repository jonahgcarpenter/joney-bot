package runtime

import (
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

// Delivery timing excludes indicators and durable post-delivery bookkeeping.
type measuredResponder struct {
	Responder
	durationMS int64
	status     string
}

func (r *measuredResponder) measure(send func() error) error {
	start := time.Now()
	err := send()
	r.durationMS += time.Since(start).Milliseconds()
	r.status = "ok"
	if err != nil {
		r.status = "error"
	}
	return err
}

func (r *measuredResponder) SendFallback(text string) error {
	return r.measure(func() error { return r.Responder.SendFallback(text) })
}
func (r *measuredResponder) SendAgentError(text string) error {
	return r.measure(func() error { return r.Responder.SendAgentError(text) })
}
func (r *measuredResponder) SendCommandResponse(result commands.Result) error {
	return r.measure(func() error { return r.Responder.SendCommandResponse(result) })
}
func (r *measuredResponder) SendAgentResponse(response *agent.Response) error {
	return r.measure(func() error { return r.Responder.SendAgentResponse(response) })
}
func (r *measuredResponder) CancelAgentResponse() error {
	if c, ok := r.Responder.(CancellationResponder); ok {
		return c.CancelAgentResponse()
	}
	return nil
}
