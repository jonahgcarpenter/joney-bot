package globalmemory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func TestSearchHandlerRequiresAuthenticationAndValidatesArguments(t *testing.T) {
	store := newTestStore(t)
	handler := NewSearchHandler(store, config.NewLogger(config.LevelError))
	if _, err := handler(context.Background(), map[string]interface{}{"query": "fact"}); err == nil || !strings.Contains(err.Error(), "authenticated") {
		t.Fatalf("unauthenticated error = %v", err)
	}
	ctx := authenticatedContext("user-1")
	for _, test := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{name: "empty query", args: map[string]interface{}{"query": " \t"}, want: "1..500"},
		{name: "long query", args: map[string]interface{}{"query": strings.Repeat("x", 501)}, want: "1..500"},
		{name: "zero limit", args: map[string]interface{}{"query": "fact", "limit": float64(0)}, want: "between 1"},
		{name: "large limit", args: map[string]interface{}{"query": "fact", "limit": float64(global.MaxSearchLimit + 1)}, want: "between 1"},
		{name: "fractional limit", args: map[string]interface{}{"query": "fact", "limit": 1.5}, want: "between 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := handler(ctx, test.args); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSearchHandlerRendersDirectJSONSharedAcrossTenants(t *testing.T) {
	store := newTestStore(t)
	added, err := store.Add(context.Background(), `Oswald supports "policy" references and an <admin> command namespace.`)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewSearchHandler(store, config.NewLogger(config.LevelError))
	args := map[string]interface{}{"query": "oswald supports policy references admin command namespace", "limit": float64(1)}
	first, err := handler(authenticatedContext("tenant-a"), args)
	if err != nil {
		t.Fatal(err)
	}
	second, err := handler(authenticatedContext("tenant-b"), args)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("tenant results differ:\nA: %s\nB: %s", first.Content, second.Content)
	}
	if first.Outcome != governance.OutcomeProductive {
		t.Fatalf("search outcome = %q, want %q", first.Outcome, governance.OutcomeProductive)
	}
	if strings.Contains(first.Content, "Global Memory Reference") || strings.Contains(first.Content, "UNTRUSTED") {
		t.Fatalf("search result contains legacy wrapper: %q", first.Content)
	}
	var record struct {
		ID      int64    `json:"id"`
		Memory  string   `json:"memory"`
		Score   float64  `json:"score"`
		Sources []string `json:"sources"`
	}
	if err := json.Unmarshal([]byte(first.Content), &record); err != nil {
		t.Fatalf("result is not direct JSON: %v (%q)", err, first.Content)
	}
	if record.ID != added.Memory.ID || record.Memory != added.Memory.Text || record.Score != 1 || strings.Join(record.Sources, ",") != "lexical" {
		t.Fatalf("unexpected rendered record: %+v", record)
	}
	if utf8.RuneCountInString(first.Content) > searchOutputLimit {
		t.Fatalf("render exceeds %d runes", searchOutputLimit)
	}
}

func TestRenderSearchEmptyResultIsDirect(t *testing.T) {
	if got := renderSearch(nil, searchOutputLimit); got != "No relevant global memories found." {
		t.Fatalf("empty result = %q", got)
	}
}

func authenticatedContext(userID string) context.Context {
	ctx := requestctx.WithPrincipal(context.Background(), identity.Principal{
		CanonicalUserID: userID,
		Gateway:         "homeassistant",
		ExternalID:      "external-" + userID,
		Assurance:       identity.AssuranceHomeAssistantToken,
	})
	return requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "request-1", SessionID: "session-1", Model: "test-model"})
}

func newTestStore(t *testing.T) *global.Store {
	t.Helper()
	store, err := global.NewStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
