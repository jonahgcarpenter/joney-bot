package imessage

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func (g *Gateway) loadImages(attachments []attachment, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	return g.loadImagesLimit(attachments, media.MaxImagesPerRequest, scoped...)
}

func (g *Gateway) loadImagesLimit(attachments []attachment, maxImages int, scoped ...*config.Logger) ([]llm.InputImage, []string) {
	log := g.log(scoped...)
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

		image, err := g.fetchAttachmentImage(attachment, log)
		if err != nil {
			log.Debug("gateway.attachment.rejected", "rejected imessage attachment", config.F("status", "degraded"))
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

func (g *Gateway) fetchAttachmentImage(attachment attachment, scoped ...*config.Logger) (llm.InputImage, error) {
	log := g.log(scoped...)
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
		log.Debug("gateway.attachment.fetch_failed", "failed to fetch imessage attachment", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
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
	log.Debug("gateway.attachment.normalized", "normalized imessage attachment", config.F("normalized_mime", result.Image.MimeType), config.F("attachment_bytes", len(body)), config.F("original_width", result.OriginalWidth), config.F("original_height", result.OriginalHeight), config.F("width", result.Width), config.F("height", result.Height), config.F("is_resized", result.WasResized), config.F("normalized_bytes", result.NormalizedBytes), config.F("base64_chars", result.Base64Chars), config.F("preserved_alpha", result.PreservedAlpha), config.F("used_declared_mime", result.UsedDeclaredMIME))

	image := result.Image
	return image, nil
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
