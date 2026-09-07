package discord

import (
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type runtimeResponder struct {
	gateway    *Gateway
	requestID  string
	channelID  string
	replyToID  string
	sessionKey string
	authorID   string
	stream     *discordResponseStream
	typingMu   sync.Mutex
	stopTyping chan struct{}
	typingOnce sync.Once
}

func newRuntimeResponder(gateway *Gateway, requestID, channelID, replyToID, sessionKey, authorID string) *runtimeResponder {
	r := &runtimeResponder{
		gateway: gateway, requestID: requestID, channelID: channelID,
		replyToID: replyToID, sessionKey: sessionKey, authorID: authorID,
	}
	r.stream = newDiscordResponseStream(r)
	return r
}

func (r *runtimeResponder) StartProcessing() (func(), error) {
	log := r.gateway.log().With(config.F("request_id", r.requestID))
	stopTyping := make(chan struct{})
	r.typingMu.Lock()
	r.stopTyping = stopTyping
	r.typingMu.Unlock()
	go func() {
		r.typingMu.Lock()
		select {
		case <-stopTyping:
			r.typingMu.Unlock()
			return
		default:
			_ = r.gateway.sendTyping(r.channelID, log)
			r.typingMu.Unlock()
		}

		ticker := time.NewTicker(9 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				r.typingMu.Lock()
				select {
				case <-stopTyping:
					r.typingMu.Unlock()
					return
				default:
					_ = r.gateway.sendTyping(r.channelID, log)
					r.typingMu.Unlock()
				}
			case <-stopTyping:
				return
			}
		}
	}()
	return r.stopTypingIndicator, nil
}

func (r *runtimeResponder) stopTypingIndicator() {
	r.typingOnce.Do(func() {
		r.typingMu.Lock()
		defer r.typingMu.Unlock()
		if r.stopTyping != nil {
			close(r.stopTyping)
		}
	})
}

func (r *runtimeResponder) Stream(chunk agent.StreamChunk) {
	r.stream.Push(chunk)
}

func (r *runtimeResponder) SendFallback(text string) error {
	return r.stream.Finish(&agent.Response{Response: text})
}

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	if err := result.ValidateAttachments(); err != nil {
		return err
	}
	return r.stream.Finish(&agent.Response{Response: result.Text, Attachments: result.Attachments})
}

func (r *runtimeResponder) SendAgentError(text string) error {
	return r.stream.Fail(text)
}

func (r *runtimeResponder) CancelAgentResponse() error {
	return r.stream.Abort()
}

func (r *runtimeResponder) SendAgentResponse(response *agent.Response) error {
	if response == nil {
		return nil
	}
	return r.stream.Finish(response)
}
