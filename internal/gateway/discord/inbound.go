package discord

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// resolveMentions replaces every <@ID> and <@!ID> token in text with @username.
func resolveMentions(text string, mentions []struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}) string {
	lookup := make(map[string]string, len(mentions))
	for _, m := range mentions {
		lookup[m.ID] = m.Username
	}

	re := regexp.MustCompile(`<@!?(\d+)>`)
	return re.ReplaceAllStringFunc(text, func(match string) string {
		sub := re.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		if username, ok := lookup[sub[1]]; ok {
			return "@" + username
		}
		return match
	})
}

// handleMessage processes an incoming Discord message.
func (dg *Gateway) handleMessage(msg MessageCreate) {
	log := dg.log()
	if msg.Author.Bot {
		return
	}
	requestID := config.NewRequestID()

	mention1 := fmt.Sprintf("<@%s>", dg.BotID)
	mention2 := fmt.Sprintf("<@!%s>", dg.BotID)
	mentionsBot := strings.Contains(msg.Content, mention1) || strings.Contains(msg.Content, mention2)
	isReplyToBot := msg.ReferencedMessage != nil && msg.ReferencedMessage.Author.ID == dg.BotID
	replyToID := ""
	if msg.GuildID != "" {
		replyToID = msg.ID
	}

	re := regexp.MustCompile(`<a?:([^:]+):\d+>`)
	text := strings.ReplaceAll(msg.Content, mention1, "")
	text = strings.ReplaceAll(text, mention2, "")
	text = strings.TrimSpace(text)
	text = re.ReplaceAllString(text, ":$1:")
	text = resolveMentions(text, msg.Mentions)
	preflight := routing.Preflight(routing.PreflightInput{
		IsGroup:      msg.GuildID != "",
		IsMention:    mentionsBot,
		IsReplyToBot: isReplyToBot,
		Text:         text,
	})
	isCommandAttempt := routing.IsCommandAttempt(text)
	if preflight.Action == routing.ActionIgnore {
		log.Debug("gateway.message.ignored", "ignored discord message",
			config.F("request_id", requestID),
			config.F("chat_id", msg.ChannelID),
			config.F("user_id", msg.Author.ID),
			config.F("is_group", msg.GuildID != ""),
			config.F("is_mention", mentionsBot),
			config.F("is_reply", msg.ReferencedMessage != nil),
			config.F("is_command", isCommandAttempt),
			config.F("reason", preflight.Reason),
			config.F("message_chars", len(msg.Content)),
		)
		return
	}

	images, unsupported := dg.loadImages(msg.Attachments)
	embedImageCount := 0
	if len(msg.Attachments) > 0 {
		log.Info("gateway.attachment.processed", "processed discord attachments", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("accepted_count", len(images)), config.F("downgraded_count", len(unsupported)), config.F("declared_format_count", len(msg.Attachments)))
	}
	if len(msg.Embeds) > 0 {
		embedImages, embedUnsupported := dg.loadEmbedImagesLimit(msg.Embeds, media.MaxImagesPerRequest-len(images))
		embedImageCount = len(embedImages)
		images = append(images, embedImages...)
		unsupported = append(unsupported, embedUnsupported...)
		log.Info("gateway.embed.processed", "processed discord embeds", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("accepted_count", len(embedImages)), config.F("downgraded_count", len(embedUnsupported)), config.F("declared_embed_count", len(msg.Embeds)))
	}
	if embedImageCount > 0 {
		text = stripEmbedURLsFromText(text, msg.Embeds)
	}

	// Compute the session key using the hybrid strategy:
	//   DMs (no GuildID):      SenderID           — continuous per-user memory
	//   Guild channels/threads: ChannelID:SenderID — per-user isolation, prevents cross-talk
	var sessionKey string
	if msg.GuildID == "" {
		sessionKey = "discord:dm:" + msg.Author.ID
	} else {
		sessionKey = "discord:" + msg.ChannelID + ":" + msg.Author.ID
	}
	normalizedAuthorID, normErr := accounts.NormalizeIdentifier("discord", msg.Author.ID)
	if normErr != nil {
		log.Error("gateway.account.normalize_failed", "failed to normalize discord account", config.F("request_id", requestID), config.ErrorField(normErr))
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I could not resolve your Discord account identity.", replyToID)
		return
	}

	canonicalUserID, err := dg.Links.EnsureAccount("discord", normalizedAuthorID, msg.Author.Username)
	if err != nil {
		log.Error("gateway.account.resolve_failed", "failed to resolve discord account", config.F("request_id", requestID), config.F("user_id", normalizedAuthorID), config.ErrorField(err))
		_, _ = dg.sendMessage(msg.ChannelID, "Sorry, I could not resolve your account identity.", replyToID)
		return
	}

	var reply *routing.ReplyContext
	if msg.ReferencedMessage != nil {
		reply = dg.resolveReplyContext(msg, re, images, requestID)
	}
	dg.rememberReply(msg.ID, replyContext{
		SessionKey:  sessionKey,
		ChannelID:   msg.ChannelID,
		SenderID:    msg.Author.ID,
		DisplayName: msg.Author.Username,
		Text:        text,
		Attachments: msg.Attachments,
		Embeds:      msg.Embeds,
		IsFromBot:   false,
		CreatedAt:   time.Now(),
	})

	responder := newRuntimeResponder(dg, requestID, msg.ChannelID, replyToID, sessionKey, msg.Author.ID)
	gatewayruntime.Execute(gatewayruntime.Request{
		RequestID: requestID,
		ChatID:    msg.ChannelID,
		Principal: identity.Principal{
			CanonicalUserID: canonicalUserID,
			Gateway:         "discord",
			ExternalID:      normalizedAuthorID,
			Assurance:       identity.AssuranceDiscordGateway,
		},
		DisplayName:  msg.Author.Username,
		SessionKey:   sessionKey,
		IsDirect:     msg.GuildID == "",
		IsGroup:      msg.GuildID != "",
		IsMention:    mentionsBot,
		IsReplyToBot: isReplyToBot,
		Text:         text,
		Images:       images,
		Unsupported:  unsupported,
		Reply:        reply,
		StreamFunc:   responder.Stream,
	}, dg.runtimeDependencies(), responder)
}
