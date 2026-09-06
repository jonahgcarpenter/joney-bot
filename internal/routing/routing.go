package routing

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// Decide applies the shared gateway policy for deciding if and how a message reaches the LLM.
func Decide(input Input) Decision {
	text := strings.TrimSpace(input.Text)
	reply := input.Reply

	if input.IsCommandAttempt {
		if input.IsGroup && !input.IsMention {
			return Decision{Action: ActionIgnore, Reason: "group_command_without_mention"}
		}
		return Decision{Action: ActionCommand, Prompt: text, Reason: "command"}
	}

	if ShouldIgnoreUninvokedGroup(input.IsGroup, input.IsMention, input.IsReplyToBot, input.IsCommandAttempt) {
		return Decision{Action: ActionIgnore, Reason: "group_message_without_invocation"}
	}

	images, unsupported := combineImages(input.CurrentImages, input.CurrentUnsupported, reply)
	prompt := BuildPrompt(text, images, unsupported, reply)
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		return Decision{Action: ActionGatewayFallback, ResponseText: "What do you want idiot.", Reason: "empty_prompt"}
	}

	return Decision{Action: ActionLLM, Prompt: prompt, Images: images, Reason: "llm"}
}

// Preflight cheaply identifies messages that should be ignored before gateways
// perform expensive attachment, account, or reply-context work.
func Preflight(input PreflightInput) Decision {
	isCommandAttempt := IsCommandAttempt(input.Text)
	if isCommandAttempt {
		if input.IsGroup && !input.IsMention {
			return Decision{Action: ActionIgnore, Reason: "group_command_without_mention"}
		}
		return Decision{}
	}

	if ShouldIgnoreUninvokedGroup(input.IsGroup, input.IsMention, input.IsReplyToBot, isCommandAttempt) {
		return Decision{Action: ActionIgnore, Reason: "group_message_without_invocation"}
	}
	return Decision{}
}

// IsCommandAttempt reports whether text begins with command syntax.
func IsCommandAttempt(text string) bool {
	return commands.IsAttempt(text)
}

// ShouldIgnoreUninvokedGroup reports whether a group message lacks any gateway-normalized invocation.
func ShouldIgnoreUninvokedGroup(isGroup, isMention, isReplyToBot, isCommand bool) bool {
	return isGroup && !isMention && !isReplyToBot && !isCommand
}

// BuildPrompt builds the exact current-turn text sent to the LLM.
func BuildPrompt(text string, images []llm.InputImage, unsupported []string, reply *ReplyContext) string {
	parts := make([]string, 0, 3)
	if replyText := formatReplyContext(reply); replyText != "" {
		parts = append(parts, replyText)
	}

	text = strings.TrimSpace(text)
	if text == "" && len(images) > 0 {
		text = imageOnlyNote(images)
	}
	if text != "" {
		parts = append(parts, text)
	}
	if note := UnsupportedFilesNote(unsupported); note != "" {
		parts = append(parts, note)
	}

	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// UnsupportedFilesNote returns the canonical fallback text for current-turn unsupported attachments.
func UnsupportedFilesNote(labels []string) string {
	labels = compactLabels(labels)
	if len(labels) == 0 {
		return ""
	}
	if len(labels) == 1 {
		return fmt.Sprintf("[User sent an unsupported attachment: %s]", labels[0])
	}
	return fmt.Sprintf("[User sent unsupported attachments: %s]", strings.Join(labels, ", "))
}

func combineImages(currentImages []llm.InputImage, currentUnsupported []string, reply *ReplyContext) ([]llm.InputImage, []string) {
	images := append([]llm.InputImage(nil), currentImages...)
	unsupported := compactLabels(currentUnsupported)
	if reply == nil {
		return images, unsupported
	}

	for _, image := range reply.Images {
		if len(images) >= media.MaxImagesPerRequest {
			continue
		}
		if image.Data != "" {
			images = append(images, image)
		}
	}
	return images, compactLabels(unsupported)
}

func formatReplyContext(reply *ReplyContext) string {
	if reply == nil {
		return ""
	}
	name := strings.TrimSpace(reply.SenderName)
	text := strings.TrimSpace(reply.Text)
	unsupported := compactLabels(reply.Unsupported)

	if reply.IsUnavailable {
		if name != "" {
			return fmt.Sprintf("[Replying to %s: Message unavailable]", name)
		}
		return "[Replying to a message: Message unavailable]"
	}
	if reply.AttachmentUnavailable && name != "" {
		return fmt.Sprintf("[Replying to %s's attachment: Attachment unavailable]", name)
	}

	suffix := replyAttachmentSuffix(reply.Images, unsupported)
	switch {
	case text != "" && name != "" && suffix != "":
		return fmt.Sprintf("[Replying to %s: %q with %s]", name, text, suffix)
	case text != "" && name != "":
		return fmt.Sprintf("[Replying to %s: %q]", name, text)
	case text != "" && suffix != "":
		return fmt.Sprintf("[Replying to a message: %q with %s]", text, suffix)
	case text != "":
		return fmt.Sprintf("[Replying to a message: %q]", text)
	case name != "" && suffix != "":
		return fmt.Sprintf("[Replying to %s's %s]", name, suffix)
	case suffix != "":
		return fmt.Sprintf("[Replying to %s]", suffix)
	case name != "":
		return fmt.Sprintf("[Replying to %s: Message unavailable]", name)
	default:
		return "[Replying to a message: Message unavailable]"
	}
}

func replyAttachmentSuffix(images []llm.InputImage, unsupported []string) string {
	parts := make([]string, 0, 2)
	if len(images) > 0 {
		parts = append(parts, imageDescription(images))
	}
	if len(unsupported) == 1 {
		parts = append(parts, "unsupported attachment: "+unsupported[0])
	} else if len(unsupported) > 1 {
		parts = append(parts, "unsupported attachments: "+strings.Join(unsupported, ", "))
	}
	return strings.Join(parts, "; ")
}

func imageOnlyNote(images []llm.InputImage) string {
	count := len(images)
	gifCount := gifContactSheetCount(images)
	switch {
	case count == 1 && gifCount == 1:
		return "[User sent a GIF; the attached image is a contact sheet showing its contents over time]"
	case count > 1 && gifCount == count:
		return fmt.Sprintf("[User sent %d GIFs; the attached images are contact sheets showing their contents over time]", count)
	case gifCount > 0:
		return fmt.Sprintf("[User sent %d images, including GIF contact sheets showing their contents over time]", count)
	case count == 1:
		return "[User sent an image]"
	default:
		return fmt.Sprintf("[User sent %d images]", count)
	}
}

func imageDescription(images []llm.InputImage) string {
	count := len(images)
	gifCount := gifContactSheetCount(images)
	switch {
	case count == 1 && gifCount == 1:
		return "GIF contact sheet showing its contents over time"
	case count > 1 && gifCount == count:
		return fmt.Sprintf("%d GIF contact sheets showing their contents over time", count)
	case gifCount > 0:
		return fmt.Sprintf("%d images, including GIF contact sheets showing their contents over time", count)
	case count == 1:
		return "image"
	default:
		return fmt.Sprintf("%d images", count)
	}
}

func gifContactSheetCount(images []llm.InputImage) int {
	count := 0
	for _, image := range images {
		if image.IsGIFContactSheet {
			count++
		}
	}
	return count
}

func compactLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		result = append(result, label)
	}
	return result
}
