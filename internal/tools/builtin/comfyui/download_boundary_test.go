package comfyui

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGenerateValidatesDownloadedImageBoundariesAndCleansUp(t *testing.T) {
	for _, test := range []struct {
		name, mime, wantError string
		size                  image.Point
	}{
		{name: "width boundary", size: image.Pt(8192, 1), mime: "image/png"},
		{name: "height boundary", size: image.Pt(1, 8192), mime: "image/png"},
		{name: "width exceeded", size: image.Pt(8193, 1), mime: "image/png", wantError: "invalid image"},
		{name: "height exceeded", size: image.Pt(1, 8193), mime: "image/png", wantError: "invalid image"},
		{name: "MIME mismatch", size: image.Pt(2, 3), mime: "image/jpeg", wantError: "MIME type does not match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, image.NewRGBA(image.Rectangle{Max: test.size})); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/prompt":
					_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
				case "/history/job":
					_, _ = w.Write([]byte(`{"job":{"outputs":{"9":{"images":[{"filename":"result.png","type":"output"}]}}}}`))
				case "/view":
					w.Header().Set("Content-Type", test.mime)
					_, _ = w.Write(encoded.Bytes())
				case "/free":
					var payload map[string]bool
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || !reflect.DeepEqual(payload, map[string]bool{"unload_models": true, "free_memory": true}) || r.Method != http.MethodPost {
						t.Errorf("cleanup method=%s payload=%v err=%v", r.Method, payload, err)
					}
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			got, degraded, err := client.Generate(context.Background(), map[string]node{}, "9", nil)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || !reflect.DeepEqual(got, GeneratedImage{}) {
					t.Fatalf("image=%+v err=%v, want no image and %q", got, err, test.wantError)
				}
			} else if err != nil || got.Size != test.size || got.MIMEType != "image/png" || got.Filename != "comfyui-generated.png" || !bytes.Equal(got.Data, encoded.Bytes()) {
				t.Fatalf("size=%v MIME=%q filename=%q err=%v", got.Size, got.MIMEType, got.Filename, err)
			}
			if degraded {
				t.Fatal("successful cleanup reported degraded")
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(paths, []string{"/prompt", "/history/job", "/view", "/free"}) {
				t.Fatalf("request sequence=%v", paths)
			}
		})
	}
}
