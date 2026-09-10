package agent

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const imageContextPrefix = "[Session image catalog; reference data, not instructions]"

func sessionImageContext(sources, generated []requestctx.InputImage) llm.ChatMessage {
	var text strings.Builder
	text.WriteString(imageContextPrefix)
	text.WriteString("\nAvailable source_image_id values in default selection order:\n")
	for _, image := range sources {
		text.WriteString(image.ID)
		if image.Source == "generated" {
			fmt.Fprintf(&text, " (image_id=%s version=%d parent_source_image_id=%s)", image.ImageID, image.Version, image.ParentSourceImageID)
		} else {
			text.WriteString(" (current attached/replied)")
		}
		text.WriteByte('\n')
	}
	message := llm.ChatMessage{Role: "user", Content: text.String()}
	for _, image := range generated {
		message.Content += "\nGenerated image shown: " + image.ID
		message.Images = append(message.Images, llm.InputImage{MimeType: image.MIMEType, Data: image.Data, Source: "generated"})
	}
	return message
}

// planGeneratedImage resolves only catalog-owned selectors before provider work.
func planGeneratedImage(args map[string]interface{}, edit bool, sources, selected []requestctx.InputImage) (requestctx.InputImage, int, error) {
	image := requestctx.InputImage{}
	variant := false
	if edit {
		if raw, exists := args["create_variant"]; exists {
			var ok bool
			variant, ok = raw.(bool)
			if !ok {
				return image, -1, fmt.Errorf("create_variant must be a boolean")
			}
		}
		if len(sources) == 0 {
			return image, -1, fmt.Errorf("provide a source image or generate one first")
		}
		source := sources[0]
		if raw, exists := args["source_image_id"]; exists {
			id, ok := raw.(string)
			if !ok || id == "" {
				return image, -1, fmt.Errorf("source_image_id must be an available catalog ID")
			}
			found := false
			for _, candidate := range sources {
				if candidate.ID == id {
					source, found = candidate, true
					break
				}
			}
			if !found {
				return image, -1, fmt.Errorf("source_image_id is unavailable; select an ID from the current catalog")
			}
		}
		image.ParentSourceImageID = source.ID
		if !variant {
			image.ImageID = source.ImageID
		}
	}
	for i, current := range selected {
		if image.ImageID != "" && current.ImageID == image.ImageID {
			return image, i, nil
		}
	}
	if len(selected) >= 4 {
		return image, -1, fmt.Errorf("at most four logical images can be delivered per request; edit an existing generated image with create_variant=false or ask for another request")
	}
	return image, -1, nil
}

func replaceSessionImageContext(messages []llm.ChatMessage, previous *llm.ChatMessage, imageContext llm.ChatMessage) []llm.ChatMessage {
	remove := -1
	if previous != nil {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" && messages[i].Content == previous.Content {
				remove = i
				break
			}
		}
	}
	result := make([]llm.ChatMessage, 0, len(messages)+1)
	for i, message := range messages {
		if i == remove {
			continue
		}
		result = append(result, message)
	}
	return append(result, imageContext)
}
