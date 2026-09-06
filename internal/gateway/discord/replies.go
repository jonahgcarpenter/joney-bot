package discord

import (
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
)

const replyIndexTTL = time.Hour

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

// HandleRuntimeInvalidation purges reply context owned by the invalidated tenant.
func (dg *Gateway) HandleRuntimeInvalidation(event invalidation.Event) {
	sessions := make(map[string]bool, len(event.SessionIDs))
	for _, sessionID := range event.SessionIDs {
		sessions[sessionID] = true
	}
	senders := make(map[string]bool)
	const prefix = "discord:"
	for _, external := range event.ExternalIdentities {
		if len(external) > len(prefix) && external[:len(prefix)] == prefix {
			senders[external[len(prefix):]] = true
		}
	}
	dg.replyMu.Lock()
	for id, ctx := range dg.replyIndex {
		if sessions[ctx.SessionKey] || senders[ctx.SenderID] {
			delete(dg.replyIndex, id)
		}
	}
	dg.replyMu.Unlock()
}
