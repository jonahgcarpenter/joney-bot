package homeassistant

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestRejectedFrameHasServerCorrelationAndNoPayload(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "gateway-log")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stderr
	os.Stderr = file
	log := config.NewLogger(config.LevelDebug)
	os.Stderr = old
	links, _ := testLinks(t)
	g, err := New("8000", testToken, links, gatewayDependencies(log), log)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		g.handleConnection(w, r, nil)
	}))
	defer server.Close()
	conn, _, err := gorilla.DefaultDialer.Dial(strings.Replace(server.URL, "http://", "ws://", 1), http.Header{"Authorization": []string{"Bearer " + testToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	readProtocolMessage(t, conn)
	if err := conn.WriteMessage(gorilla.TextMessage, []byte(`{"private-payload":"synthetic-secret"}`)); err != nil {
		t.Fatal(err)
	}
	if result := readProtocolMessage(t, conn); result.Code != "invalid_request" {
		t.Fatalf("result=%+v", result)
	}
	<-done
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic-secret") || strings.Contains(string(data), "private-payload") {
		t.Fatal("payload leaked")
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "gateway.request.rejected" {
			found = true
			if event["request_id"] == nil || event["reason_code"] != "invalid_request" || event["user_id"] != nil {
				t.Fatalf("rejection=%+v", event)
			}
		}
	}
	if !found {
		t.Fatal("missing correlated rejection")
	}
}
