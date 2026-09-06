package config

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func captureLog(t *testing.T, logger *Logger, emit func()) map[string]any {
	t.Helper()
	var output bytes.Buffer
	logger.SetOutput(&output)
	emit()
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("decode log: %v; output=%q", err, output.String())
	}
	return record
}

func assertEnvelope(t *testing.T, record map[string]any) {
	t.Helper()
	for _, key := range []string{"ts", "level", "service", "log_type", "component", "event", "msg"} {
		value, ok := record[key]
		if !ok {
			t.Fatalf("missing envelope field %q: %#v", key, record)
		}
		if _, ok := value.(string); !ok {
			t.Fatalf("envelope field %q has type %T, want string", key, value)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, record["ts"].(string)); err != nil {
		t.Fatalf("invalid timestamp: %v", err)
	}
	if record["log_schema_version"] != float64(1) {
		t.Fatalf("bad schema version: %v", record)
	}
	if id, ok := record["instance_id"].(string); !ok || id == "" {
		t.Fatalf("bad instance ID: %v", record)
	}
	if _, ok := record["session_id"]; ok {
		t.Fatal("private session ID emitted")
	}
}

func TestLoggerEnvelopeAndReservedFieldsCannotBeOverwritten(t *testing.T) {
	logger := NewLogger(LevelDebug).Server("contract",
		F("service", "attacker"), F("component", "attacker"), F("event", "attacker"),
	)
	record := captureLog(t, logger, func() {
		logger.Info("contract.complete", "contract message",
			F("ts", "attacker"), F("level", "attacker"), F("service", "attacker"),
			F("log_type", "attacker"), F("component", "attacker"), F("event", "attacker"), F("msg", "attacker"),
			F("status", "ok"),
		)
	})
	assertEnvelope(t, record)
	want := map[string]string{
		"level": "info", "service": serviceName, "log_type": "server", "component": "contract",
		"event": "contract.complete", "msg": "contract message", "status": "ok",
	}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("%s=%v, want %q", key, record[key], value)
		}
	}
}

func TestLoggerRootAndAgentEnvelopesAreComplete(t *testing.T) {
	for name, logger := range map[string]*Logger{
		"root":  NewLogger(LevelDebug),
		"agent": NewLogger(LevelDebug).Agent("agent", "request", "user", "gateway", "model"),
	} {
		t.Run(name, func(t *testing.T) {
			record := captureLog(t, logger, func() { logger.Debug("contract.event", "message") })
			assertEnvelope(t, record)
		})
	}
}

func TestLoggerAgentFoundationCannotBeOverwritten(t *testing.T) {
	logger := NewLogger(LevelDebug).With(
		F("request_id", "inherited-request"), F("session_id", "inherited-session"),
		F("user_id", "inherited-user"), F("gateway", "inherited-gateway"), F("model", "inherited-model"),
	).Agent("agent-component", "request", "user", "gateway", "model",
		F("request_id", "scoped-request"), F("session_id", "scoped-session"),
		F("user_id", "scoped-user"), F("gateway", "scoped-gateway"), F("model", "scoped-model"),
	).With(
		F("request_id", "later-request"), F("session_id", "later-session"),
		F("user_id", "later-user"), F("gateway", "later-gateway"), F("model", "later-model"),
	)
	record := captureLog(t, logger, func() {
		logger.Info("agent.complete", "message",
			F("request_id", "event-request"), F("session_id", "event-session"),
			F("user_id", "event-user"), F("gateway", "event-gateway"), F("model", "event-model"),
			F("log_type", "server"), F("component", "event-component"),
		)
	})
	want := map[string]string{
		"request_id": "request", "user_id": "user",
		"gateway": "gateway", "model": "model", "log_type": "agent", "component": "agent-component",
	}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("%s=%v, want %q; record=%#v", key, record[key], value, record)
		}
	}
}

func TestLoggerNormalizesStatusVocabulary(t *testing.T) {
	valid := []string{"ok", "error", "rejected", "retry", "degraded"}
	for _, status := range valid {
		logger := NewLogger(LevelDebug)
		record := captureLog(t, logger, func() { logger.Info("status.test", "message", F("status", status)) })
		if record["status"] != status {
			t.Fatalf("status=%v, want %q", record["status"], status)
		}
	}
	logger := NewLogger(LevelDebug)
	record := captureLog(t, logger, func() { logger.Info("status.test", "message", F("status", "completed")) })
	if record["status"] != "degraded" {
		t.Fatalf("invalid status normalized to %v", record["status"])
	}
}

func TestLoggerMarshalFailureHasCompleteEnvelope(t *testing.T) {
	logger := NewLogger(LevelDebug).Agent("contract", "request", "user", "gateway", "model")
	record := captureLog(t, logger, func() {
		logger.Info("original.event", "original message", F("unmarshalable", make(chan int)))
	})
	assertEnvelope(t, record)
	if record["event"] != "logger.marshal_failed" || record["status"] != "error" || record["log_type"] != "agent" || record["component"] != "contract" {
		t.Fatalf("unexpected fallback envelope: %#v", record)
	}
	for key, value := range map[string]string{
		"request_id": "request", "user_id": "user", "gateway": "gateway", "model": "model",
	} {
		if record[key] != value {
			t.Fatalf("fallback %s=%v, want %q; record=%#v", key, record[key], value, record)
		}
	}
}

func TestLoggerSecretPromptAndMCPCanariesAreRedacted(t *testing.T) {
	logger := NewLogger(LevelDebug).Server("canary")
	record := captureLog(t, logger, func() {
		logger.Error("canary.failed", "safe fixed message", ErrorField(assertionError("password=mcp-secret token=prompt-secret user=person@example.com endpoint=https://mcp-user:mcp-password@example.com/tools?api_key=mcp-api-key")))
	})
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"mcp-secret", "prompt-secret", "person@example.com", "mcp-user", "mcp-password", "mcp-api-key"} {
		if strings.Contains(string(raw), canary) {
			t.Fatalf("canary %q leaked: %s", canary, raw)
		}
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }

type logCanary struct{ called *bool }

func (c logCanary) MarshalJSON() ([]byte, error) {
	*c.called = true
	return nil, errors.New("private marshaler prose https://secret.invalid")
}
func (c logCanary) String() string { *c.called = true; return "private stringer prose" }

func TestLoggerPrivacyBoundaryAndFallbackCorrelation(t *testing.T) {
	for _, level := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		for _, fallback := range []bool{false, true} {
			called := false
			logger := NewLogger(LevelDebug).With(F("request_id", "req_1"), F("operation_id", "op_1"), F("job_id", int64(12)), F("user_id", "usr_1"), F("gateway", "discord"))
			record := captureLog(t, logger, func() {
				fields := []Field{F("unknown", "arbitrary private prose https://secret.invalid"), F("reason", "private prose"), ErrorField(errors.New("unrecognizable private prose")), F("duration_ms", int64(42)), F("account_count", 3)}
				for _, key := range []string{"session_id", "chat_id", "account", "external_id", "target_account_id", "identifier", "display_name", "client_id", "email", "prompt", "response", "reasoning", "url", "headers", "arguments", "results"} {
					fields = append(fields, F(key, logCanary{&called}))
				}
				if fallback {
					fields = append(fields, F("object", logCanary{&called}))
				}
				logger.log(level, "privacy.test", "fixed message", fields...)
			})
			assertEnvelope(t, record)
			if called {
				t.Fatal("called custom logging methods")
			}
			raw, _ := json.Marshal(record)
			for _, secret := range []string{"private", "secret.invalid", "unrecognizable"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("leaked canary: %s", raw)
				}
			}
			for key, value := range map[string]any{"request_id": "req_1", "operation_id": "op_1", "job_id": float64(12), "user_id": "usr_1", "gateway": "discord"} {
				if record[key] != value {
					t.Fatalf("lost %s: %v", key, record)
				}
			}
			if fallback && record["event"] != "logger.marshal_failed" {
				t.Fatal("missing fallback")
			}
			if !fallback && (record["duration_ms"] != float64(42) || record["account_count"] != float64(3)) {
				t.Fatal("lost metrics")
			}
		}
	}
}

func TestLoggerInstanceAndBounds(t *testing.T) {
	root := NewLogger(LevelInfo)
	child := root.Server("child").With(F("instance_id", "spoof"), F("log_schema_version", 99)).Agent("agent", "req", "usr", "discord", "model")
	if root.instanceID != child.instanceID || root.instanceID == NewLogger(LevelInfo).instanceID {
		t.Fatal("instance inheritance/uniqueness")
	}
	record := captureLog(t, child, func() {
		child.Info("bounds.test", strings.Repeat("x", 2000), F("model", strings.Repeat("x", 1000)), F("counts", make([]int, 100)))
	})
	assertEnvelope(t, record)
	if len(record["msg"].(string)) != maxLogMessageBytes || len(record["counts"].([]any)) != maxLogArrayItems {
		t.Fatal("unbounded fields")
	}
	record = captureLog(t, root, func() {
		fields := make([]Field, 2000)
		for i := range fields {
			fields[i] = F(fmt.Sprintf("metric_%d", i), i)
		}
		root.Info("bounds.test", "fixed", fields...)
	})
	raw, _ := json.Marshal(record)
	if len(raw) > maxLogRecordBytes || record["event"] != "logger.marshal_failed" {
		t.Fatal("record not bounded")
	}
}

type statusError int

func (e statusError) Error() string       { panic("must not format errors") }
func (e statusError) HTTPStatusCode() int { return int(e) }

func TestErrorClassificationNeverFormatsText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{nil, ""}, {errors.New("arbitrary private prose"), "unknown_error"},
		{fmt.Errorf("private: %w", context.Canceled), "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"}, {sql.ErrNoRows, "sql_no_rows"}, {sql.ErrTxDone, "sql_tx_done"},
		{&url.Error{Op: "private", URL: "https://secret.invalid", Err: context.DeadlineExceeded}, "deadline_exceeded"},
		{&net.OpError{Op: "private", Err: errors.New("private")}, "network_error"},
		{&json.SyntaxError{}, "json_syntax"}, {&json.UnmarshalTypeError{Value: "private"}, "json_type"},
		{&fs.PathError{Path: "private", Err: fs.ErrNotExist}, "file_not_found"},
		{statusError(429), "http_rate_limited"}, {statusError(503), "http_server_error"}, {statusError(401), "http_client_error"},
	} {
		if got := ErrorCode(tc.err); got != tc.code {
			t.Fatalf("%T: got %q want %q", tc.err, got, tc.code)
		}
	}
	for _, code := range []int{0, 99, 200, 429, 599, 600} {
		want := code
		if code < 100 || code > 599 {
			want = 0
		}
		if got := HTTPStatus(fmt.Errorf("wrapped: %w", statusError(code))); got != want {
			t.Fatalf("status %d: %d", code, got)
		}
	}
}

func TestLoggerRejectsObjectsAndInvalidCorrelationTypes(t *testing.T) {
	for _, value := range []any{map[string]any{"private": "private"}, []byte("private"), math.NaN(), math.Inf(1), []any{"private"}, make(chan int), func() {}} {
		logger := NewLogger(LevelInfo)
		record := captureLog(t, logger, func() { logger.Info("types.test", "fixed", F("object", value), F("request_id", "req_1")) })
		if record["event"] != "logger.marshal_failed" || record["request_id"] != "req_1" {
			t.Fatalf("unsafe %T: %v", value, record)
		}
	}
	logger := NewLogger(LevelInfo)
	record := captureLog(t, logger, func() { logger.Info("types.test", "fixed", F("request_id", []string{"req_1"})) })
	if _, ok := record["request_id"]; ok {
		t.Fatal("non-scalar correlation escaped fallback")
	}
	for _, status := range []any{42, []string{"ok"}, logCanary{new(bool)}} {
		record = captureLog(t, logger, func() { logger.Info("types.test", "fixed", F("status", status)) })
		if record["status"] != "degraded" {
			t.Fatal("invalid status not normalized")
		}
	}
}
