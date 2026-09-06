package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	gorilla "github.com/gorilla/websocket"

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

const replyIndexTTL = time.Hour

var (
	errDiscordReconnectRequested = errors.New("discord requested reconnect")
	errDiscordInvalidSession     = errors.New("discord invalid session")
	errDiscordMissedHeartbeatAck = errors.New("discord missed heartbeat ack")
)

// Name returns the human-readable gateway name.
func (dg *Gateway) Name() string {
	return "Discord"
}

// Start initializes the resilient connection loop.
// It blocks forever, automatically reconnecting if the websocket drops.
func (dg *Gateway) Start(b *broker.Broker) error {
	dg.Broker = b
	log := dg.log()
	if dg.replyIndex == nil {
		dg.replyIndex = make(map[string]replyContext)
	}
	dg.setHeartbeatAcked(true)

	for {
		err := dg.connectAndListen()

		if err != nil {
			switch {
			case errors.Is(err, errDiscordReconnectRequested):
				log.Info("gateway.session.resume_requested", "discord requested session resume")
			case errors.Is(err, errDiscordInvalidSession):
				log.Warn("gateway.session.invalid", "discord session invalid, reidentifying")
			case errors.Is(err, errDiscordMissedHeartbeatAck):
				log.Warn("gateway.heartbeat.missed_ack", "discord heartbeat ack missing, reconnecting")
			default:
				log.Warn("gateway.connection.dropped", "discord connection dropped", config.ErrorField(err))
			}
		} else {
			log.Debug("gateway.connection.closed", "discord connection closed normally")
		}

		log.Debug("gateway.reconnect.scheduled", "scheduled discord reconnect", config.F("delay_ms", 5000))
		time.Sleep(5 * time.Second)
	}
}

func (dg *Gateway) rememberReply(messageID string, ctx replyContext) {
	if messageID == "" {
		return
	}

	dg.replyMu.Lock()
	dg.pruneReplyIndexLocked()
	dg.replyIndex[messageID] = ctx
	dg.replyMu.Unlock()
}

func (dg *Gateway) lookupReply(messageID string) (replyContext, bool) {
	if messageID == "" {
		return replyContext{}, false
	}

	dg.replyMu.Lock()
	dg.pruneReplyIndexLocked()
	ctx, ok := dg.replyIndex[messageID]
	dg.replyMu.Unlock()

	return ctx, ok
}

func (dg *Gateway) pruneReplyIndexLocked() int {
	cutoff := time.Now().Add(-replyIndexTTL)
	pruned := 0
	for id, ctx := range dg.replyIndex {
		if ctx.CreatedAt.Before(cutoff) {
			delete(dg.replyIndex, id)
			pruned++
		}
	}
	return pruned
}

// connectAndListen manages a single Discord gateway session.
func (dg *Gateway) connectAndListen() error {
	resumeURL, shouldResume := dg.resumeGatewayURL()
	conn, _, err := gorilla.DefaultDialer.Dial(resolveGatewayURL(resumeURL), nil)
	if err != nil {
		return fmt.Errorf("Failed to dial Discord Gateway: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	defer conn.Close()

	var helloPayload Payload
	if err := conn.ReadJSON(&helloPayload); err != nil || helloPayload.Op != 10 {
		return fmt.Errorf("Expected HELLO payload: %v", err)
	}

	var hello HelloEvent
	json.Unmarshal(helloPayload.D, &hello) // nolint: errcheck

	dg.setHeartbeatAcked(true)
	hbErrCh := make(chan error, 1)
	go dg.heartbeatLoop(conn, hello.HeartbeatInterval*time.Millisecond, done, hbErrCh)

	if shouldResume {
		if err := dg.resume(conn); err != nil {
			return fmt.Errorf("Failed to resume: %w", err)
		}
		dg.log().Debug("gateway.session.resume_attempt", "attempted discord session resume")
	} else {
		if err := dg.identify(conn); err != nil {
			return fmt.Errorf("Failed to identify: %w", err)
		}
	}

	err = dg.listenLoop(conn)
	select {
	case hbErr := <-hbErrCh:
		if hbErr != nil {
			return hbErr
		}
	default:
	}

	return err
}

// identify authenticates the bot with its token and intents.
func (dg *Gateway) identify(conn *gorilla.Conn) error {
	identifyData := map[string]interface{}{
		"token":   dg.Token,
		"intents": intents,
		"properties": map[string]string{
			"os":      "linux",
			"browser": "oswald-ai",
			"device":  "oswald-ai",
		},
	}

	identifyPayload := Payload{
		Op: 2,
		D:  marshalJSON(identifyData),
	}

	if err := conn.WriteJSON(identifyPayload); err != nil {
		return fmt.Errorf("Failed to send IDENTIFY: %w", err)
	}
	return nil
}

// resume attempts to resume a prior Discord gateway session.
func (dg *Gateway) resume(conn *gorilla.Conn) error {
	sessionID, seq, ok := dg.resumeState()
	if !ok {
		return errors.New("resume state unavailable")
	}

	resumeData := map[string]interface{}{
		"token":      dg.Token,
		"session_id": sessionID,
		"seq":        seq,
	}

	resumePayload := Payload{
		Op: 6,
		D:  marshalJSON(resumeData),
	}

	if err := conn.WriteJSON(resumePayload); err != nil {
		return fmt.Errorf("Failed to send RESUME: %w", err)
	}

	return nil
}

// heartbeatLoop sends heartbeat packets to Discord at the specified interval.
func (dg *Gateway) heartbeatLoop(conn *gorilla.Conn, interval time.Duration, done <-chan struct{}, errCh chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			select {
			case errCh <- nil:
			default:
			}
			return
		case <-ticker.C:
		}

		if !dg.heartbeatAcked() {
			select {
			case errCh <- errDiscordMissedHeartbeatAck:
			default:
			}
			_ = conn.Close()
			return
		}

		dg.setHeartbeatAcked(false)

		hb := Payload{Op: 1, D: dg.heartbeatPayload()}
		if err := conn.WriteJSON(hb); err != nil {
			select {
			case <-done:
				return
			default:
			}

			if isClosedConnError(err) {
				return
			}

			select {
			case errCh <- fmt.Errorf("discord heartbeat failed: %w", err):
			default:
			}
			_ = conn.Close()
			return
		}
	}
}

// listenLoop reads events from the Discord gateway and dispatches them.
func (dg *Gateway) listenLoop(conn *gorilla.Conn) error {
	for {
		var p Payload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("Discord read error: %w", err)
		}

		if p.S != nil {
			dg.setLastSequence(*p.S)
		}

		switch p.Op {
		case 0:
			if p.T != nil {
				switch *p.T {
				case "READY":
					var ready ReadyEvent
					if err := json.Unmarshal(p.D, &ready); err == nil {
						dg.BotID = ready.User.ID
						dg.setReadySession(ready.SessionID, ready.ResumeGatewayURL)
						dg.setHeartbeatAcked(true)
						dg.log().Info("gateway.session.ready", "discord gateway ready", config.F("bot_id", dg.BotID), config.F("bot_username", ready.User.Username))
					}
				case "RESUMED":
					dg.setHeartbeatAcked(true)
					dg.log().Debug("gateway.session.resumed", "discord session resumed")
				case "MESSAGE_CREATE":
					var msg MessageCreate
					if err := json.Unmarshal(p.D, &msg); err == nil {
						go dg.handleMessage(msg)
					}
				}
			}
		case 7:
			return errDiscordReconnectRequested
		case 9:
			dg.clearResumeState()
			return errDiscordInvalidSession
		case 11:
			dg.setHeartbeatAcked(true)
		}
	}
}

func (dg *Gateway) setReadySession(sessionID, resumeURL string) {
	dg.sessionMu.Lock()
	dg.sessionID = sessionID
	dg.resumeURL = resumeURL
	dg.sessionMu.Unlock()
}

func (dg *Gateway) clearResumeState() {
	dg.sessionMu.Lock()
	dg.sessionID = ""
	dg.resumeURL = ""
	dg.lastSeq = nil
	dg.hbAcked = true
	dg.sessionMu.Unlock()
}

func (dg *Gateway) setLastSequence(seq int) {
	dg.sessionMu.Lock()
	seqCopy := seq
	dg.lastSeq = &seqCopy
	dg.sessionMu.Unlock()
}

func (dg *Gateway) heartbeatPayload() json.RawMessage {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	if dg.lastSeq == nil {
		return json.RawMessage("null")
	}
	return marshalJSON(*dg.lastSeq)
}

func (dg *Gateway) setHeartbeatAcked(acked bool) {
	dg.sessionMu.Lock()
	dg.hbAcked = acked
	dg.sessionMu.Unlock()
}

func (dg *Gateway) heartbeatAcked() bool {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	return dg.hbAcked
}

func (dg *Gateway) resumeState() (string, int, bool) {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	if dg.sessionID == "" || dg.lastSeq == nil {
		return "", 0, false
	}
	return dg.sessionID, *dg.lastSeq, true
}

func (dg *Gateway) resumeGatewayURL() (string, bool) {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	return dg.resumeURL, dg.sessionID != "" && dg.lastSeq != nil
}

func resolveGatewayURL(resumeURL string) string {
	if resumeURL == "" {
		return gatewayURL
	}
	if strings.HasPrefix(resumeURL, "ws://") || strings.HasPrefix(resumeURL, "wss://") {
		return resumeURL
	}
	return fmt.Sprintf("wss://%s/?v=10&encoding=json", resumeURL)
}

func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "closed network connection")
}

// splitMessage breaks a large string into chunks respecting Discord's 2000-char limit.
func splitMessage(text string, limit int) []string {
	var chunks []string
	runes := []rune(text)

	for len(runes) > 0 {
		prefixLen := discordPrefixRuneCount(runes, limit)
		if prefixLen == len(runes) {
			chunks = append(chunks, string(runes))
			break
		}
		if prefixLen == 0 {
			prefixLen = 1
		}

		chunkRunes := runes[:prefixLen]
		splitIdx := -1

		for i := len(chunkRunes) - 1; i > 0; i-- {
			if chunkRunes[i] == '\n' {
				splitIdx = i
				break
			}
		}

		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i > 0; i-- {
				if (chunkRunes[i-1] == '.' || chunkRunes[i-1] == '!' || chunkRunes[i-1] == '?') && chunkRunes[i] == ' ' {
					splitIdx = i
					break
				}
			}
		}

		if splitIdx == -1 {
			for i := len(chunkRunes) - 1; i > 0; i-- {
				if chunkRunes[i] == ' ' {
					splitIdx = i
					break
				}
			}
		}

		if splitIdx == -1 {
			splitIdx = prefixLen
		}

		chunks = append(chunks, strings.TrimSpace(string(runes[:splitIdx])))
		runes = runes[splitIdx:]
		runes = []rune(strings.TrimLeft(string(runes), " \n\r"))
	}

	return chunks
}

func discordContentLength(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func discordPrefixRuneCount(runes []rune, limit int) int {
	units := 0
	for i, r := range runes {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			return i
		}
		units += width
	}
	return len(runes)
}

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
	normalizedAuthorID, normErr := accountlinking.NormalizeIdentifier("discord", msg.Author.ID)
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

func (dg *Gateway) runtimeDependencies() gatewayruntime.Dependencies {
	deps := dg.Runtime
	deps.Broker = dg.Broker
	if deps.Access == nil {
		deps.Access = dg.Links
	}
	if deps.Log == nil {
		deps.Log = dg.Log
	}
	return deps
}

func (dg *Gateway) resolveReplyContext(msg MessageCreate, emojiRE *regexp.Regexp, currentImages []llm.InputImage, requestID string) *routing.ReplyContext {
	log := dg.log()
	referenced := msg.ReferencedMessage
	if referenced == nil {
		return nil
	}

	if cached, ok := dg.lookupReply(referenced.ID); ok {
		reply := &routing.ReplyContext{
			SenderName: strings.TrimSpace(cached.DisplayName),
			Text:       strings.TrimSpace(cached.Text),
			IsFromBot:  cached.IsFromBot,
		}
		if reply.SenderName == "" && cached.IsFromBot {
			reply.SenderName = "Oswald"
		}
		if len(cached.Attachments) > 0 {
			remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
			if remainingImageSlots > 0 {
				reply.Images, reply.Unsupported = dg.loadImagesLimit(cached.Attachments, remainingImageSlots)
			} else {
				reply.Unsupported = discordAttachmentLabels(cached.Attachments)
			}
		}
		if len(cached.Embeds) > 0 {
			remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
			if remainingImageSlots > 0 {
				embedImages, embedUnsupported := dg.loadEmbedImagesLimit(cached.Embeds, remainingImageSlots)
				reply.Images = append(reply.Images, embedImages...)
				reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
				if len(embedImages) > 0 {
					reply.Text = stripEmbedURLsFromText(reply.Text, cached.Embeds)
				}
			} else {
				reply.Unsupported = append(reply.Unsupported, discordEmbedLabels(cached.Embeds)...)
			}
		}
		log.Debug("gateway.reply_context.applied", "applied discord cached reply context", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("is_bot_reply", cached.IsFromBot), config.F("reply_image_count", len(reply.Images)))
		return reply
	}

	replyName := strings.TrimSpace(referenced.Author.Username)
	if replyName == "" && referenced.Author.ID == dg.BotID {
		replyName = "Oswald"
	}
	quotedContent := strings.TrimSpace(emojiRE.ReplaceAllString(referenced.Content, ":$1:"))
	reply := &routing.ReplyContext{
		SenderName: replyName,
		Text:       quotedContent,
		IsFromBot:  referenced.Author.ID == dg.BotID,
	}
	if len(referenced.Attachments) > 0 {
		remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
		if remainingImageSlots > 0 {
			reply.Images, reply.Unsupported = dg.loadImagesLimit(referenced.Attachments, remainingImageSlots)
		} else {
			reply.Unsupported = discordAttachmentLabels(referenced.Attachments)
		}
	}
	if len(referenced.Embeds) > 0 {
		remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
		if remainingImageSlots > 0 {
			embedImages, embedUnsupported := dg.loadEmbedImagesLimit(referenced.Embeds, remainingImageSlots)
			reply.Images = append(reply.Images, embedImages...)
			reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
			if len(embedImages) > 0 {
				reply.Text = stripEmbedURLsFromText(reply.Text, referenced.Embeds)
			}
		} else {
			reply.Unsupported = append(reply.Unsupported, discordEmbedLabels(referenced.Embeds)...)
		}
	}
	if quotedContent == "" && len(reply.Images) == 0 && len(reply.Unsupported) == 0 {
		if fetched, ok := dg.fetchMessage(msg.ChannelID, referenced.ID, requestID); ok {
			reply.SenderName = strings.TrimSpace(fetched.Author.Username)
			if reply.SenderName == "" && fetched.Author.ID == dg.BotID {
				reply.SenderName = "Oswald"
			}
			reply.Text = strings.TrimSpace(emojiRE.ReplaceAllString(fetched.Content, ":$1:"))
			reply.IsFromBot = fetched.Author.ID == dg.BotID
			if len(fetched.Attachments) > 0 {
				remainingImageSlots := media.MaxImagesPerRequest - len(currentImages)
				if remainingImageSlots > 0 {
					reply.Images, reply.Unsupported = dg.loadImagesLimit(fetched.Attachments, remainingImageSlots)
				} else {
					reply.Unsupported = discordAttachmentLabels(fetched.Attachments)
				}
			}
			if len(fetched.Embeds) > 0 {
				remainingImageSlots := media.MaxImagesPerRequest - len(currentImages) - len(reply.Images)
				if remainingImageSlots > 0 {
					embedImages, embedUnsupported := dg.loadEmbedImagesLimit(fetched.Embeds, remainingImageSlots)
					reply.Images = append(reply.Images, embedImages...)
					reply.Unsupported = append(reply.Unsupported, embedUnsupported...)
					if len(embedImages) > 0 {
						reply.Text = stripEmbedURLsFromText(reply.Text, fetched.Embeds)
					}
				} else {
					reply.Unsupported = append(reply.Unsupported, discordEmbedLabels(fetched.Embeds)...)
				}
			}
		}
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.Images) == 0 && len(reply.Unsupported) == 0 {
		reply.IsUnavailable = true
	}
	status := "ok"
	if reply.IsUnavailable {
		status = "degraded"
	}
	log.Debug("gateway.reply_context.applied", "applied discord reply context", config.F("request_id", requestID), config.F("chat_id", msg.ChannelID), config.F("is_bot_reply", reply.IsFromBot), config.F("reply_image_count", len(reply.Images)), config.F("status", status))
	return reply
}

func (dg *Gateway) fetchMessage(channelID, messageID, requestID string) (messageResponse, bool) {
	log := dg.log()
	if strings.TrimSpace(channelID) == "" || strings.TrimSpace(messageID) == "" {
		return messageResponse{}, false
	}

	url := fmt.Sprintf("%s/channels/%s/messages/%s", dg.apiBaseURL(), channelID, messageID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to build discord reply lookup request", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	req.Header.Set("Authorization", "Bot "+dg.Token)

	resp, err := dg.httpClient(10 * time.Second).Do(req)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to fetch discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to read discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Debug("gateway.reply_lookup.failed", "discord reply lookup failed", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(respBody)), config.F("status", "degraded"))
		return messageResponse{}, false
	}

	var result messageResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to decode discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	log.Debug("gateway.reply_lookup.fetched", "fetched discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("attachment_count", len(result.Attachments)), config.F("status", "ok"))
	return result, true
}

func (dg *Gateway) loadImages(attachments []Attachment) ([]llm.InputImage, []string) {
	return dg.loadImagesLimit(attachments, media.MaxImagesPerRequest)
}

func (dg *Gateway) loadImagesLimit(attachments []Attachment, maxImages int) ([]llm.InputImage, []string) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if maxImages <= 0 {
		return nil, discordAttachmentLabels(attachments)
	}

	images := make([]llm.InputImage, 0, len(attachments))
	unsupported := make([]string, 0)
	for _, attachment := range attachments {
		label := media.AttachmentLabel(attachment.Filename, attachment.ContentType)
		if len(images) >= maxImages {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.ContentType != "" && !media.LooksLikeImageMIME(attachment.ContentType) {
			unsupported = append(unsupported, label)
			continue
		}
		if attachment.Size > media.MaxImageBytes {
			unsupported = append(unsupported, label)
			continue
		}

		image, err := dg.fetchAttachmentImage(attachment.ID, attachment.URL, attachment.ContentType, attachment.Filename)
		if err != nil {
			dg.log().Debug("gateway.attachment.rejected", "rejected discord attachment", config.F("attachment_id", attachment.ID), config.F("declared_mime", strings.TrimSpace(attachment.ContentType)), config.F("status", "degraded"))
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

func discordAttachmentLabels(attachments []Attachment) []string {
	labels := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		labels = append(labels, media.AttachmentLabel(attachment.Filename, attachment.ContentType))
	}
	return labels
}

func (dg *Gateway) loadEmbedImagesLimit(embeds []Embed, maxImages int) ([]llm.InputImage, []string) {
	if len(embeds) == 0 {
		return nil, nil
	}
	if maxImages <= 0 {
		return nil, discordEmbedLabels(embeds)
	}

	images := make([]llm.InputImage, 0, len(embeds))
	unsupported := make([]string, 0)
	for _, embed := range embeds {
		if !discordEmbedIsIntentionalMedia(embed) {
			continue
		}
		label := discordEmbedLabel(embed)
		if len(images) >= maxImages {
			unsupported = append(unsupported, label)
			continue
		}

		if videoURL := discordEmbedVideoURL(embed); videoURL != "" {
			image, err := dg.fetchEmbedVideo(videoURL, label)
			if err == nil && image.Data != "" {
				images = append(images, image)
				continue
			}
			if err == nil {
				err = fmt.Errorf("video extractor returned an empty image")
			}
			dg.log().Warn("gateway.embed.video_fallback", "failed to extract animated discord embed; using static preview",
				config.F("embed_type", strings.TrimSpace(embed.Type)), config.F("status", "degraded"), config.ErrorField(err))
		}

		assetURL := discordEmbedImageURL(embed)
		if assetURL == "" {
			continue
		}
		image, err := dg.fetchAttachmentImage("", assetURL, "", label)
		if err != nil {
			dg.log().Debug("gateway.embed.rejected", "rejected discord embed image", config.F("embed_type", strings.TrimSpace(embed.Type)), config.F("status", "degraded"), config.ErrorField(err))
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

func discordEmbedLabels(embeds []Embed) []string {
	labels := make([]string, 0, len(embeds))
	for _, embed := range embeds {
		if !discordEmbedIsIntentionalMedia(embed) {
			continue
		}
		if discordEmbedVideoURL(embed) != "" || discordEmbedImageURL(embed) != "" {
			labels = append(labels, discordEmbedLabel(embed))
		}
	}
	return labels
}

func discordEmbedIsIntentionalMedia(embed Embed) bool {
	switch strings.ToLower(strings.TrimSpace(embed.Type)) {
	case "image", "gifv":
		return true
	default:
		return false
	}
}

func discordEmbedLabel(embed Embed) string {
	embedType := strings.TrimSpace(embed.Type)
	if embedType == "" {
		embedType = "link"
	}
	return media.AttachmentLabel("discord embed", embedType)
}

func discordEmbedImageURL(embed Embed) string {
	if url := strings.TrimSpace(embed.Image.ProxyURL); url != "" {
		return url
	}
	if url := strings.TrimSpace(embed.Image.URL); url != "" {
		return url
	}
	if url := strings.TrimSpace(embed.Thumbnail.ProxyURL); url != "" {
		return url
	}
	return strings.TrimSpace(embed.Thumbnail.URL)
}

func discordEmbedVideoURL(embed Embed) string {
	if !strings.EqualFold(strings.TrimSpace(embed.Type), "gifv") {
		return ""
	}
	if url := strings.TrimSpace(embed.Video.ProxyURL); url != "" {
		return url
	}
	return strings.TrimSpace(embed.Video.URL)
}

func stripEmbedURLsFromText(text string, embeds []Embed) string {
	for _, rawURL := range discordEmbedSourceURLs(embeds) {
		text = strings.ReplaceAll(text, rawURL, "")
	}
	return strings.Join(strings.Fields(text), " ")
}

func discordEmbedSourceURLs(embeds []Embed) []string {
	urls := make([]string, 0, len(embeds)*7)
	seen := make(map[string]struct{}, len(embeds)*7)
	for _, embed := range embeds {
		if !discordEmbedIsIntentionalMedia(embed) {
			continue
		}
		for _, rawURL := range []string{
			embed.URL,
			embed.Image.URL,
			embed.Image.ProxyURL,
			embed.Thumbnail.URL,
			embed.Thumbnail.ProxyURL,
			embed.Video.URL,
			embed.Video.ProxyURL,
		} {
			rawURL = strings.TrimSpace(rawURL)
			if rawURL == "" {
				continue
			}
			if _, ok := seen[rawURL]; ok {
				continue
			}
			seen[rawURL] = struct{}{}
			urls = append(urls, rawURL)
		}
	}
	return urls
}

func (dg *Gateway) fetchEmbedVideo(rawURL, label string) (llm.InputImage, error) {
	resp, err := dg.httpClient(15 * time.Second).Get(rawURL)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("download animated embed %q: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return llm.InputImage{}, fmt.Errorf("download animated embed %q: unexpected status %d", label, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("read animated embed %q: %w", label, err)
	}
	if len(body) > media.MaxImageBytes {
		return llm.InputImage{}, fmt.Errorf("animated embed %q exceeds %d bytes", label, media.MaxImageBytes)
	}
	extractor := dg.VideoFrames
	if extractor == nil {
		extractor = media.FFmpegVideoFrameExtractor{}
	}
	image, err := extractor.Extract(context.Background(), body, label)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("extract animated embed %q: %w", label, err)
	}
	return image, nil
}

func (dg *Gateway) fetchAttachmentImage(attachmentID, rawURL, declaredMIME, filename string) (llm.InputImage, error) {
	if strings.TrimSpace(rawURL) == "" {
		return llm.InputImage{}, nil
	}

	resp, err := dg.httpClient(15 * time.Second).Get(rawURL)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("download attachment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		dg.log().Warn("gateway.attachment.fetch_failed", "failed to fetch discord attachment", config.F("attachment_id", attachmentID), config.F("declared_mime", strings.TrimSpace(declaredMIME)), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return llm.InputImage{}, fmt.Errorf("download attachment: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("read attachment: %w", err)
	}
	if len(body) > media.MaxImageBytes {
		return llm.InputImage{}, fmt.Errorf("attachment exceeds %d bytes", media.MaxImageBytes)
	}

	result, err := media.NormalizeInputImageFromBytes(resp.Header, declaredMIME, body, filename)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("attachment rejected: %w", err)
	}
	dg.log().Debug("gateway.attachment.normalized", "normalized discord attachment", config.F("attachment_id", attachmentID), config.F("declared_mime", strings.TrimSpace(declaredMIME)), config.F("detected_mime", result.DetectedMIME), config.F("normalized_mime", result.Image.MimeType), config.F("attachment_bytes", len(body)), config.F("original_width", result.OriginalWidth), config.F("original_height", result.OriginalHeight), config.F("width", result.Width), config.F("height", result.Height), config.F("is_resized", result.WasResized), config.F("normalized_bytes", result.NormalizedBytes), config.F("base64_chars", result.Base64Chars), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))
	return result.Image, nil
}

// sendTyping posts a typing indicator to Discord.
func (dg *Gateway) sendTyping(channelID string) error {
	url := fmt.Sprintf("%s/channels/%s/typing", dg.apiBaseURL(), channelID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+dg.Token)

	resp, err := dg.httpClient(5 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		dg.log().Debug("gateway.typing.failed", "discord typing request failed", config.F("chat_id", channelID), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

// sendMessage posts a message to a Discord channel and returns the created
// Discord message ID when available.
func (dg *Gateway) sendMessage(channelID, content, replyToID string) (string, error) {
	url := fmt.Sprintf("%s/channels/%s/messages", dg.apiBaseURL(), channelID)

	payload := map[string]interface{}{
		"content": content,
	}

	if replyToID != "" {
		payload["message_reference"] = map[string]string{
			"message_id": replyToID,
		}
	}

	created, err := dg.doMessageJSON(http.MethodPost, url, payload)
	if err != nil {
		dg.log().Warn("gateway.send.failed", "discord send request failed", config.F("chat_id", channelID), config.F("status", "error"), config.ErrorField(err))
		return "", err
	}
	return created.ID, nil
}

// editMessage replaces the content of a message previously sent by Oswald.
func (dg *Gateway) editMessage(channelID, messageID, content string) error {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", dg.apiBaseURL(), channelID, messageID)
	_, err := dg.doMessageJSON(http.MethodPatch, url, map[string]string{"content": content})
	return err
}

func (dg *Gateway) deleteMessage(channelID, messageID string) error {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", dg.apiBaseURL(), channelID, messageID)
	_, err := dg.doMessageJSON(http.MethodDelete, url, nil)
	return err
}

func (dg *Gateway) doMessageJSON(method, url string, payload interface{}) (createMessageResponse, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return createMessageResponse{}, err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		var requestBody io.Reader
		if body != nil {
			requestBody = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, url, requestBody)
		if err != nil {
			return createMessageResponse{}, err
		}
		req.Header.Set("Authorization", "Bot "+dg.Token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := dg.httpClient(10 * time.Second).Do(req)
		if err != nil {
			return createMessageResponse{}, err
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		if readErr != nil {
			return createMessageResponse{}, readErr
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			retryAfter := discordRetryAfter(resp, respBody)
			if retryAfter > 0 && retryAfter <= 5*time.Second {
				time.Sleep(retryAfter)
				continue
			}
		}
		if resp.StatusCode >= 500 && attempt == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return createMessageResponse{}, fmt.Errorf("discord %s request returned status %d", method, resp.StatusCode)
		}

		var result createMessageResponse
		if len(respBody) > 0 {
			if err := json.Unmarshal(respBody, &result); err != nil {
				return createMessageResponse{}, err
			}
		}
		return result, nil
	}
	return createMessageResponse{}, fmt.Errorf("discord %s request exhausted retries", method)
}

func discordRetryAfter(resp *http.Response, body []byte) time.Duration {
	if value := strings.TrimSpace(resp.Header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.RetryAfter > 0 {
		return time.Duration(payload.RetryAfter * float64(time.Second))
	}
	return 0
}

// sendCommandAttachment posts ordered in-memory command attachments to Discord.
func (dg *Gateway) sendCommandAttachment(channelID string, result commands.Result, replyToID string) (string, error) {
	if err := result.ValidateAttachments(); err != nil {
		return "", err
	}
	attachments := result.Attachments
	if len(attachments) == 0 {
		return dg.sendMessage(channelID, result.Text, replyToID)
	}

	metadata := make([]map[string]any, 0, len(attachments))
	for i, attachment := range attachments {
		metadata = append(metadata, map[string]any{"id": i, "filename": attachment.Filename})
	}
	payload := map[string]any{
		"content":     result.Text,
		"attachments": metadata,
	}
	if replyToID != "" {
		payload["message_reference"] = map[string]string{"message_id": replyToID}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal Discord attachment payload: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("payload_json", string(payloadJSON)); err != nil {
		return "", fmt.Errorf("write Discord attachment payload: %w", err)
	}
	for i, attachment := range attachments {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": fmt.Sprintf("files[%d]", i), "filename": attachment.Filename}))
		header.Set("Content-Type", attachment.MIMEType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return "", fmt.Errorf("create Discord attachment part %d: %w", i, err)
		}
		if _, err := part.Write(attachment.Data); err != nil {
			return "", fmt.Errorf("write Discord attachment %d: %w", i, err)
		}
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close Discord attachment payload: %w", err)
	}

	endpoint := fmt.Sprintf("%s/channels/%s/messages", dg.apiBaseURL(), channelID)
	req, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bot "+dg.Token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := dg.httpClient(15 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Discord attachment send failed with status %d", resp.StatusCode)
	}
	var created createMessageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// marshalJSON converts any value to a json.RawMessage, ignoring marshal errors.
func marshalJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
