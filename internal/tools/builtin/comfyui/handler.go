package comfyui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

type generator interface {
	Generate(context.Context, map[string]node, string, []byte) (GeneratedImage, bool, error)
}

type imageSource func(context.Context) []requestctx.InputImage

// NewHandler builds a ComfyUI tool handler using current request images from requestctx.
func NewHandler(mode Mode, workflow *Workflow, client *Client, log *config.Logger) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return newHandler(mode, workflow, client, log, requestctx.InputImagesFromContext)
}

func newHandler(mode Mode, workflow *Workflow, client generator, log *config.Logger, images imageSource) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("ComfyUI generation requires an authenticated request")
		}
		prompt, _ := args["prompt"].(string)
		negative, _ := args["negative_prompt"].(string)
		prompt = strings.TrimSpace(prompt)
		negative = strings.TrimSpace(negative)
		if prompt == "" {
			return governance.Result{}, errors.New("prompt is required")
		}
		if utf8.RuneCountInString(prompt) > maxPromptRunes || utf8.RuneCountInString(negative) > maxPromptRunes {
			return governance.Result{}, fmt.Errorf("prompt text must not exceed %d characters", maxPromptRunes)
		}

		var inputPNG []byte
		if mode == ImageToImage {
			requestImages := images(ctx)
			if len(requestImages) == 0 {
				return governance.Result{}, errors.New("image-to-image generation requires an image on the current request")
			}
			var err error
			inputPNG, err = reencodePNG(requestImages[0])
			if err != nil {
				return governance.Result{}, err
			}
		}
		built, seed, err := workflow.Build(prompt, negative)
		if err != nil {
			return governance.Result{}, err
		}
		meta := requestctx.MetadataFromContext(ctx)
		agentLog := log.Agent("agent.tool.comfyui", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		agentLog.Debug("agent.tool.comfyui.start", "starting ComfyUI generation", config.F("mode", string(mode)), config.F("prompt_chars", utf8.RuneCountInString(prompt)), config.F("negative_prompt_chars", utf8.RuneCountInString(negative)))

		outputNode := "9"
		if mode == ImageToImage {
			outputNode = "30"
		}
		generated, cleanupFailed, err := client.Generate(context.WithValue(ctx, generationLoggerKey{}, log), built, outputNode, inputPNG)
		if err != nil {
			if cleanupFailed {
				agentLog.Warn("agent.tool.comfyui.cleanup_failed", "ComfyUI VRAM cleanup failed", config.F("mode", string(mode)), config.F("reason_code", "vram_cleanup_failed"), config.F("status", "degraded"))
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return governance.Result{}, fmt.Errorf("ComfyUI generation canceled: %w", ctxErr)
			}
			return governance.Result{}, errors.New("ComfyUI generation failed")
		}
		filename, err := outputFilename(mode, seed, generated.MIMEType)
		if err != nil {
			return governance.Result{}, err
		}
		metadata, err := json.Marshal(toolMetadata{Status: "generated", Mode: mode, MIMEType: generated.MIMEType, Width: generated.Size.X, Height: generated.Size.Y, Seed: seed, AttachmentCount: 1})
		if err != nil {
			return governance.Result{}, errors.New("encode ComfyUI result metadata")
		}
		result := governance.Result{
			Content: string(metadata), Outcome: governance.OutcomeProductive,
			Attachments: []media.OutputAttachment{{Filename: filename, MIMEType: generated.MIMEType, Data: generated.Data}},
			IsDegraded:  cleanupFailed,
		}
		if cleanupFailed {
			result.ReasonCode = "vram_cleanup_failed"
			agentLog.Warn("agent.tool.comfyui.cleanup_failed", "ComfyUI VRAM cleanup failed after image generation", config.F("mode", string(mode)), config.F("reason_code", result.ReasonCode), config.F("status", "degraded"))
		}
		agentLog.Debug("agent.tool.comfyui.complete", "completed ComfyUI generation", config.F("mode", string(mode)), config.F("image_bytes", len(generated.Data)), config.F("width", generated.Size.X), config.F("height", generated.Size.Y), config.F("is_degraded", cleanupFailed), config.F("status", map[bool]string{true: "degraded", false: "ok"}[cleanupFailed]))
		return result, nil
	}
}

func outputFilename(mode Mode, seed uint32, mimeType string) (string, error) {
	extension := ""
	switch mimeType {
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpg"
	case "image/gif":
		extension = ".gif"
	case "image/webp":
		extension = ".webp"
	default:
		return "", errors.New("ComfyUI returned an unsupported image type")
	}
	return fmt.Sprintf("oswald-%s-%d%s", strings.ReplaceAll(string(mode), "_", "-"), seed, extension), nil
}

func reencodePNG(input requestctx.InputImage) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(input.Data))
	if err != nil {
		return nil, errors.New("current request image is not valid base64")
	}
	if len(data) == 0 || len(data) > media.MaxImageBytes {
		return nil, errors.New("current request image exceeds the input size limit")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("current request image could not be decoded")
	}
	var output bytes.Buffer
	if err := png.Encode(&output, decoded); err != nil {
		return nil, errors.New("current request image could not be encoded as PNG")
	}
	if output.Len() > media.MaxImageBytes {
		return nil, errors.New("current request image exceeds the PNG upload size limit")
	}
	return output.Bytes(), nil
}
