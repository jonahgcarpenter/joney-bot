package imessage

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// processIncomingMessage normalizes an inbound iMessage and routes it to the broker.
func (g *Gateway) processIncomingMessage(msg webhookMessage) {
	g.processReceivedMessage(msg, config.NewRequestID(), time.Now())
}

func (g *Gateway) processReceivedMessage(msg webhookMessage, requestID string, receivedAt time.Time) {
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: requestID})
	log := g.log().With(config.F("request_id", requestID))
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

	normalizationStarted := time.Now()
	images, unsupported := g.loadImages(msg.Attachments, log)
	if len(msg.Attachments) > 0 {
		status := "ok"
		if len(unsupported) > 0 {
			status = "degraded"
		}
		log.Info("gateway.attachment.processed", "normalized imessage input attachments", config.F("accepted_count", len(images)), config.F("downgraded_count", len(unsupported)), config.F("declared_format_count", len(msg.Attachments)), config.F("duration_ms", time.Since(normalizationStarted).Milliseconds()), config.F("status", status))
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

	normalizedSenderID, err := accounts.NormalizeIdentifier("imessage", msg.Handle.Address)
	if err != nil {
		log.Error("gateway.account.normalize_failed", "failed to normalize imessage account", config.F("request_id", requestID), config.ErrorField(err))
		return
	}
	displayName := normalizedSenderID
	if resolvedName, err := g.lookupContactDisplayName(normalizedSenderID, log); err != nil {
		log.Debug("gateway.contact_lookup.failed", "imessage contact lookup failed", config.F("request_id", requestID), config.F("status", "degraded"), config.ErrorField(err))
	} else if resolvedName != "" {
		displayName = resolvedName
	}

	canonicalUserID, err := g.Links.EnsureAccount(ctx, "imessage", normalizedSenderID, displayName)
	if err != nil {
		log.Error("gateway.account.resolve_failed", "failed to resolve imessage account", config.F("request_id", requestID), config.ErrorField(err))
		return
	}

	sessionKey := g.sessionKey(chat, normalizedSenderID)
	log = log.With(config.F("user_id", canonicalUserID))
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
					reply.Images, reply.Unsupported = g.loadImagesLimit(replyCtx.Attachments, remainingImageSlots, log)
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
		ReceivedAt: receivedAt,
		RequestID:  requestID,
		ChatID:     chat.GUID,
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
	log := g.log().With(config.F("request_id", requestID))
	go func() {
		g.markRead(chatGUID, log)
		time.Sleep(typingAfterReadDelay)
		if err := g.startTyping(chatGUID, log); err != nil {
			log.Debug("gateway.typing.failed", "failed to start BlueBubbles typing indicator", config.F("status", "degraded"), config.ErrorField(err))
		}
	}()
}

// sessionKey returns the session identifier for a direct or group iMessage chat.
func (g *Gateway) sessionKey(chat messageChat, normalizedSenderID string) string {
	if chat.Style == chatStyleDirect {
		return "imessage:dm:" + normalizedSenderID
	}
	return "imessage:" + chat.GUID + ":" + normalizedSenderID
}

// primaryChat returns the first chat attached to the webhook payload.
func (m webhookMessage) primaryChat() messageChat {
	if len(m.Chats) == 0 {
		return messageChat{}
	}
	return m.Chats[0]
}

var mentionRE = regexp.MustCompile(`@?Oswald\b`)
