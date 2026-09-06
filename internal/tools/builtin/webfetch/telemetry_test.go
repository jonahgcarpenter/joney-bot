package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestFetchDegradedCompletionDoesNotLogURLOrContent(t *testing.T) {
	const canary = "private_page_prose"
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	handler := NewHandler(fakeFetcher{response: Response{URL: "https://example.com/" + canary, Content: canary, Title: canary, Source: "direct", ContentType: "text/plain", IsDegraded: true}}, log)
	ctx := requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "usr_1", Gateway: "discord", ExternalID: "external", Assurance: identity.AssuranceDiscordGateway})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req_fetch", OperationID: "op_parent"})
	result, err := handler(ctx, map[string]interface{}{"url": "https://example.com/" + canary})
	if err != nil || !result.IsDegraded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	found := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["event"] == "provider.web.fetch.complete" || r["event"] == "agent.tool.web.fetch.complete" {
			found[r["event"].(string)] = true
			if r["status"] != "degraded" {
				t.Errorf("completion=%+v", r)
			}
		}
	}
	if len(found) != 2 || strings.Contains(output.String(), canary) {
		t.Fatalf("logs=%s", output.String())
	}
}
