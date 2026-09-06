package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestAttachmentAndEmbedShareOneInfoSummary(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "summary-log")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stderr
	os.Stderr = file
	log := config.NewLogger(config.LevelInfo)
	os.Stderr = old
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"id":"synthetic-response"}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "private-body")
	}))
	defer server.Close()
	g := &Gateway{Log: log, APIBaseURL: server.URL, HTTPClient: server.Client()}
	msg := MessageCreate{ChannelID: "private-chat", Attachments: []Attachment{{Filename: "private-filename", ContentType: "application/private-mime"}}, Embeds: []Embed{{Type: "image", Image: EmbedImage{URL: server.URL + "/asset"}}}}
	msg.Author.ID = "private-address"
	g.handleReceivedMessage(msg, "req-input-summary", time.Now())
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-filename", "private-address", "private-body", "private-mime", "private-chat", server.URL} {
		if strings.Contains(string(data), private) {
			t.Fatalf("log contains %s", private)
		}
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] != "gateway.attachment.processed" {
			continue
		}
		count++
		if event["level"] != "info" || event["status"] != "degraded" || event["gateway"] != "discord" || event["request_id"] != "req-input-summary" || event["user_id"] != nil || event["accepted_count"] != float64(0) || event["downgraded_count"] != float64(2) || event["declared_format_count"] != float64(1) || event["declared_embed_count"] != float64(1) {
			t.Fatalf("summary=%+v", event)
		}
		if duration, ok := event["duration_ms"].(float64); !ok || duration < 0 {
			t.Fatalf("duration=%v", event["duration_ms"])
		}
	}
	if count != 1 {
		t.Fatalf("summary count=%d", count)
	}
}
