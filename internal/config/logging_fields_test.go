package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type telemetryString string

func (telemetryString) String() string               { panic("String must not run") }
func (telemetryString) MarshalJSON() ([]byte, error) { panic("MarshalJSON must not run") }

type telemetryInt int64

func (telemetryInt) String() string               { panic("String must not run") }
func (telemetryInt) MarshalJSON() ([]byte, error) { panic("MarshalJSON must not run") }

type telemetryBool bool

func (telemetryBool) MarshalJSON() ([]byte, error) { panic("MarshalJSON must not run") }

type telemetryFloat float64

func (telemetryFloat) MarshalJSON() ([]byte, error) { panic("MarshalJSON must not run") }

func TestInfoOperationalMetadataCompleteness(t *testing.T) {
	// Samples reflect vetted production sources, not arbitrary personal names or
	// free-form error text. Assert values AND JSON types, not substring matches.
	want := map[string]any{
		"challenge_id": "chl_0123456789abcdef0123456789abcdef",
		"server_id":    "mcp_1788714000000000000",
		"command":      "help", "command_name": "unknown", "prior_release": "v4.0.8", "target_release": "v4.0.9",
		"mode": "text_to_image", "server": "github", "remote_tool_name": "list_issues", "tool_outcome": "unproductive",
		"embedding_model": "ollama/nomic-embed-text:latest", "log_level": "info", "default_method": "private-api",
		"normalized_mime": "image/png", "event_type": "READY", "gateways": "discord, imessage, homeassistant",
		"tools": "time.current,user_memory_save,web.search", "phase": "connect", "reason_code": "no_results",
		"request_kind": "prompt", "prompt_type": "text_image", "execution_status": "ok", "delivery_status": "ok",
		"persistence_status": "pending", "response_kind": "answer", "record_kind": "measurement", "workload": "foreground",
		"source": "x_oembed", "build_version": "v4.0.9", "build_revision": "abc123", "go_version": "go1.25.7",
		"retry_at": "2026-09-06T19:00:00.123+01:00", "delay_ms": float64(5000), "retry_attempt": float64(1),
		"identity_assurance": string(identity.AssuranceSelfAsserted), "port": float64(8000),
	}
	fields := make([]Field, 0, len(want))
	for key, value := range want {
		fields = append(fields, F(key, value))
	}
	fields = append(fields, F("port", "8000"), F("identity_assurance", identity.AssuranceSelfAsserted))
	logger := NewLogger(LevelInfo)
	record := captureLog(t, logger, func() { logger.Info("metadata.complete", "fixed operational message", fields...) })
	assertEnvelope(t, record)
	if record["event"] != "metadata.complete" {
		t.Fatalf("unexpected fallback: %v", record)
	}
	for key, value := range want {
		if !reflect.DeepEqual(record[key], value) {
			t.Errorf("%s: got %#v (%T), want %#v (%T)", key, record[key], record[key], value, value)
		}
	}
}

func TestNamedScalarsNeverInvokeMethods(t *testing.T) {
	logger := NewLogger(LevelInfo).With(F("request_id", telemetryString("req_1")), F("job_id", telemetryInt(12)))
	record := captureLog(t, logger, func() {
		logger.Info("scalar.complete", "fixed", F("status", telemetryString("ok")), F("tool_outcome", telemetryString("productive")),
			F("attempt_count", telemetryInt(3)), F("is_submitted", telemetryBool(true)), F("effective_output_tps", telemetryFloat(2.5)),
			F("counts", []telemetryInt{1, 2}), F("tool_name", []telemetryString{"web.search", "time.current"}))
	})
	want := map[string]any{"request_id": "req_1", "job_id": float64(12), "status": "ok", "tool_outcome": "productive", "attempt_count": float64(3), "is_submitted": true, "effective_output_tps": 2.5, "counts": []any{float64(1), float64(2)}, "tool_name": []any{"web.search", "time.current"}}
	for key, value := range want {
		if !reflect.DeepEqual(record[key], value) {
			t.Errorf("%s: got %#v want %#v", key, record[key], value)
		}
	}
}

func TestPrivateFieldsOmittedAtEveryLevelAndScope(t *testing.T) {
	for _, level := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		for _, fallback := range []bool{false, true} {
			for _, value := range []any{"private canary", int64(123), telemetryString("private_canary"), []int{123}} {
				fields := []Field{}
				for _, key := range []string{"session_id", "chat_id", "account", "identifier", "external_id", "display_name", "client_id", "message_guid", "reply_message_id", "sender_email", "prompt", "response", "reasoning", "url", "headers", "arguments", "results"} {
					fields = append(fields, F(key, value))
				}
				logger := NewLogger(LevelDebug).With(fields...).Agent("privacy", "req_1", "usr_1", "discord", "model", fields...)
				record := captureLog(t, logger, func() {
					if fallback {
						fields = append(fields, F("counts", struct{ Count int }{1}))
					}
					logger.log(level, "privacy.complete", "fixed", fields...)
				})
				for _, f := range fields {
					if f.Key != "counts" {
						if _, exists := record[f.Key]; exists {
							t.Fatalf("private key %q emitted at %s", f.Key, level)
						}
					}
				}
				assertEnvelope(t, record)
				if record["request_id"] != "req_1" {
					t.Fatal("lost correlation")
				}
				encoded, _ := json.Marshal(record)
				if strings.Contains(string(encoded), "private_canary") || strings.Contains(string(encoded), "private_session") {
					t.Fatal("private value emitted")
				}
			}
		}
	}
}

func TestMetadataRejectsUnknownEventsAndUnsafeStrings(t *testing.T) {
	for key, value := range map[string]any{"event_type": "unreviewed_private_event", "gateways": "discord, private_name", "command_name": "help private_argument", "remote_tool_name": "https://private.invalid", "retry_at": "private", "port": "private", "unknown_metadata": "private"} {
		logger := NewLogger(LevelInfo)
		record := captureLog(t, logger, func() { logger.Info("metadata.test", "fixed", F(key, value)) })
		if record[key] == value {
			t.Fatalf("unsafe %s retained", key)
		}
	}
	// time.Time and arbitrary count structs are not silently serialized. Owners
	// must supply RFC3339 strings / explicit numeric metrics.
	for _, value := range []any{time.Now(), struct{ Count int }{1}} {
		logger := NewLogger(LevelInfo)
		record := captureLog(t, logger, func() { logger.Info("metadata.test", "fixed", F("retry_at", value)) })
		if record["event"] != "logger.marshal_failed" {
			t.Fatalf("accepted object %T", value)
		}
	}
}
