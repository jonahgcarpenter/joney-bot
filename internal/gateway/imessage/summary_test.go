package imessage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func captureInfoSummaries(t *testing.T) (*config.Logger, func() []map[string]any) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "summary-log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	old := os.Stderr
	os.Stderr = file
	log := config.NewLogger(config.LevelInfo)
	os.Stderr = old
	return log, func() []map[string]any {
		data, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"private-filename", "private-address", "private-body", "private-mime", "private-chat", "private-attachment"} {
			if strings.Contains(string(data), private) {
				t.Fatalf("log contains %s", private)
			}
		}
		var events []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		return events
	}
}

func TestUnsupportedAttachmentSummaryAtInfoBeforeAccountResolution(t *testing.T) {
	log, events := captureInfoSummaries(t)
	g := &Gateway{Log: log}
	// Stop at identity normalization after media processing, without an account DB.
	g.processReceivedMessage(webhookMessage{
		Handle:      messageHandle{Address: "private-address"},
		Chats:       []messageChat{{GUID: "private-chat", Style: chatStyleDirect}},
		Attachments: []attachment{{GUID: "private-attachment", MimeType: "application/private-mime", TransferName: "private-filename"}},
	}, "req-input-summary", time.Now())
	count := 0
	for _, event := range events() {
		if event["event"] != "gateway.attachment.processed" {
			continue
		}
		count++
		if event["level"] != "info" || event["status"] != "degraded" || event["request_id"] != "req-input-summary" || event["gateway"] != "imessage" || event["user_id"] != nil || event["accepted_count"] != float64(0) || event["downgraded_count"] != float64(1) || event["declared_format_count"] != float64(1) {
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

func TestCapabilityResolutionSummariesAtInfo(t *testing.T) {
	for _, tc := range []struct {
		name, body, event, reason, status string
		httpStatus                        int
		available                         bool
	}{
		{"available", `{"data":{"private_api":true,"helper_connected":true}}`, "gateway.bluebubbles.capabilities", "", "ok", http.StatusOK, true},
		{"helper unavailable", `{"data":{"private_api":true,"helper_connected":false}}`, "gateway.bluebubbles.capabilities_unavailable", "private_api_or_helper_unavailable", "degraded", http.StatusOK, false},
		{"provider failure", "private-body", "gateway.bluebubbles.capabilities_unavailable", "probe_failed", "degraded", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, events := captureInfoSummaries(t)
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tc.httpStatus)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			g := &Gateway{Log: log, BlueBubblesURL: server.URL, HTTPClient: server.Client()}
			if available := g.refreshBlueBubblesCapabilitiesWithRetry(3, 0, g.log().With(config.F("request_id", "req-probe-summary"), config.F("user_id", "usr_synthetic"))); available != tc.available {
				t.Fatalf("available=%t", available)
			}
			wantAttempts := 3
			if tc.available {
				wantAttempts = 1
			}
			if got := attempts.Load(); got != int32(wantAttempts) {
				t.Fatalf("attempt count=%d", got)
			}
			got := events()
			if len(got) != 1 {
				t.Fatalf("expected one terminal summary, got %+v", got)
			}
			event := got[0]
			if event["event"] != tc.event || event["status"] != tc.status || event["attempt_count"] != float64(wantAttempts) || event["gateway"] != "imessage" || event["request_id"] != "req-probe-summary" || event["user_id"] != "usr_synthetic" {
				t.Fatalf("summary=%+v", event)
			}
			if tc.reason != "" && event["reason_code"] != tc.reason {
				t.Fatalf("reason=%v", event["reason_code"])
			}
			if duration, ok := event["duration_ms"].(float64); !ok || duration < 0 {
				t.Fatalf("duration=%v", event["duration_ms"])
			}
		})
	}
}
