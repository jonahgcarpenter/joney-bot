package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/comfyui"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestComfyImageGovernanceUsesEffectiveSource(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []map[string]interface{}
		uploads []image.Point
		blocked int
	}{
		{
			name:    "distinct explicit sources and exact duplicate",
			args:    []map[string]interface{}{{"source_image_id": "current-1"}, {"source_image_id": "current-2"}, {"source_image_id": "current-1"}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(4, 5)}, blocked: 1,
		},
		{
			name:    "omitted source advances after each output",
			args:    []map[string]interface{}{{}, {}, {}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(6, 7), image.Pt(6, 7)},
		},
		{
			name:    "implicit and explicit same source are duplicates",
			args:    []map[string]interface{}{{}, {"source_image_id": "current-1"}, {"source_image_id": "current-2"}, {"source_image_id": "current-2"}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(4, 5)}, blocked: 2,
		},
		{
			name:    "invalid explicit selectors still reach validation",
			args:    []map[string]interface{}{{"source_image_id": "current-1"}, {"source_image_id": ""}, {"source_image_id": " current-1 "}, {"source_image_id": nil}, {"source_image_id": 7}, {"source_image_id": "unknown"}, {"source_image_id": "current-2"}},
			uploads: []image.Point{image.Pt(2, 3), image.Pt(4, 5)},
		},
	} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%t", test.name, batch), func(t *testing.T) {
				output := testInputImage(t, 6, 7)
				data, err := base64.StdEncoding.DecodeString(output.Data)
				if err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				var uploads []image.Point
				submissions := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch r.URL.Path {
					case "/upload/image":
						if err := r.ParseMultipartForm(1 << 20); err != nil {
							t.Error(err)
							http.Error(w, "bad upload", 400)
							return
						}
						defer r.MultipartForm.RemoveAll()
						file, _, err := r.FormFile("image")
						if err != nil {
							t.Error(err)
							http.Error(w, "missing image", 400)
							return
						}
						defer file.Close()
						decoded, _, err := image.Decode(file)
						if err != nil {
							t.Error(err)
							http.Error(w, "invalid image", 400)
							return
						}
						uploads = append(uploads, decoded.Bounds().Size())
						_ = json.NewEncoder(w).Encode(map[string]string{"name": comfyui.InputFilename, "subfolder": comfyui.InputSubfolder, "type": "input"})
					case "/prompt":
						submissions++
						_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
					case "/history/job":
						_, _ = w.Write([]byte(`{"job":{"outputs":{"30":{"images":[{"filename":"result.jpg","type":"output"}]}}}}`))
					case "/view":
						w.Header().Set("Content-Type", output.MimeType)
						_, _ = w.Write(data)
					case "/free":
						w.WriteHeader(http.StatusOK)
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				log := config.NewLogger(config.LevelError)
				reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "data", "tools"), log)
				if err != nil {
					t.Fatal(err)
				}
				if err := builtin.Register(reg, &config.Config{
					ComfyUIURL: server.URL, ComfyUIGenerationTimeout: time.Second,
					ComfyUITextToImageWorkflowPath:  filepath.Join("..", "..", "data", "workflows", "comfyui", "text-to-image-basic.json"),
					ComfyUIImageToImageWorkflowPath: filepath.Join("..", "..", "data", "workflows", "comfyui", "image-to-image-basic.json"),
				}, nil, nil, log); err != nil {
					t.Fatal(err)
				}
				chat := &fakeChatter{}
				var calls []llm.ToolCall
				for i, source := range test.args {
					args := map[string]interface{}{"prompt": "make it blue"}
					for key, value := range source {
						args[key] = value
					}
					calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Function: llm.ToolFunction{Name: toolnames.ComfyUIImageToImage, Arguments: args}})
				}
				original, _ := json.Marshal(calls)
				if batch {
					chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: calls}})
				} else {
					for _, call := range calls {
						chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{call}}})
					}
				}
				chat.responses = append(chat.responses, &llm.ChatResponse{Message: llm.ChatMessage{Role: "assistant", Content: "Finished."}})
				a, _ := newTestAgent(t, chat, nil, reg)
				response, err := processAgent(a, "source-governance", "discord", "session", "user-1", "User", "edit these images", []llm.InputImage{testInputImage(t, 2, 3), testInputImage(t, 4, 5)}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if response.ToolExecutionCount != len(test.args)-test.blocked || response.ToolBlockedCount != test.blocked {
					t.Fatalf("executions=%d blocked=%d", response.ToolExecutionCount, response.ToolBlockedCount)
				}
				mu.Lock()
				defer mu.Unlock()
				if submissions != len(test.uploads) || len(uploads) != len(test.uploads) {
					t.Fatalf("submissions=%d uploads=%v want=%v", submissions, uploads, test.uploads)
				}
				for i, want := range test.uploads {
					if uploads[i] != want {
						t.Fatalf("upload %d size=%v want=%v", i, uploads[i], want)
					}
				}
				after, _ := json.Marshal(calls)
				if !bytes.Equal(original, after) {
					t.Fatal("fingerprinting mutated model arguments")
				}
			})
		}
	}
}
