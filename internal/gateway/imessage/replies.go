package imessage

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func (g *Gateway) lookupReplyContext(replyGUID, chatGUID, sessionKey, requestID string) (messageContext, bool) {
	if replyCtx, ok := g.lookupMessage(replyGUID); ok {
		return replyCtx, true
	}

	replyCtx, ok := g.fetchReplyContext(replyGUID, chatGUID, sessionKey, requestID)
	if !ok {
		return messageContext{}, false
	}
	g.rememberMessage(replyGUID, replyCtx)
	return replyCtx, true
}

func (g *Gateway) fetchReplyContext(replyGUID, chatGUID, sessionKey, requestID string) (messageContext, bool) {
	log := g.log()
	if strings.TrimSpace(replyGUID) == "" {
		return messageContext{}, false
	}

	if data, ok := g.queryReplyMessage(replyGUID, requestID); ok {
		return g.replyContextFromMessage(data, chatGUID, sessionKey, requestID), true
	}

	endpoint, err := buildBlueBubblesMessageEndpoint(g.BlueBubblesURL, replyGUID, g.BlueBubblesPassword)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to build imessage reply lookup request", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageContext{}, false
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to build imessage reply lookup request", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageContext{}, false
	}

	resp, err := g.httpClient().Do(req)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to fetch imessage reply target", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageContext{}, false
	}
	defer resp.Body.Close()

	var result messageLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to decode imessage reply target", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageContext{}, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if result.Error != nil {
			log.Debug("gateway.reply_lookup.failed", "BlueBubbles reply lookup failed", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("http_status", resp.StatusCode), config.F("has_provider_error", true), config.F("status", "degraded"))
		} else {
			log.Debug("gateway.reply_lookup.failed", "BlueBubbles reply lookup failed", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("http_status", resp.StatusCode), config.F("status", "degraded"))
		}
		return messageContext{}, false
	}

	return g.replyContextFromMessage(result.Data, chatGUID, sessionKey, requestID), true
}

func (g *Gateway) queryReplyMessage(replyGUID, requestID string) (messageLookupData, bool) {
	log := g.log()
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/message/query", g.BlueBubblesPassword)
	if err != nil {
		log.Debug("gateway.reply_lookup.query_failed", "failed to build imessage reply query request", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageLookupData{}, false
	}

	payload := messageQueryRequest{
		Limit:  1,
		Offset: 0,
		With:   []string{"chat", "attachment", "handle"},
		Where: []messageQueryClause{
			{
				Statement: "message.guid = :guid",
				Args: map[string]string{
					"guid": replyGUID,
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Debug("gateway.reply_lookup.query_failed", "failed to marshal imessage reply query", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageLookupData{}, false
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		log.Debug("gateway.reply_lookup.query_failed", "failed to build imessage reply query request", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageLookupData{}, false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		log.Debug("gateway.reply_lookup.query_failed", "failed to query imessage reply target", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageLookupData{}, false
	}
	defer resp.Body.Close()

	var result messageQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Debug("gateway.reply_lookup.query_failed", "failed to decode imessage reply query", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"), config.ErrorField(err))
		return messageLookupData{}, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if result.Error != nil {
			log.Debug("gateway.reply_lookup.query_failed", "BlueBubbles reply query failed", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("http_status", resp.StatusCode), config.F("has_provider_error", true), config.F("status", "degraded"))
		} else {
			log.Debug("gateway.reply_lookup.query_failed", "BlueBubbles reply query failed", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("http_status", resp.StatusCode), config.F("status", "degraded"))
		}
		return messageLookupData{}, false
	}
	if len(result.Data) == 0 {
		log.Debug("gateway.reply_lookup.query_miss", "BlueBubbles reply query returned no messages", config.F("request_id", requestID), config.F("message_guid", replyGUID), config.F("status", "degraded"))
		return messageLookupData{}, false
	}
	return result.Data[0], true
}

func (g *Gateway) replyContextFromMessage(data messageLookupData, chatGUID, sessionKey, requestID string) messageContext {
	log := g.log()
	ctx := messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    chatGUID,
		Text:        strings.TrimSpace(data.Text),
		Attachments: data.Attachments,
		IsFromBot:   data.IsFromMe,
		CreatedAt:   time.Now(),
	}
	if len(data.Chats) > 0 && data.Chats[0].GUID != "" {
		ctx.ChatGUID = data.Chats[0].GUID
	}
	if data.IsFromMe {
		ctx.SenderID = "imessage:self"
		ctx.DisplayName = "Oswald"
		log.Debug("gateway.reply_lookup.fetched", "fetched imessage reply target", config.F("request_id", requestID), config.F("message_guid", data.GUID), config.F("is_bot_reply", true), config.F("attachment_count", len(ctx.Attachments)), config.F("status", "ok"))
		return ctx
	}

	address := strings.TrimSpace(data.Handle.Address)
	ctx.SenderID = address
	ctx.DisplayName = address
	if address != "" {
		if normalizedSenderID, err := accounts.NormalizeIdentifier("imessage", address); err != nil {
			log.Debug("gateway.reply_lookup.normalize_failed", "failed to normalize imessage reply sender", config.F("request_id", requestID), config.F("message_guid", data.GUID), config.F("status", "degraded"), config.ErrorField(err))
		} else {
			ctx.SenderID = normalizedSenderID
			ctx.DisplayName = normalizedSenderID
			if resolvedName, err := g.lookupContactDisplayName(normalizedSenderID, log.With(config.F("request_id", requestID))); err != nil {
				log.Debug("gateway.reply_lookup.contact_failed", "imessage reply contact lookup failed", config.F("request_id", requestID), config.F("status", "degraded"), config.ErrorField(err))
			} else if resolvedName != "" {
				ctx.DisplayName = resolvedName
			}
		}
	}
	if strings.TrimSpace(ctx.DisplayName) == "" {
		ctx.DisplayName = "someone"
	}
	log.Debug("gateway.reply_lookup.fetched", "fetched imessage reply target", config.F("request_id", requestID), config.F("message_guid", data.GUID), config.F("is_bot_reply", false), config.F("attachment_count", len(ctx.Attachments)), config.F("status", "ok"))
	return ctx
}

// rememberInboundMessage caches inbound message context for reply reconstruction.
func (g *Gateway) rememberInboundMessage(msg webhookMessage, sessionKey, normalizedSenderID, displayName string) {
	if msg.GUID == "" {
		return
	}
	g.rememberMessage(msg.GUID, messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    msg.primaryChat().GUID,
		SenderID:    normalizedSenderID,
		DisplayName: displayName,
		Text:        strings.TrimSpace(msg.Text),
		Attachments: msg.Attachments,
		IsFromBot:   false,
		CreatedAt:   time.Now(),
	})
}

// rememberBotMessage caches bot-authored message context for reply reconstruction.
func (g *Gateway) rememberBotMessage(messageGUID, sessionKey, chatGUID, senderID, text string) {
	g.rememberMessage(messageGUID, messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    chatGUID,
		SenderID:    senderID,
		DisplayName: "Oswald",
		Text:        strings.TrimSpace(text),
		IsFromBot:   true,
		CreatedAt:   time.Now(),
	})
}

// rememberMessage stores reply context in the in-memory message index.
func (g *Gateway) rememberMessage(messageGUID string, ctx messageContext) {
	if messageGUID == "" {
		return
	}
	g.messageMu.Lock()
	defer g.messageMu.Unlock()
	g.pruneMessageIndexLocked()
	g.messageIndex[messageGUID] = ctx
}

// lookupMessage returns cached reply context for a prior message GUID.
func (g *Gateway) lookupMessage(messageGUID string) (messageContext, bool) {
	if messageGUID == "" {
		return messageContext{}, false
	}
	g.messageMu.Lock()
	defer g.messageMu.Unlock()
	g.pruneMessageIndexLocked()
	ctx, ok := g.messageIndex[messageGUID]
	return ctx, ok
}

// pruneMessageIndexLocked removes expired entries from the in-memory message index.
func (g *Gateway) pruneMessageIndexLocked() {
	cutoff := time.Now().Add(-messageIndexTTL)
	for guid, ctx := range g.messageIndex {
		if ctx.CreatedAt.Before(cutoff) {
			delete(g.messageIndex, guid)
		}
	}
}

// replyTargetGUID returns the GUID of the message this inbound message references.
func (m webhookMessage) replyTargetGUID() string {
	if m.ThreadOriginatorGUID != "" {
		return m.ThreadOriginatorGUID
	}
	if m.ReplyToGUID != "" {
		return m.ReplyToGUID
	}
	return ""
}
