package commands

import (
	"bytes"
	"strings"
	"testing"
)

func TestAttachmentValidate(t *testing.T) {
	valid := Attachment{Filename: "export.json", MIMEType: "application/json", Data: []byte(`{"ok":true}`)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid attachment rejected: %v", err)
	}

	tests := []Attachment{
		{Filename: "../export.json", MIMEType: "application/json", Data: []byte("x")},
		{Filename: `dir\export.json`, MIMEType: "application/json", Data: []byte("x")},
		{Filename: "export.json", MIMEType: "not-a-mime", Data: []byte("x")},
		{Filename: "export.json", MIMEType: "application/json", Data: nil},
		{Filename: "export.json", MIMEType: "application/json", Data: bytes.Repeat([]byte("x"), MaxAttachmentBytes+1)},
	}
	for _, attachment := range tests {
		if err := attachment.Validate(); err == nil {
			t.Fatalf("invalid attachment accepted: filename=%q mime=%q bytes=%d", attachment.Filename, attachment.MIMEType, len(attachment.Data))
		}
	}
}

func TestResultValidateAttachments(t *testing.T) {
	attachment := func(name string, size int) Attachment {
		return Attachment{Filename: name, MIMEType: "application/octet-stream", Data: bytes.Repeat([]byte("x"), size)}
	}
	valid := make([]Attachment, MaxAttachments)
	for i := range valid {
		valid[i] = attachment("part"+string(rune('a'+i)), MaxAttachmentBytes)
	}
	if err := (Result{Attachments: valid}).ValidateAttachments(); err != nil {
		t.Fatalf("exact aggregate limit rejected: %v", err)
	}

	duplicate := Result{Attachments: []Attachment{attachment("same", 1), attachment("same", 1)}}
	if err := duplicate.ValidateAttachments(); err == nil || !strings.Contains(err.Error(), "duplicate filename") {
		t.Fatalf("duplicate filenames err=%v", err)
	}

	overTotal := append(append([]Attachment(nil), valid...), attachment("extra", 1))
	if err := (Result{Attachments: overTotal}).ValidateAttachments(); err == nil {
		t.Fatal("response over aggregate limits was accepted")
	}
}
