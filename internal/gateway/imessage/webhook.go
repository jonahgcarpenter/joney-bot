package imessage

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// handleWebhook validates and dispatches incoming BlueBubbles webhook events.
func (g *Gateway) handleWebhook(w http.ResponseWriter, r *http.Request) {
	log := g.log()
	if r.Method != http.MethodPost {
		g.logIgnoredMessage("invalid_method", "", webhookMessage{}, config.F("method", r.Method))
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !g.validWebhookCredential(r) {
		g.logIgnoredMessage("invalid_webhook_credential", "", webhookMessage{})
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.logIgnoredMessage("webhook_body_read_failed", "", webhookMessage{}, config.ErrorField(err))
		log.Warn("gateway.webhook.read_failed", "failed to read imessage webhook body", config.ErrorField(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var event webhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		g.logIgnoredMessage("webhook_decode_failed", "", webhookMessage{}, config.ErrorField(err))
		log.Warn("gateway.webhook.decode_failed", "failed to decode imessage webhook body", config.ErrorField(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	eventType := strings.TrimSpace(strings.ToLower(event.Type))
	switch eventType {
	case "new-message":
		if event.Data.IsFromMe {
			g.logIgnoredMessage("self_authored", eventType, event.Data)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if isTapbackMessage(event.Data) {
			g.logIgnoredMessage("tapback", eventType, event.Data)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !hasMessageContent(event.Data) {
			g.logIgnoredMessage("no_message_content", eventType, event.Data)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		go g.processIncomingMessage(event.Data)
		w.WriteHeader(http.StatusAccepted)
	case "typing-indicator":
		g.logIgnoredMessage("typing_indicator", eventType, event.Data)
		w.WriteHeader(http.StatusNoContent)
	default:
		g.logIgnoredMessage("unsupported_event_type", eventType, event.Data)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (g *Gateway) logIgnoredMessage(reason, eventType string, msg webhookMessage, fields ...config.Field) {
	chat := msg.primaryChat()
	baseFields := []config.Field{
		config.F("reason", reason),
		config.F("event_type", eventType),
		config.F("message_guid", msg.GUID),
		config.F("chat_id", chat.GUID),
		config.F("user_id", strings.TrimSpace(msg.Handle.Address)),
		config.F("is_from_me", msg.IsFromMe),
		config.F("associated_type", associatedMessageTypeString(msg.AssociatedMessageType)),
		config.F("has_text", strings.TrimSpace(msg.Text) != ""),
		config.F("attachment_count", len(msg.Attachments)),
	}
	baseFields = append(baseFields, fields...)
	g.log().Debug("gateway.message.ignored", "ignored imessage message", baseFields...)
}

func (g *Gateway) validWebhookCredential(r *http.Request) bool {
	password := strings.TrimSpace(g.BlueBubblesPassword)
	if password == "" {
		return true
	}
	token := strings.TrimSpace(r.URL.Query().Get("password"))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("guid"))
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-password"))
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-guid"))
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-bluebubbles-guid"))
	}
	return token == password
}

func hasMessageContent(msg webhookMessage) bool {
	return strings.TrimSpace(msg.Text) != "" || len(msg.Attachments) > 0
}

func isTapbackMessage(msg webhookMessage) bool {
	associatedType, ok := associatedMessageTypeInt(msg.AssociatedMessageType)
	if !ok {
		return false
	}
	return (associatedType >= 2000 && associatedType <= 2005) || (associatedType >= 3000 && associatedType <= 3005)
}

func associatedMessageTypeInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func associatedMessageTypeString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}
