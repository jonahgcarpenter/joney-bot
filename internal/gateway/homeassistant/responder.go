package homeassistant

import (
	"fmt"
	"mime"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

type runtimeResponder struct {
	connection *trackedConnection
	requestID  string
	mu         sync.Mutex
	terminal   bool
}

func (r *runtimeResponder) StartProcessing() (func(), error) { return nil, nil }

func (r *runtimeResponder) SendFallback(text string) error { return r.sendResult(text, "", nil) }

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	text, supported, err := commandResponseText(result)
	if err != nil {
		return err
	}
	if !supported {
		return r.sendError("command_attachments_unsupported", "Home Assistant does not support command attachments.")
	}
	return r.sendResult(text, "", nil)
}

func commandResponseText(result commands.Result) (string, bool, error) {
	if err := result.ValidateAttachments(); err != nil {
		return "", false, err
	}
	attachments := result.Attachments
	if len(attachments) == 0 {
		return result.Text, true, nil
	}

	var text strings.Builder
	for _, attachment := range attachments {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(attachment.MIMEType))
		if err != nil {
			return "", false, err
		}
		if !strings.HasPrefix(mediaType, "text/") || !utf8.Valid(attachment.Data) {
			return "", false, nil
		}
		_, _ = text.Write(attachment.Data)
	}
	return text.String(), true, nil
}

func (r *runtimeResponder) SendAgentError(_ string) error {
	return r.sendError("request_failed", "Oswald could not process the request.")
}

func (r *runtimeResponder) CancelAgentResponse() error {
	return r.sendError("request_canceled", "The request was canceled.")
}

func (r *runtimeResponder) SendAgentResponse(response *agent.AgentResponse) error {
	if response == nil {
		return r.sendError("request_failed", "Oswald returned no response.")
	}
	if response.Error != "" {
		return r.sendError("request_failed", "Oswald could not process the request.")
	}
	if len(response.Attachments) > 0 {
		return r.sendError("agent_attachments_unsupported", "Home Assistant does not support generated attachments.")
	}
	return r.sendResult(response.Response, response.Model, response.Metrics)
}

func (r *runtimeResponder) sendResult(response, model string, metrics *agent.ModelMetrics) error {
	return r.sendTerminal(protocolMessage{Type: "result", RequestID: r.requestID, Response: response, Model: model, Metrics: metrics})
}

func (r *runtimeResponder) sendError(code, message string) error {
	return r.sendTerminal(protocolMessage{Type: "error", RequestID: r.requestID, Code: code, Message: message})
}

func (r *runtimeResponder) sendTerminal(message protocolMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminal {
		return fmt.Errorf("home assistant terminal response already sent")
	}
	r.terminal = true
	return r.connection.writeJSON(message)
}
