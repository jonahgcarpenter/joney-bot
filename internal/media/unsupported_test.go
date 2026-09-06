package media

import "testing"

func TestAttachmentLabelFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		mimeType string
		want     string
	}{
		{name: " report.pdf ", mimeType: "application/pdf", want: "report.pdf"},
		{name: "", mimeType: " image/tiff ", want: "image/tiff"},
		{name: " ", mimeType: " ", want: "unknown file"},
	}

	for _, tt := range tests {
		if got := AttachmentLabel(tt.name, tt.mimeType); got != tt.want {
			t.Fatalf("AttachmentLabel(%q, %q) = %q, want %q", tt.name, tt.mimeType, got, tt.want)
		}
	}
}
