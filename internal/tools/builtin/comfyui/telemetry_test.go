package comfyui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestGenerationStagesAndCleanupWarningExcludeProviderProse(t *testing.T) {
	const canary = "private_generation_prose"
	imageData := testPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			_, _ = io.WriteString(w, `{"prompt_id":"`+canary+`","unknown":"`+canary+`"}`)
		case "/history/" + canary:
			_, _ = io.WriteString(w, `{"`+canary+`":{"outputs":{"9":{"images":[{"filename":"`+canary+`.png","type":"output"}]}}}}`)
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imageData)
		case "/free":
			http.Error(w, canary, http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := LoadWorkflow(workflowPath("text-to-image-basic.json"), TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	ctx := requestctx.WithMetadata(authenticatedContext(), requestctx.Metadata{RequestID: "req_image", OperationID: "op_parent"})
	result, err := NewHandler(TextToImage, workflow, client, log)(ctx, map[string]interface{}{"prompt": canary, "negative_prompt": canary})
	if err != nil || !result.IsDegraded {
		t.Fatalf("err=%v degraded=%t", err, result.IsDegraded)
	}
	stages := map[string]int{}
	cleanupWarning := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["event"] == "provider.comfyui.stage.complete" {
			stages[r["phase"].(string)]++
			if r["level"] != "info" || r["duration_ms"] == nil || r["parent_operation_id"] != "op_parent" {
				t.Errorf("stage=%+v", r)
			}
			if r["phase"] == "cleanup" && (r["http_status"] != float64(503) || r["error_code"] != "http_server_error") {
				t.Errorf("cleanup=%+v", r)
			}
		}
		if r["event"] == "agent.tool.comfyui.cleanup_failed" && r["level"] == "warn" {
			cleanupWarning = true
		}
	}
	for _, phase := range []string{"permit", "submit", "poll", "download", "cleanup"} {
		if stages[phase] != 1 {
			t.Errorf("stages=%v", stages)
		}
	}
	if !cleanupWarning || strings.Contains(output.String(), canary) {
		t.Fatalf("logs=%s", output.String())
	}
}
