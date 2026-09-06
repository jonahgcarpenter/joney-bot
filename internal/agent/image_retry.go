package agent

import (
	"context"
	"math"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

const (
	imageSizeFallback        = "Your image is too big. Crop it and try again."
	maxImageModelAttempts    = 5
	imageRetryScale          = 0.75
	imageInitialScaleMaxEdge = 1920
)

func (a *Agent) chatWithImageRetries(ctx context.Context, req llm.ChatRequest, callback func(llm.ChatMessage), log *config.Logger) (*llm.ChatResponse, error, bool) {
	originalMessages := req.Messages
	imageCount := 0
	for _, message := range originalMessages {
		imageCount += len(message.Images)
	}

	var firstErr error
	for attempt := 1; attempt <= maxImageModelAttempts; attempt++ {
		if imageCount > 0 {
			messages := append([]llm.ChatMessage(nil), originalMessages...)
			for i := range messages {
				if len(originalMessages[i].Images) == 0 {
					continue
				}
				resized, err := media.ResizeInputImagesForAttempt(originalMessages[i].Images, attempt, imageRetryScale, imageInitialScaleMaxEdge)
				if err != nil {
					log.Warn("agent.model.image_retry_resize_failed", "failed to resize images for model retry",
						config.F("attempt", attempt), config.F("image_count", imageCount),
						config.F("status", "degraded"), config.ErrorField(err))
					return nil, err, false
				}
				messages[i].Images = resized
			}
			req.Messages = messages
		}

		resp, err := a.chatClient.Chat(ctx, req, callback)
		if ctx.Err() != nil {
			return nil, ctx.Err(), false
		}
		if err == nil {
			if attempt > 1 {
				log.Info("agent.model.image_retry_recovered", "model recovered after image resize", config.F("attempt_count", attempt), config.F("status", "ok"))
			}
			return resp, nil, false
		}
		if imageCount == 0 || !llm.IsOllamaModelRunnerStoppedError(err) {
			return nil, err, false
		}
		if firstErr == nil {
			firstErr = err
		}
		if attempt == maxImageModelAttempts {
			log.Warn("agent.model.image_retry_exhausted", "model runner stopped after resized image retries",
				config.F("attempt_count", attempt), config.F("image_count", imageCount),
				config.F("status", "degraded"), config.ErrorField(err))
			return nil, err, true
		}
		log.Warn("agent.model.image_retry", "retrying model call with smaller images",
			config.F("attempt", attempt+1), config.F("image_count", imageCount),
			config.F("scale_percent", int(math.Pow(imageRetryScale, float64(attempt+1))*100)),
			config.F("status", "retry"))
	}
	return nil, firstErr, false
}
