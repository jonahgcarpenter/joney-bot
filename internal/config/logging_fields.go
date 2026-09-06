package config

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxLogStringBytes  = 256
	maxLogMessageBytes = 1024
	maxLogArrayItems   = 16
	maxLogRecordBytes  = 16 * 1024
)

var correlationLogKeys = []string{
	"request_id", "user_id", "gateway", "model", "workload", "operation_id", "parent_operation_id", "job_id",
}

// String fields are opt-in. Numeric/bool metrics remain extensible, but private
// keys are rejected regardless of type. Labels are not a place for user prose.
var stringLogFields = map[string]bool{
	"request_id": true, "user_id": true, "actor_user_id": true, "target_user_id": true,
	"gateway": true, "model": true, "workload": true, "operation_id": true, "parent_operation_id": true,
	"status": true, "outcome": true, "error_code": true, "operation": true,
	"tool_name": true, "provider": true, "primary_provider": true, "fallback_provider": true,
	"kind": true, "scope": true, "reason": true, "failure_kind": true, "rate_limit_scope": true,
	"job_state": true, "formation_purpose": true, "generator_version": true, "extractor_version": true,
	"input_type": true, "prompt_type": true, "finish_reason": true, "transport": true,
	"phase": true, "reason_code": true, "response_kind": true, "request_kind": true,
	"execution_status": true, "delivery_status": true, "persistence_status": true, "record_kind": true,
	"done_reason": true, "job_kind": true, "entity_kind": true, "index_kind": true,
	"revision_state": true, "source": true, "server_name": true, "identity_assurance": true,
	"build_version": true, "build_revision": true, "go_version": true, "cleanup_reason": true,
	"trigger": true, "retry_at": true, "status_before": true, "status_after": true,
	// Registered command names, immutable migration releases, validated MCP
	// catalog names, and deployment-selected model names. Never raw command text.
	"command": true, "command_name": true, "prior_release": true, "target_release": true,
	"mode": true, "server": true, "remote_tool_name": true, "tool_outcome": true,
	"embedding_model": true, "log_level": true, "default_method": true, "normalized_mime": true,
	"event_type": true, "gateways": true, "tools": true, "port": true,
	// Server-generated persisted IDs: random challenge ID (not its code/hash)
	// and MCP config ID (not its URL or remote client/account identity).
	"challenge_id": true,
	"server_id":    true,
}

var privateLogFields = map[string]bool{
	"session": true, "sessionid": true, "sessionkey": true, "chat": true, "chatid": true,
	"account": true, "accountid": true, "accounts": true, "identifier": true, "identifiers": true,
	"externalid": true, "externalidentity": true, "externalidentities": true,
	"displayname": true, "name": true, "username": true, "clientid": true,
	"phone": true, "phonenumber": true, "email": true, "senderid": true, "authorid": true,
	"conversationid": true, "channelid": true, "guildid": true, "messageid": true,
	"messageguid": true,
	"prompt":      true, "prompts": true, "response": true, "responses": true, "reasoning": true,
	"text": true, "content": true, "currentusertext": true, "url": true, "urls": true,
	"uri": true, "endpoint": true, "headers": true, "header": true,
	"arguments": true, "args": true, "result": true, "results": true,
	"error": true, "err": true, "errormessage": true, "errortext": true,
	"path": true, "file": true, "filename": true, "token": true, "password": true, "secret": true,
	"authorization": true, "apikey": true, "credential": true, "credentials": true,
	"body": true, "payload": true, "query": true, "input": true, "output": true,
	"base64": true, "image": true, "images": true, "handle": true, "address": true,
	"externalids": true, "accountids": true, "sessionids": true, "chatids": true,
}

func privateLogKey(key string) bool {
	key = strings.ToLower(key)
	compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(key)
	if privateLogFields[compact] {
		return true
	}
	// Prefix variants such as target_account_id and replied_to_chat_id are private
	// too, while operational counts (account_count, prompt_tokens) remain useful.
	for _, suffix := range []string{"sessionid", "sessionkey", "chatid", "accountid", "externalid", "externalids", "identifier", "identifiers", "displayname", "clientid", "messageid", "messageguid", "email", "phone", "url", "headers", "arguments", "results", "authorization", "password", "secret", "apikey"} {
		if strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return false
}

func boundedLogString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func safeLogLabel(s string) string {
	if len(s) > maxLogStringBytes {
		return "redacted"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:/,-", c)) {
			return "redacted"
		}
	}
	if strings.Contains(s, "://") || strings.Contains(s, "@") {
		return "redacted"
	}
	return s
}

// addLogField never passes caller objects to encoding/json. Unsupported objects
// cause the fixed marshal_failed fallback without calling their methods.
func addLogField(payload map[string]any, field Field) bool {
	if field.Key == "" || len(field.Key) > 64 || safeLogLabel(field.Key) != field.Key || isReservedLogField(field.Key) || privateLogKey(field.Key) || field.Value == nil {
		return true
	}
	if field.Key == "status" {
		status := "degraded"
		if raw := reflect.ValueOf(field.Value); raw.Kind() == reflect.String {
			status = raw.String()
		}
		if _, valid := validLogStatuses[status]; !valid {
			status = "degraded"
		}
		payload[field.Key] = status
		return true
	}
	for _, key := range correlationLogKeys {
		if field.Key != key {
			continue
		}
		if key == "job_id" {
			switch reflect.TypeOf(field.Value).Kind() {
			case reflect.Int, reflect.Int64:
			default:
				return false
			}
		} else {
			if reflect.TypeOf(field.Value).Kind() != reflect.String {
				return false
			}
		}
	}
	value, keep, valid := logScalar(field.Key, field.Value)
	if !valid {
		return false
	}
	if keep {
		payload[field.Key] = value
	}
	return true
}

func logScalar(key string, value any) (any, bool, bool) {
	switch v := value.(type) {
	case string:
		if !stringLogFields[key] {
			return nil, false, true
		}
		// These fields have wire-specific syntax that generic labels must not
		// acquire (spaces in a gateway list, '+' in RFC3339 offsets).
		switch key {
		case "event_type":
			switch v {
			case "", "READY", "MESSAGE_CREATE", "RESUMED", "new-message", "typing-indicator", "unknown":
				return v, true, true
			}
			return "unknown", true, true
		case "gateways":
			if compact := strings.ReplaceAll(v, " ", ""); len(v) > maxLogStringBytes || safeLogLabel(compact) != compact {
				return "redacted", true, true
			}
			for _, gateway := range strings.Split(v, ",") {
				switch strings.TrimSpace(gateway) {
				case "discord", "imessage", "homeassistant":
				default:
					return "redacted", true, true
				}
			}
			return v, true, true
		case "port":
			if port, err := strconv.Atoi(v); err == nil && port > 0 && port <= 65535 {
				return port, true, true
			}
			return nil, false, true
		case "retry_at":
			if len(v) <= maxLogStringBytes {
				if _, err := time.Parse(time.RFC3339Nano, v); err == nil {
					return v, true, true
				}
			}
			return nil, false, true
		}
		return safeLogLabel(v), true, true
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, true, true
	case float32:
		return v, true, !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0)
	case float64:
		return v, true, !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	// Only unnamed slices/arrays of scalar elements are supported. Named scalars
	// are converted below; no caller methods, pointers, or objects are serialized.
	t := reflect.TypeOf(value)
	if t == nil {
		return nil, false, true
	}
	// Typed scalar metrics and enums are read directly, without invoking custom
	// marshaling or String methods on their named types.
	raw := reflect.ValueOf(value)
	switch t.Kind() {
	case reflect.String:
		return logScalar(key, raw.String())
	case reflect.Bool:
		return raw.Bool(), true, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return raw.Int(), true, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return raw.Uint(), true, true
	case reflect.Float32, reflect.Float64:
		return logScalar(key, raw.Float())
	}
	if t.Name() != "" || (t.Kind() != reflect.Slice && t.Kind() != reflect.Array) || t.Elem().Kind() == reflect.Uint8 {
		return nil, false, false
	}
	switch t.Elem().Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
	default:
		return nil, false, false
	}
	rv := reflect.ValueOf(value)
	items := make([]any, 0, min(rv.Len(), maxLogArrayItems))
	for i := 0; i < min(rv.Len(), maxLogArrayItems); i++ {
		e := rv.Index(i).Interface()
		v, keep, valid := logScalar(key, e)
		if !valid {
			return nil, false, false
		}
		if keep {
			items = append(items, v)
		}
	}
	return items, len(items) > 0, true
}
