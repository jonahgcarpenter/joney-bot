package imessage

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

type runtimeResponder struct {
	gateway             *Gateway
	requestID           string
	chatGUID            string
	selectedMessageGUID string
	sessionKey          string
	senderID            string
}

func (r *runtimeResponder) StartProcessing() (func(), error) {
	return nil, nil
}

func (r *runtimeResponder) SendFallback(text string) error {
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) SendCommandResponse(result commands.Result) error {
	if err := result.ValidateAttachments(); err != nil {
		return err
	}
	attachments := result.Attachments
	if len(attachments) == 0 {
		return r.sendAndRemember(result.Text)
	}
	for _, attachment := range attachments {
		if err := r.gateway.sendCommandAttachment(r.chatGUID, attachment); err != nil {
			return err
		}
	}
	if strings.TrimSpace(result.Text) != "" {
		return r.sendAndRemember(result.Text)
	}
	return nil
}

func (r *runtimeResponder) SendAgentError(text string) error {
	return r.sendAndRemember(text)
}

func (r *runtimeResponder) CancelAgentResponse() error { return nil }

func (r *runtimeResponder) SendAgentResponse(response *agent.Response) error {
	if response == nil {
		return nil
	}
	if err := media.ValidateOutputAttachments(response.Attachments); err != nil {
		return err
	}
	for _, attachment := range response.Attachments {
		if err := r.gateway.sendCommandAttachment(r.chatGUID, attachment); err != nil {
			return err
		}
	}
	responseText := strings.TrimSpace(response.Response)
	if responseText == "" {
		r.gateway.log().Debug("gateway.response.empty", "imessage agent returned empty response", config.F("request_id", r.requestID), config.F("chat_id", r.chatGUID), config.F("status", "degraded"))
		return nil
	}
	return r.sendAndRemember(responseText)
}

func (r *runtimeResponder) sendAndRemember(text string) error {
	messageGUID, err := r.gateway.sendTextReply(r.chatGUID, text, r.selectedMessageGUID, 0)
	if err != nil {
		return err
	}
	r.gateway.log().Debug("gateway.response.sent", "sent imessage response", config.F("request_id", r.requestID), config.F("chat_id", r.chatGUID), config.F("response_chars", len(text)), config.F("status", "ok"))
	r.gateway.rememberBotMessage(messageGUID, r.sessionKey, r.chatGUID, r.senderID, text)
	return nil
}
