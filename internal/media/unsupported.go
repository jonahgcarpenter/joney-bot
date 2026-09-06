package media

import "strings"

// AttachmentLabel returns a readable label for an attachment in user-facing prompt notes.
func AttachmentLabel(name, mimeType string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType != "" {
		return mimeType
	}
	return "unknown file"
}
