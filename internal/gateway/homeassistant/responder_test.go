package homeassistant

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

func TestCommandResponseText(t *testing.T) {
	tests := []struct {
		name      string
		result    commands.Result
		want      string
		supported bool
	}{
		{
			name:      "response without attachments",
			result:    commands.Result{Text: "command complete"},
			want:      "command complete",
			supported: true,
		},
		{
			name: "single text attachment",
			result: commands.Result{
				Text:        "file attached",
				Attachments: []commands.Attachment{{Filename: "export.txt", MIMEType: "text/plain; charset=utf-8", Data: []byte("plain text")}},
			},
			want:      "plain text",
			supported: true,
		},
		{
			name: "multipart text attachments",
			result: commands.Result{
				Text: "files attached",
				Attachments: []commands.Attachment{
					{Filename: "part001.txt", MIMEType: "text/plain; charset=utf-8", Data: []byte("first ")},
					{Filename: "part002.txt", MIMEType: "text/plain; charset=utf-8", Data: []byte("second")},
				},
			},
			want:      "first second",
			supported: true,
		},
		{
			name: "non-text attachment",
			result: commands.Result{Attachments: []commands.Attachment{{
				Filename: "export.json", MIMEType: "application/json", Data: []byte(`{"ok":true}`),
			}}},
			supported: false,
		},
		{
			name: "mixed attachments",
			result: commands.Result{Attachments: []commands.Attachment{
				{Filename: "export.txt", MIMEType: "text/plain", Data: []byte("text")},
				{Filename: "image.png", MIMEType: "image/png", Data: []byte("png")},
			}},
			supported: false,
		},
		{
			name: "invalid utf8",
			result: commands.Result{Attachments: []commands.Attachment{{
				Filename: "export.txt", MIMEType: "text/plain", Data: []byte{0xff},
			}}},
			supported: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, supported, err := commandResponseText(test.result)
			if err != nil {
				t.Fatal(err)
			}
			if supported != test.supported || got != test.want {
				t.Fatalf("commandResponseText()=(%q, %t), want (%q, %t)", got, supported, test.want, test.supported)
			}
		})
	}
}

func TestCommandResponseTextPreservesAttachmentValidation(t *testing.T) {
	result := commands.Result{Attachments: []commands.Attachment{
		{Filename: "same.txt", MIMEType: "text/plain", Data: []byte("first")},
		{Filename: "same.txt", MIMEType: "text/plain", Data: []byte("second")},
	}}

	if _, _, err := commandResponseText(result); err == nil || !strings.Contains(err.Error(), "duplicate filename") {
		t.Fatalf("commandResponseText() error=%v, want duplicate filename validation error", err)
	}
}
