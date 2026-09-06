package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestLoadToolSpecsPaginatesSDKAndDiscardsCanceledPartialCatalog(t *testing.T) {
	for _, cancelLater := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_later=%t", cancelLater), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var mu sync.Mutex
			var cursors []string
			laterPage := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Cursor string `json:"cursor"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(request.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				result := `{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}`
				if request.Method == "tools/list" {
					mu.Lock()
					cursors = append(cursors, request.Params.Cursor)
					page := len(cursors)
					mu.Unlock()
					switch page {
					case 1:
						result = `{"tools":[{"name":"first","inputSchema":{"type":"object"}}],"nextCursor":"opaque-next-page"}`
					case 2:
						if cancelLater {
							close(laterPage)
							select {
							case <-r.Context().Done():
							case <-release:
							}
							return
						}
						result = `{"tools":[{"name":"second","inputSchema":{"type":"object"}}]}`
					default:
						t.Errorf("unexpected catalog page %d", page)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				} else if request.Method != "initialize" {
					t.Errorf("unexpected method %q", request.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, result)
			}))
			defer server.Close()
			defer close(release)
			cfg := ServerConfig{Name: "test", URL: server.URL}
			session, closeSession, err := connectStreamableHTTP(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer closeSession()
			type catalogResult struct {
				specs []ToolSpec
				err   error
			}
			done := make(chan catalogResult, 1)
			go func() {
				specs, err := loadToolSpecs(ctx, cfg, session, config.NewLogger(config.LevelError))
				done <- catalogResult{specs, err}
			}()
			if cancelLater {
				select {
				case <-laterPage:
					cancel()
				case <-ctx.Done():
					t.Fatal("second catalog page was not requested")
				}
			}
			select {
			case got := <-done:
				if cancelLater {
					if !errors.Is(got.err, context.Canceled) || got.specs != nil {
						t.Fatalf("canceled catalog specs=%v err=%v", got.specs, got.err)
					}
				} else {
					if got.err != nil || len(got.specs) != 2 {
						t.Fatalf("catalog specs=%v err=%v", got.specs, got.err)
					}
					if got.specs[0].Name != "test.first" || got.specs[1].Name != "test.second" {
						t.Fatalf("catalog names=%q, %q", got.specs[0].Name, got.specs[1].Name)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("catalog call did not return")
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(cursors, []string{"", "opaque-next-page"}) {
				t.Fatalf("tools/list cursors=%q", cursors)
			}
		})
	}
}
