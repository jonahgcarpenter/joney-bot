package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSafeTextRedactsSensitiveValues(t *testing.T) {
	got := SafeText("token=abc123 email me@example.com http://user:pass@example.com/path?api_key=secret /home/alice/file 192.168.1.2 Bearer deadbeef /connect OSW-0123-4567-89AB-CDEF-GHJK OSW0123456789ABCDEFGHJK")
	for _, forbidden := range []string{"abc123", "me@example.com", "user:pass", "secret", "/home/alice", "192.168.1.2", "deadbeef", "0123-4567", "OSW0123"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("expected %q redacted from %q", forbidden, got)
		}
	}
}

func TestSafeErrorTextFallback(t *testing.T) {
	if SafeErrorText(nil) != FallbackErrorText {
		t.Fatal("expected nil error fallback")
	}
	if SafeErrorText(errors.New("password=hunter2")) != "password=[redacted]" {
		t.Fatalf("unexpected safe error text")
	}
}

func TestErrorFieldLogRedactsCanariesAndPreservesStructure(t *testing.T) {
	log := NewLogger(LevelDebug).Server("canary")
	record := captureLog(t, log, func() {
		log.Error("canary.failed", "canary operation failed",
			F("http_status", 502),
			F("status", "error"),
			ErrorField(errors.New("password=hunter2 user=alice@example.com token=abc123")),
		)
	})
	output, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, forbidden := range []string{"hunter2", "alice@example.com", "abc123"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sensitive canary %q leaked in log: %s", forbidden, text)
		}
	}
	for key, want := range map[string]any{"component": "canary", "event": "canary.failed", "http_status": float64(502), "status": "error", "error_code": "unknown_error"} {
		if record[key] != want {
			t.Fatalf("structured field %q missing from log: %s", key, text)
		}
	}
	if _, ok := record["error"]; ok {
		t.Fatal("raw error field emitted")
	}
}
