package usermemory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestTranscriptHandlerLogsExactlyOneSafeMeasurement(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, group := range []bool{false, true} {
			scope := "dm"
			if group {
				scope = "group"
			}
			for _, test := range []struct {
				name, status, outcome string
				count                 float64
				wantError             bool
			}{
				{"success", "ok", "found", 1, false},
				{"empty", "ok", "empty", 0, false},
				{"unavailable", "error", "error", 0, true},
				{"rejected", "rejected", "rejected", 0, true},
				{"canceled", "ok", "canceled", 0, true},
			} {
				t.Run(level.String()+"/"+scope+"/"+test.name, func(t *testing.T) {
					store, db := newHandlerTestStore(t, nil)
					seedHandlerUser(t, db, "caller")
					turn := appendHandlerGroupTurn(t, store, "caller", "sessioncanary", "discord", "chatcanary", "publicquerycanary", true)
					rebuildHandlerIndexes(t, store, false)
					meta := requestctx.Metadata{
						RequestID: "req_transcript", OperationID: "op_transcript", ParentOperationID: "op_parent",
						Workload: "foreground", Model: "test-model", SessionID: turn.SessionID, SessionGeneration: turn.Generation,
						CurrentUserText: "currentprivatecanary", PublicUserText: "currentpubliccanary",
					}
					if group {
						meta.GroupGateway, meta.GroupChatID = "discord", "chatcanary"
					}
					if test.name == "rejected" {
						meta.SessionGeneration = 0
					}
					ctx := requestctx.WithPrincipal(context.Background(), transcriptHandlerPrincipal("discord"))
					ctx = requestctx.WithMetadata(ctx, meta)
					if test.name == "canceled" {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					}
					if test.name == "unavailable" {
						live, err := store.LiveIndexRevision(context.Background(), memory.IndexKindTranscriptFTS)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := db.Exec(`DROP TABLE ` + live.TableName); err != nil {
							t.Fatal(err)
						}
					}
					query := "internalcanary"
					if group {
						query = "publicquerycanary"
					}
					if test.name == "empty" {
						query = "absentquerycanary"
					}
					var output bytes.Buffer
					log := config.NewLogger(level)
					log.SetOutput(&output)
					_, err := NewTranscriptSearchHandler(store, log)(ctx, map[string]interface{}{"query": query})
					if (err != nil) != test.wantError {
						t.Errorf("error=%v, wantError=%v", err, test.wantError)
					}
					if test.name == "unavailable" && !errors.Is(err, memory.ErrTranscriptSearchUnavailable) {
						t.Errorf("error=%v, want unavailable", err)
					}
					if test.name == "canceled" && !errors.Is(err, context.Canceled) {
						t.Errorf("error=%v, want cancellation preserved", err)
					}
					count := 0
					for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
						var record map[string]interface{}
						if err := json.Unmarshal([]byte(line), &record); err != nil {
							t.Fatalf("invalid log JSON: %v", err)
						}
						if record["event"] != "agent.tool.transcript.searched" {
							t.Errorf("unexpected handler log event: %v", record["event"])
							continue
						}
						count++
						for key, want := range map[string]interface{}{
							"level": "info", "record_kind": "measurement", "log_type": "agent", "component": "agent.tool.memory",
							"status": test.status, "outcome": test.outcome, "is_group": group, "returned_count": test.count,
							"request_id": "req_transcript", "operation_id": "op_transcript", "parent_operation_id": "op_parent",
							"user_id": "caller", "gateway": "discord", "model": "test-model", "workload": "foreground",
							"tool_name": "session_transcript_search",
						} {
							if record[key] != want {
								t.Errorf("%s=%#v (%T), want %#v (%T)", key, record[key], record[key], want, want)
							}
						}
						if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
							t.Errorf("duration_ms=%#v, want nonnegative JSON number", record["duration_ms"])
						}
						for _, key := range []string{"query", "chat_id", "group_chat_id", "session_id", "current_user_text", "public_user_text", "external_id", "error"} {
							if _, ok := record[key]; ok {
								t.Errorf("private key %q present", key)
							}
						}
					}
					if count != 1 {
						t.Errorf("searched event count=%d, want exactly one", count)
					}
					for _, canary := range []string{"publicquerycanary", "absentquerycanary", "internalcanary", "intermediatecanary", "argumentcanary", "toolresultcanary", "publicanswercanary", "sessioncanary", "chatcanary", "externalcanary", "currentprivatecanary", "currentpubliccanary"} {
						if strings.Contains(output.String(), canary) {
							t.Errorf("logs leaked %q", canary)
						}
					}
				})
			}
		}
	}
}
