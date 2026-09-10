package comfyui

import (
	"bytes"
	"encoding/base64"
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

func TestImageStrengthStageTelemetry(t *testing.T) {
	const canary = "private_strength_prompt"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/image":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": InputFilename, "subfolder": InputSubfolder, "type": "input"})
		case "/prompt":
			http.Error(w, canary, http.StatusBadRequest)
		case "/free":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, explicit := range []bool{false, true} {
			client, err := NewClient(server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			workflow, err := LoadWorkflow(workflowPath("image-to-image-basic.json"), ImageToImage)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			log := config.NewLogger(level)
			log.SetOutput(&output)
			ctx := requestctx.WithMetadata(authenticatedContext(), requestctx.Metadata{RequestID: "req_strength", OperationID: "op_strength"})
			ctx = requestctx.WithInputImages(ctx, []requestctx.InputImage{{Data: base64.StdEncoding.EncodeToString(testPNG(t))}})
			args := map[string]interface{}{"prompt": canary}
			want := workflow.nodes["26"].Inputs["denoise"]
			if explicit {
				args["strength"] = 0.6
				want = 0.6
			}
			if _, err := NewHandler(ImageToImage, workflow, client, log)(ctx, args); err == nil {
				t.Fatal("expected submission failure")
			}
			submits := 0
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var record map[string]interface{}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] == "provider.comfyui.stage.complete" && record["phase"] == "submit" {
					submits++
					if record["strength"] != want || record["level"] != "info" || record["status"] != "error" || record["request_id"] != "req_strength" || record["parent_operation_id"] != "op_strength" {
						t.Fatalf("unexpected strength measurement: %+v", record)
					}
				}
			}
			if submits != 1 || strings.Contains(output.String(), canary) {
				t.Fatalf("unsafe or missing measurement: %s", output.String())
			}
		}
	}
}
