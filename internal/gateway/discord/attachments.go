package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func (dg *Gateway) loadImages(attachments []Attachment, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	return dg.loadImagesLimit(attachments, media.MaxImagesPerRequest, scoped...)
}

func (dg *Gateway) loadImagesLimit(attachments []Attachment, maxImages int, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	log := dg.log(scoped...)
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

		image, err := dg.fetchAttachmentImage(attachment.ID, attachment.URL, attachment.ContentType, attachment.Filename, log)
		if err != nil {
			log.Debug("gateway.attachment.rejected", "rejected discord attachment", config.F("status", "degraded"))
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

func (dg *Gateway) loadEmbedImagesLimit(embeds []Embed, maxImages int, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	log := dg.log(scoped...)
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
			log.Warn("gateway.embed.video_fallback", "failed to extract animated discord embed; using static preview",
				config.F("status", "degraded"), config.ErrorField(err))
		}

		assetURL := discordEmbedImageURL(embed)
		if assetURL == "" {
			continue
		}
		image, err := dg.fetchAttachmentImage("", assetURL, "", label, log)
		if err != nil {
			log.Debug("gateway.embed.rejected", "rejected discord embed image", config.F("status", "degraded"), config.ErrorField(err))
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

func (dg *Gateway) fetchAttachmentImage(attachmentID, rawURL, declaredMIME, filename string, scoped ...*config.Logger) (llm.InputImage, error) {
	log := dg.log(scoped...)
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
		log.Debug("gateway.attachment.fetch_failed", "failed to fetch discord attachment", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
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
	log.Debug("gateway.attachment.normalized", "normalized discord attachment", config.F("normalized_mime", result.Image.MimeType), config.F("attachment_bytes", len(body)), config.F("original_width", result.OriginalWidth), config.F("original_height", result.OriginalHeight), config.F("width", result.Width), config.F("height", result.Height), config.F("is_resized", result.WasResized), config.F("normalized_bytes", result.NormalizedBytes), config.F("base64_chars", result.Base64Chars), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))
	return result.Image, nil
}

// sendCommandAttachment posts ordered in-memory command attachments to Discord.
func (dg *Gateway) sendCommandAttachment(channelID string, result commands.Result, replyToID string) (string, error) {
	return dg.sendAttachmentNonce(channelID, result, replyToID, "")
}

func (dg *Gateway) sendAttachmentNonce(channelID string, result commands.Result, replyToID, nonce string) (string, error) {
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
	if nonce != "" {
		payload["nonce"], payload["enforce_nonce"] = nonce, true
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
	req, err := http.NewRequestWithContext(dg.restContext(), http.MethodPost, endpoint, &body)
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
	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return "", discordRateLimitError{retryAfter: discordRetryAfter(resp, body)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", discordHTTPError(resp.StatusCode)
	}
	var created createMessageResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&created); err != nil {
		return "", err
	}
	if nonce != "" && created.ID == "" {
		return "", io.ErrUnexpectedEOF
	}
	return created.ID, nil
}
