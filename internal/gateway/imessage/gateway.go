package imessage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
)

// Name returns the human-readable gateway name.
func (g *Gateway) Name() string {
	return "iMessage"
}

// Start initializes the BlueBubbles webhook listener.
func (g *Gateway) Start(b *broker.Broker) error {
	g.Broker = b
	log := g.log()
	if g.messageIndex == nil {
		g.messageIndex = make(map[string]messageContext)
	}
	if g.contactNames == nil {
		g.contactNames = make(map[string]contactNameCacheEntry)
	}
	g.refreshBlueBubblesCapabilitiesWithRetry(capabilityAttempts, capabilityRetryDelay)

	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, g.handleWebhook)

	log.Info("gateway.listen", "imessage gateway listening", config.F("port", g.Port), config.F("path", webhookPath))
	return http.ListenAndServe(":"+g.Port, mux)
}

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

// processIncomingMessage normalizes an inbound iMessage and routes it to the broker.
func (g *Gateway) processIncomingMessage(msg webhookMessage) {
	log := g.log()
	requestID := config.NewRequestID()
	chat := msg.primaryChat()
	if chat.GUID == "" {
		g.logIgnoredMessage("missing_chat_guid", "new-message", msg, config.F("request_id", requestID))
		return
	}
	if strings.TrimSpace(msg.Handle.Address) == "" {
		g.logIgnoredMessage("missing_sender", "new-message", msg, config.F("request_id", requestID))
		return
	}
	if strings.TrimSpace(msg.Text) == "" && len(msg.Attachments) == 0 {
		g.logIgnoredMessage("empty_text_and_no_attachments", "new-message", msg, config.F("request_id", requestID))
		return
	}

	text := strings.TrimSpace(msg.Text)
	replyGUID := msg.replyTargetGUID()
	isGroup := chat.Style == chatStyleGroup || strings.Contains(chat.GUID, ";+;")
	selectedMessageGUID := ""
	if isGroup {
		selectedMessageGUID = msg.GUID
	}
	mentionsBot := mentionRE.MatchString(text)
	textWithoutMention := strings.TrimSpace(mentionRE.ReplaceAllString(text, ""))
	currentIsCommandAttempt := routing.IsCommandAttempt(textWithoutMention)
	currentIsReplyToBot := false
	if replyCtx, ok := g.lookupMessage(replyGUID); ok {
		currentIsReplyToBot = replyCtx.IsFromBot
	}
	preflight := routing.Preflight(routing.PreflightInput{
		IsGroup:      isGroup,
		IsMention:    mentionsBot,
		IsReplyToBot: currentIsReplyToBot,
		Text:         textWithoutMention,
	})
	if preflight.Action == routing.ActionIgnore {
		g.logIgnoredMessage(preflight.Reason, "new-message", msg,
			config.F("request_id", requestID),
			config.F("is_group", isGroup),
			config.F("is_mention", mentionsBot),
			config.F("is_reply", replyGUID != ""),
			config.F("is_command", currentIsCommandAttempt),
			config.F("message_chars", len(msg.Text)),
		)
		return
	}

	images, unsupported := g.loadImages(msg.Attachments)
	if len(msg.Attachments) > 0 {
		log.Info("gateway.attachment.processed", "processed imessage attachments", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("accepted_count", len(images)), config.F("downgraded_count", len(unsupported)), config.F("declared_format_count", len(msg.Attachments)))
	}
	if strings.TrimSpace(msg.Text) == "" && len(images) == 0 {
		if len(unsupported) == 0 {
			g.logIgnoredMessage("no_supported_content", "new-message", msg, config.F("request_id", requestID))
			return
		}
	}

	if strings.TrimSpace(msg.Text) == "" && len(images) == 0 && len(unsupported) == 0 {
		g.logIgnoredMessage("no_supported_content", "new-message", msg, config.F("request_id", requestID))
		return
	}

	normalizedSenderID, err := accountlinking.NormalizeIdentifier("imessage", msg.Handle.Address)
	if err != nil {
		log.Error("gateway.account.normalize_failed", "failed to normalize imessage account", config.F("request_id", requestID), config.ErrorField(err))
		return
	}
	displayName := normalizedSenderID
	if resolvedName, err := g.lookupContactDisplayName(normalizedSenderID); err != nil {
		log.Debug("gateway.contact_lookup.failed", "imessage contact lookup failed", config.F("request_id", requestID), config.F("user_id", normalizedSenderID), config.F("status", "degraded"), config.ErrorField(err))
	} else if resolvedName != "" {
		displayName = resolvedName
	}

	canonicalUserID, err := g.Links.EnsureAccount("imessage", normalizedSenderID, displayName)
	if err != nil {
		log.Error("gateway.account.resolve_failed", "failed to resolve imessage account", config.F("request_id", requestID), config.F("user_id", normalizedSenderID), config.ErrorField(err))
		return
	}

	sessionKey := g.sessionKey(chat, normalizedSenderID)
	var reply *routing.ReplyContext
	if replyGUID != "" {
		if replyCtx, ok := g.lookupReplyContext(replyGUID, chat.GUID, sessionKey, requestID); ok {
			currentIsReplyToBot = replyCtx.IsFromBot
			replyName := strings.TrimSpace(replyCtx.DisplayName)
			if replyName == "" && replyCtx.IsFromBot {
				replyName = "Oswald"
			}
			reply = &routing.ReplyContext{
				SenderName: replyName,
				Text:       strings.TrimSpace(replyCtx.Text),
				IsFromBot:  replyCtx.IsFromBot,
			}
			if len(replyCtx.Attachments) > 0 {
				remainingImageSlots := media.MaxImagesPerRequest - len(images)
				if remainingImageSlots > 0 {
					reply.Images, reply.Unsupported = g.loadImagesLimit(replyCtx.Attachments, remainingImageSlots)
				} else {
					reply.Unsupported = attachmentLabels(replyCtx.Attachments)
				}
			}
			log.Debug("gateway.reply_context.applied", "applied imessage reply context", config.F("request_id", requestID), config.F("chat_id", chat.GUID), config.F("is_bot_reply", replyCtx.IsFromBot), config.F("reply_image_count", len(reply.Images)))
		} else {
			log.Debug("gateway.reply_context.applied", "imessage reply target missing from cache", config.F("request_id", requestID), config.F("status", "degraded"))
			reply = &routing.ReplyContext{IsUnavailable: true}
		}
	}
	g.rememberInboundMessage(msg, sessionKey, normalizedSenderID, displayName)
	g.startProcessingIndicators(chat.GUID, requestID)

	gatewayruntime.Execute(gatewayruntime.Request{
		RequestID: requestID,
		ChatID:    chat.GUID,
		Principal: identity.Principal{
			CanonicalUserID: canonicalUserID,
			Gateway:         "imessage",
			ExternalID:      normalizedSenderID,
			Assurance:       identity.AssuranceBlueBubblesWebhook,
		},
		DisplayName:  displayName,
		SessionKey:   sessionKey,
		IsDirect:     !isGroup,
		IsGroup:      isGroup,
		IsMention:    mentionsBot,
		IsReplyToBot: currentIsReplyToBot,
		Text:         textWithoutMention,
		Images:       images,
		Unsupported:  unsupported,
		Reply:        reply,
	}, g.runtimeDependencies(), &runtimeResponder{
		gateway:             g,
		requestID:           requestID,
		chatGUID:            chat.GUID,
		selectedMessageGUID: selectedMessageGUID,
		sessionKey:          sessionKey,
		senderID:            normalizedSenderID,
	})
}

func (g *Gateway) startProcessingIndicators(chatGUID, requestID string) {
	go func() {
		g.markRead(chatGUID)
		time.Sleep(typingAfterReadDelay)
		if err := g.startTyping(chatGUID); err != nil {
			g.log().Debug("gateway.typing.failed", "failed to start BlueBubbles typing indicator", config.F("request_id", requestID), config.F("chat_id", chatGUID), config.F("status", "degraded"), config.ErrorField(err))
		}
	}()
}

func (g *Gateway) refreshBlueBubblesCapabilitiesWithRetry(maxAttempts int, delay time.Duration) bool {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	log := g.log()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		loaded, available := g.refreshBlueBubblesCapabilities()
		if available {
			return true
		}
		if attempt < maxAttempts && delay > 0 {
			time.Sleep(delay)
		}
		if loaded {
			log.Debug("gateway.bluebubbles.capabilities_retry", "BlueBubbles private API/helper not ready", config.F("attempt", attempt), config.F("attempt_count", maxAttempts), config.F("status", "degraded"))
		}
	}
	log.Warn("gateway.bluebubbles.capabilities_unavailable", "BlueBubbles private API/helper unavailable after retries", config.F("attempt_count", maxAttempts), config.F("status", "degraded"))
	return false
}

func (g *Gateway) refreshBlueBubblesCapabilities() (bool, bool) {
	log := g.log()
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/server/info", g.BlueBubblesPassword)
	if err != nil {
		log.Warn("gateway.bluebubbles.capabilities_failed", "failed to build BlueBubbles server info request", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		log.Warn("gateway.bluebubbles.capabilities_failed", "failed to build BlueBubbles server info request", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}

	resp, err := g.httpClient().Do(req)
	if err != nil {
		log.Warn("gateway.bluebubbles.capabilities_failed", "failed to fetch BlueBubbles server info", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Warn("gateway.bluebubbles.capabilities_failed", "BlueBubbles server info failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return false, false
	}

	var result serverInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Warn("gateway.bluebubbles.capabilities_failed", "failed to decode BlueBubbles server info", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}

	g.capabilityMu.Lock()
	g.capabilitiesLoaded = true
	g.privateAPIEnabled = result.Data.PrivateAPI
	g.helperConnected = result.Data.HelperConnected
	g.capabilityMu.Unlock()

	log.Info("gateway.bluebubbles.capabilities", "resolved BlueBubbles capabilities", config.F("is_private_api_enabled", result.Data.PrivateAPI), config.F("is_helper_connected", result.Data.HelperConnected), config.F("status", "ok"))
	return true, result.Data.PrivateAPI && result.Data.HelperConnected
}

func (g *Gateway) blueBubblesPrivateAPIAvailable() bool {
	g.capabilityMu.Lock()
	if !g.capabilitiesLoaded {
		g.capabilityMu.Unlock()
		g.refreshBlueBubblesCapabilities()
		g.capabilityMu.Lock()
	}
	available := g.privateAPIEnabled && g.helperConnected
	g.capabilityMu.Unlock()
	return available
}

func (g *Gateway) runtimeDependencies() gatewayruntime.Dependencies {
	deps := g.Runtime
	deps.Broker = g.Broker
	if deps.Access == nil {
		deps.Access = g.Links
	}
	if deps.Log == nil {
		deps.Log = g.Log
	}
	return deps
}

func (g *Gateway) lookupContactDisplayName(normalizedSenderID string) (string, error) {
	if normalizedSenderID == "" {
		return "", nil
	}

	if cachedName, ok := g.cachedContactDisplayName(normalizedSenderID); ok {
		return cachedName, nil
	}

	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/contact/query", g.BlueBubblesPassword)
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(contactQueryRequest{Addresses: []string{normalizedSenderID}})
	if err != nil {
		return "", fmt.Errorf("marshal BlueBubbles contact query: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build BlueBubbles contact query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("send BlueBubbles contact query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.log().Warn("gateway.contact_lookup.failed", "BlueBubbles contact query failed", config.F("user_id", normalizedSenderID), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return "", fmt.Errorf("BlueBubbles contact query failed with status %d", resp.StatusCode)
	}

	var result contactQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode BlueBubbles contact query response: %w", err)
	}

	name := chooseContactDisplayName(result.Data)
	g.cacheContactDisplayName(normalizedSenderID, name)
	return name, nil
}

func chooseContactDisplayName(contacts []contactRecord) string {
	for _, contact := range contacts {
		if name := strings.TrimSpace(contact.DisplayName); name != "" {
			return name
		}
		fullName := strings.TrimSpace(strings.TrimSpace(contact.FirstName) + " " + strings.TrimSpace(contact.LastName))
		if fullName != "" {
			return fullName
		}
		if nickname := strings.TrimSpace(contact.Nickname); nickname != "" {
			return nickname
		}
	}
	return ""
}

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
		if normalizedSenderID, err := accountlinking.NormalizeIdentifier("imessage", address); err != nil {
			log.Debug("gateway.reply_lookup.normalize_failed", "failed to normalize imessage reply sender", config.F("request_id", requestID), config.F("message_guid", data.GUID), config.F("status", "degraded"), config.ErrorField(err))
		} else {
			ctx.SenderID = normalizedSenderID
			ctx.DisplayName = normalizedSenderID
			if resolvedName, err := g.lookupContactDisplayName(normalizedSenderID); err != nil {
				log.Debug("gateway.reply_lookup.contact_failed", "imessage reply contact lookup failed", config.F("request_id", requestID), config.F("user_id", normalizedSenderID), config.F("status", "degraded"), config.ErrorField(err))
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

func (g *Gateway) cachedContactDisplayName(normalizedSenderID string) (string, bool) {
	g.contactMu.Lock()
	defer g.contactMu.Unlock()
	g.pruneContactNamesLocked()
	entry, ok := g.contactNames[normalizedSenderID]
	if !ok {
		return "", false
	}
	return entry.DisplayName, true
}

func (g *Gateway) cacheContactDisplayName(normalizedSenderID, displayName string) {
	g.contactMu.Lock()
	defer g.contactMu.Unlock()
	g.pruneContactNamesLocked()
	g.contactNames[normalizedSenderID] = contactNameCacheEntry{
		DisplayName: displayName,
		ExpiresAt:   time.Now().Add(contactCacheTTL),
	}
}

func (g *Gateway) pruneContactNamesLocked() {
	now := time.Now()
	for senderID, entry := range g.contactNames {
		if !entry.ExpiresAt.After(now) {
			delete(g.contactNames, senderID)
		}
	}
}

func (g *Gateway) loadImages(attachments []attachment) ([]llm.InputImage, []string) {
	return g.loadImagesLimit(attachments, media.MaxImagesPerRequest)
}

func (g *Gateway) loadImagesLimit(attachments []attachment, maxImages int) ([]llm.InputImage, []string) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if maxImages <= 0 {
		return nil, attachmentLabels(attachments)
	}

	images := make([]llm.InputImage, 0, len(attachments))
	unsupported := make([]string, 0)
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.TransferName, attachment.MimeType)
		if len(images) >= maxImages {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.MimeType != "" && !media.LooksLikeImageMIME(attachment.MimeType) {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.TotalBytes > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := g.fetchAttachmentImage(attachment)
		if err != nil {
			g.log().Debug("gateway.attachment.rejected", "rejected imessage attachment", config.F("attachment_id", attachment.GUID), config.F("declared_mime", strings.TrimSpace(attachment.MimeType)), config.F("status", "degraded"))
			unsupported = append(unsupported, label)
			continue
		}
		if image.Data == "" {
			unsupported = append(unsupported, label)
			continue
		}
		images = append(images, image)
	}

	if len(images) == 0 {
		return nil, unsupported
	}
	return images, unsupported
}

func attachmentLabels(attachments []attachment) []string {
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		labels = append(labels, media.AttachmentLabel(attachment.TransferName, attachment.MimeType))
	}
	return labels
}

func (g *Gateway) fetchAttachmentImage(attachment attachment) (llm.InputImage, error) {
	if strings.TrimSpace(attachment.GUID) == "" {
		return llm.InputImage{}, nil
	}

	endpoint, err := buildBlueBubblesAttachmentEndpoint(g.BlueBubblesURL, attachment.GUID, g.BlueBubblesPassword)
	if err != nil {
		return llm.InputImage{}, err
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("build BlueBubbles attachment request: %w", err)
	}

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("download BlueBubbles attachment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.log().Warn("gateway.attachment.fetch_failed", "failed to fetch imessage attachment", config.F("attachment_id", attachment.GUID), config.F("declared_mime", strings.TrimSpace(attachment.MimeType)), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return llm.InputImage{}, fmt.Errorf("download BlueBubbles attachment failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("read BlueBubbles attachment: %w", err)
	}
	if len(body) > media.MaxImageBytes {
		return llm.InputImage{}, fmt.Errorf("attachment exceeds %d bytes", media.MaxImageBytes)
	}

	result, err := media.NormalizeInputImageFromBytes(resp.Header, attachment.MimeType, body, attachment.TransferName)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("attachment rejected: %w", err)
	}
	g.log().Debug("gateway.attachment.normalized", "normalized imessage attachment", config.F("attachment_id", attachment.GUID), config.F("declared_mime", strings.TrimSpace(attachment.MimeType)), config.F("detected_mime", result.DetectedMIME), config.F("normalized_mime", result.Image.MimeType), config.F("attachment_bytes", len(body)), config.F("original_width", result.OriginalWidth), config.F("original_height", result.OriginalHeight), config.F("width", result.Width), config.F("height", result.Height), config.F("is_resized", result.WasResized), config.F("normalized_bytes", result.NormalizedBytes), config.F("base64_chars", result.Base64Chars), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))

	image := result.Image
	return image, nil
}

// sessionKey returns the session identifier for a direct or group iMessage chat.
func (g *Gateway) sessionKey(chat messageChat, normalizedSenderID string) string {
	if chat.Style == chatStyleDirect {
		return "imessage:dm:" + normalizedSenderID
	}
	return "imessage:" + chat.GUID + ":" + normalizedSenderID
}

// startTyping enables the typing indicator for the given chat.
func (g *Gateway) startTyping(chatGUID string) error {
	return g.sendTypingRequest(chatGUID)
}

// sendTypingRequest sends a BlueBubbles typing request for the given chat.
func (g *Gateway) sendTypingRequest(chatGUID string) error {
	if !g.blueBubblesPrivateAPIAvailable() {
		return nil
	}
	endpoint, err := buildBlueBubblesChatActionEndpoint(g.BlueBubblesURL, chatGUID, "typing", g.BlueBubblesPassword)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build BlueBubbles typing request: %w", err)
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("send BlueBubbles typing request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.log().Debug("gateway.typing.failed", "BlueBubbles typing request failed", config.F("chat_id", chatGUID), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return fmt.Errorf("BlueBubbles typing request failed with status %d", resp.StatusCode)
	}
	return nil
}

// markRead sends a read receipt for the given chat when BlueBubbles supports it.
func (g *Gateway) markRead(chatGUID string) {
	if !g.blueBubblesPrivateAPIAvailable() {
		return
	}
	endpoint, err := buildBlueBubblesChatActionEndpoint(g.BlueBubblesURL, chatGUID, "read", g.BlueBubblesPassword)
	if err != nil {
		g.log().Debug("gateway.read_receipt.failed", "failed to build BlueBubbles read receipt request", config.F("chat_id", chatGUID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		g.log().Debug("gateway.read_receipt.failed", "failed to build BlueBubbles read receipt request", config.F("chat_id", chatGUID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		g.log().Debug("gateway.read_receipt.failed", "failed to send BlueBubbles read receipt", config.F("chat_id", chatGUID), config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.log().Debug("gateway.read_receipt.failed", "BlueBubbles read receipt failed", config.F("chat_id", chatGUID), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
	}
}

// sendTextReply sends a text reply, retrying with the fallback method if needed.
func (g *Gateway) sendTextReply(chatGUID, text, selectedMessageGUID string, partIndex int) (string, error) {
	if strings.TrimSpace(selectedMessageGUID) == "" || !g.blueBubblesPrivateAPIAvailable() {
		return g.sendText(chatGUID, text, "", 0, "")
	}

	messageGUID, err := g.sendText(chatGUID, text, selectedMessageGUID, partIndex, defaultSendMethod)
	if err == nil {
		return messageGUID, nil
	}
	g.log().Warn("gateway.send.retry", "retrying imessage send without private reply fields", config.F("chat_id", chatGUID), config.F("default_method", defaultSendMethod), config.F("status", "retry"), config.ErrorField(err))
	return g.sendText(chatGUID, text, "", 0, "")
}

// sendText posts a text message to BlueBubbles and returns the created message GUID.
func (g *Gateway) sendText(chatGUID, text, selectedMessageGUID string, partIndex int, method string) (string, error) {
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/message/text", g.BlueBubblesPassword)
	if err != nil {
		return "", err
	}

	payload := sendTextRequest{
		ChatGUID:            chatGUID,
		Message:             text,
		Method:              method,
		SelectedMessageGUID: selectedMessageGUID,
		PartIndex:           partIndex,
		TempGUID:            newTempGUID(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal BlueBubbles send payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build BlueBubbles request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("send BlueBubbles request: %w", err)
	}
	defer resp.Body.Close()

	var result sendTextResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode BlueBubbles send response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		g.log().Warn("gateway.send.provider_failed", "BlueBubbles send failed", config.F("chat_id", chatGUID), config.F("method", method), config.F("http_status", resp.StatusCode), config.F("has_provider_error", result.Error != nil), config.F("status", "error"))
		return "", fmt.Errorf("BlueBubbles send failed with status %d", resp.StatusCode)
	}
	return result.Data.GUID, nil
}

// sendCommandAttachment sends one in-memory attachment through BlueBubbles.
func (g *Gateway) sendCommandAttachment(chatGUID string, attachment commands.Attachment) error {
	if err := attachment.Validate(); err != nil {
		return err
	}
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/message/attachment", g.BlueBubblesPassword)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"chatGuid": chatGUID,
		"name":     attachment.Filename,
		"tempGuid": newTempGUID(),
	} {
		if err := writer.WriteField(name, value); err != nil {
			return fmt.Errorf("write BlueBubbles attachment field: %w", err)
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "attachment", "filename": attachment.Filename}))
	header.Set("Content-Type", attachment.MIMEType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create BlueBubbles attachment part: %w", err)
	}
	if _, err := part.Write(attachment.Data); err != nil {
		return fmt.Errorf("write BlueBubbles attachment: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close BlueBubbles attachment payload: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("build BlueBubbles attachment send request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("send BlueBubbles attachment request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("BlueBubbles attachment send failed with status %d", resp.StatusCode)
	}
	return nil
}

// buildBlueBubblesEndpoint constructs an authenticated BlueBubbles REST endpoint.
func buildBlueBubblesEndpoint(baseURL, path, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	query := parsed.Query()
	query.Set("password", password)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildBlueBubblesMessageEndpoint(baseURL, messageGUID, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseEscapedPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath + "/api/v1/message/" + messageGUID
	parsed.RawPath = baseEscapedPath + "/api/v1/message/" + url.PathEscape(messageGUID)
	query := parsed.Query()
	query.Set("password", password)
	query.Set("with", "chats,participants,attachment,handle")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildBlueBubblesAttachmentEndpoint(baseURL, attachmentGUID, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseEscapedPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath + "/api/v1/attachment/" + attachmentGUID + "/download"
	parsed.RawPath = baseEscapedPath + "/api/v1/attachment/" + url.PathEscape(attachmentGUID) + "/download"
	query := parsed.Query()
	query.Set("password", password)
	query.Set("original", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildBlueBubblesChatActionEndpoint(baseURL, chatGUID, action, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseEscapedPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath + "/api/v1/chat/" + chatGUID + "/" + action
	parsed.RawPath = baseEscapedPath + "/api/v1/chat/" + url.PathEscape(chatGUID) + "/" + url.PathEscape(action)
	query := parsed.Query()
	query.Set("password", password)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
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

// primaryChat returns the first chat attached to the webhook payload.
func (m webhookMessage) primaryChat() messageChat {
	if len(m.Chats) == 0 {
		return messageChat{}
	}
	return m.Chats[0]
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

// newTempGUID returns a temporary GUID for outbound BlueBubbles send requests.
func newTempGUID() string {
	return fmt.Sprintf("oswald-%d", time.Now().UnixNano())
}
