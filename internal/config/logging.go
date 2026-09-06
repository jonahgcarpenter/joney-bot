package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const serviceName = "oswald-ai"

var reservedLogFields = map[string]struct{}{
	"ts": {}, "level": {}, "service": {}, "log_type": {}, "component": {}, "event": {}, "msg": {},
	"log_schema_version": {}, "instance_id": {},
}

var validLogStatuses = map[string]struct{}{
	"ok": {}, "error": {}, "rejected": {}, "retry": {}, "degraded": {},
}

// Level represents a logging severity level.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String returns the lowercase label for a Level.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// ParseLevel converts a string (case-insensitive) to a Level.
// Unknown values default to INFO.
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

// Field is a structured log field.
type Field struct {
	Key   string
	Value any
}

// F creates a structured log field, not a blanket redaction guarantee. Callers
// must use fixed operational labels and canonical/server-generated correlation
// values, never user content. The output boundary filters keys and value types.
func F(key string, value any) Field {
	return Field{Key: key, Value: value}
}

// ErrorField creates a fixed error_code field without invoking err.Error.
func ErrorField(err error) Field {
	if err == nil {
		return Field{}
	}
	return F("error_code", ErrorCode(err))
}

// Logger emits structured JSON logs to stderr. Events, components, and messages
// must be fixed developer-owned text, never interpolated inputs or errors.
// Field filtering cannot prove the provenance of canonical IDs or model labels.
type Logger struct {
	level      Level
	logger     *log.Logger
	logType    string
	component  string
	fields     []Field
	agent      []Field
	instanceID string
}

// NewLogger creates a Logger that writes JSON to stderr at the given minimum level.
func NewLogger(level Level) *Logger {
	return &Logger{
		level:      level,
		logger:     log.New(os.Stderr, "", 0),
		logType:    "server",
		component:  "app",
		instanceID: NewRequestID(),
	}
}

// With returns a logger that always includes the supplied fields.
func (l *Logger) With(fields ...Field) *Logger {
	merged := make([]Field, 0, len(l.fields)+len(fields))
	merged = append(merged, l.fields...)
	for _, field := range fields {
		if field.Key == "" || isReservedLogField(field.Key) {
			continue
		}
		merged = append(merged, field)
	}
	return &Logger{level: l.level, logger: l.logger, logType: l.logType, component: l.component, fields: merged, agent: l.agent, instanceID: l.instanceID}
}

// SetOutput changes the destination used by this logger and its scoped children.
func (l *Logger) SetOutput(w io.Writer) {
	l.logger.SetOutput(w)
}

// Server returns a server-scoped logger for the given component.
func (l *Logger) Server(component string, fields ...Field) *Logger {
	scoped := l.With(fields...)
	scoped.logType = "server"
	scoped.component = component
	return scoped
}

// Agent attaches canonical correlation, never session or external identifiers.
func (l *Logger) Agent(component, requestID, userID, gateway, model string, fields ...Field) *Logger {
	scoped := l.With(fields...)
	scoped.logType = "agent"
	scoped.component = component
	scoped.agent = []Field{
		F("request_id", requestID),
		F("user_id", userID),
		F("gateway", gateway),
		F("model", model),
	}
	return scoped
}

func (l *Logger) log(level Level, event, msg string, fields ...Field) {
	if level < l.level {
		return
	}

	payload := map[string]any{
		"ts":                 time.Now().UTC().Format(time.RFC3339Nano),
		"level":              level.String(),
		"service":            serviceName,
		"log_type":           l.logType,
		"component":          safeLogLabel(l.component),
		"event":              safeLogLabel(event),
		"msg":                boundedLogString(msg, maxLogMessageBytes),
		"log_schema_version": 1,
		"instance_id":        l.instanceID,
		"record_kind":        "event",
	}

	valid := true
	for _, field := range l.fields {
		if field.Key == "" || field.Value == nil || isReservedLogField(field.Key) {
			continue
		}
		valid = addLogField(payload, field) && valid
	}
	for _, field := range fields {
		if field.Key == "" || field.Value == nil || isReservedLogField(field.Key) {
			continue
		}
		valid = addLogField(payload, field) && valid
	}
	for _, field := range l.agent {
		valid = addLogField(payload, field) && valid
	}

	line, err := json.Marshal(payload)
	if err != nil || !valid || len(line) > maxLogRecordBytes {
		fallback := map[string]any{
			"ts":                 time.Now().UTC().Format(time.RFC3339Nano),
			"level":              "error",
			"service":            serviceName,
			"log_type":           l.logType,
			"component":          safeLogLabel(l.component),
			"event":              "logger.marshal_failed",
			"msg":                "failed to marshal log payload",
			"status":             "error",
			"error_code":         "invalid_log_payload",
			"log_schema_version": 1,
			"instance_id":        l.instanceID,
			"record_kind":        "event",
		}
		for _, key := range correlationLogKeys {
			if value, ok := payload[key]; ok {
				fallback[key] = value
			}
		}
		line, _ = json.Marshal(fallback)
	}

	l.logger.Print(string(line))
}

func isReservedLogField(key string) bool {
	_, reserved := reservedLogFields[key]
	return reserved
}

// Debug logs a message at DEBUG level.
func (l *Logger) Debug(event, msg string, fields ...Field) {
	l.log(LevelDebug, event, msg, fields...)
}

// Info logs a message at INFO level.
func (l *Logger) Info(event, msg string, fields ...Field) {
	l.log(LevelInfo, event, msg, fields...)
}

// Warn logs a message at WARN level.
func (l *Logger) Warn(event, msg string, fields ...Field) {
	l.log(LevelWarn, event, msg, fields...)
}

// Error logs a message at ERROR level.
func (l *Logger) Error(event, msg string, fields ...Field) {
	l.log(LevelError, event, msg, fields...)
}

// Fatal logs a message at ERROR level then terminates the process.
func (l *Logger) Fatal(event, msg string, fields ...Field) {
	l.log(LevelError, event, msg, fields...)
	os.Exit(1)
}

// NewRequestID creates a short per-request correlation ID.
func NewRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "req_" + strconv.FormatInt(time.Now().UnixNano(), 16) + "_" + strconv.FormatUint(requestIDFallback.Add(1), 16)
	}
	return "req_" + hex.EncodeToString(b)
}

var requestIDFallback atomic.Uint64
