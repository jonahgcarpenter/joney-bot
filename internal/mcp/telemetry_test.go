package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestCatalogTelemetryAggregatesUnsupportedEntriesAndUsesCurrentCaller(t *testing.T) {
	const canary = "private_catalog_prose"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(request.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := `{}`
		switch request.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}`
		case "tools/list":
			result = `{"tools":[{"name":"` + canary + `!","inputSchema":{"type":"object"}},{"name":"` + canary + `","inputSchema":{"type":"array"}},{"name":"read_status","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"` + canary + `"}]}`
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result)
	}))
	defer server.Close()
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	cfg := ServerConfig{Name: "test", URL: server.URL}
	session, closeFn, err := connectStreamableHTTP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req_catalog", OperationID: "op_catalog"})
	specs, err := loadToolSpecs(ctx, cfg, session, log)
	if err != nil || len(specs) != 1 {
		t.Fatalf("specs=%v err=%v", specs, err)
	}
	for _, requestID := range []string{"req_first", "req_second"} {
		ctx = requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: requestID, OperationID: requestID + "_op"})
		if _, err := specs[0].Handler(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(output.String(), canary) {
		t.Fatalf("catalog leaked: %s", output.String())
	}
	warnings, starts := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		switch record["event"] {
		case "mcp.tool.skipped":
			warnings++
			if record["skipped_count"] != float64(2) || record["level"] != "warn" || record["request_id"] != "req_catalog" {
				t.Errorf("warning=%+v", record)
			}
		case "agent.tool.mcp.start":
			starts++
			if record["request_id"] != "req_first" && record["request_id"] != "req_second" {
				t.Errorf("cached caller=%+v", record)
			}
			if record["operation_id"] != record["request_id"].(string)+"_op" {
				t.Errorf("operation=%+v", record)
			}
		}
	}
	if warnings != 1 || starts != 2 {
		t.Fatalf("warnings=%d starts=%d logs=%s", warnings, starts, output.String())
	}
}

func TestManagerReportsConnectListAndCloseFailuresWithoutPrivateDetails(t *testing.T) {
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	store := testStore(t)
	manager := NewManagerFromStore(store, log)
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req_failure", OperationID: "op_parent"})
	if _, err := manager.ensureConnected(ctx, ServerConfig{Scope: ScopeGlobal, Name: "private_schema_label!", URL: "https://user:private_prose@example.com/private_prose"}); err == nil {
		t.Fatal("expected rejection")
	}
	manager.sessions["test"] = &server{close: func() error { return errors.New("private_prose") }}
	if err := manager.Close(); err == nil {
		t.Fatal("expected close error")
	}
	if err := store.db.SQL().Close(); err != nil {
		t.Fatal(err)
	}
	manager.ToolSpecs(ctx, "user")
	NewProvider(manager).DiscoveryTools(ctx, testPrincipal("user"))
	for _, event := range []string{"mcp.server.connect.complete", "mcp.server.connect_failed", "mcp.server.close_failed", "mcp.server_configs.list_failed"} {
		if !strings.Contains(output.String(), `"event":"`+event+`"`) {
			t.Errorf("missing %s: %s", event, output.String())
		}
	}
	if strings.Contains(output.String(), "private_prose") || strings.Contains(output.String(), "private_schema_label") {
		t.Fatalf("private data logged: %s", output.String())
	}
}
