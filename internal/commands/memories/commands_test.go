package memories

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/testutil"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestMemoriesListAndForget(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	memory := testutil.NewMemoryStore(t, path, log)
	defer memory.Close() // nolint:errcheck
	accounts := accountlinking.NewService(path, memory, nil, log)
	defer accounts.Close() // nolint:errcheck
	userID, err := accounts.EnsureAccount("homeassistant", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := accounts.EnsureAccount("homeassistant", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	var first usermemory.MemoryEntry
	for i := 0; i < 30; i++ {
		entry, err := testutil.PublishMemory(ctx, memory, userID, testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Category: "notes", Statement: fmt.Sprintf("memory %02d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = entry
		}
	}
	if _, err := testutil.PublishMemory(ctx, memory, otherID, testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Category: "identity", Statement: "other tenant secret"}); err != nil {
		t.Fatal(err)
	}
	h := handler{accounts: accounts, memory: memory}
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: "actor", Assurance: identity.AssuranceHomeAssistantToken}
	request := commands.Request{RequestID: "list", Principal: principal, Args: []string{"list"}}
	for _, args := range [][]string{nil, {"list", "extra"}, {"forget"}, {"unknown"}} {
		invalid := request
		invalid.Args = args
		result, err := h.Execute(ctx, invalid)
		if err != nil || result.Text != commands.UsageText(h.Definition()) {
			t.Fatalf("args=%v result=%+v err=%v", args, result, err)
		}
	}
	invalidID := request
	invalidID.Args = []string{"forget", "01"}
	if result, err := h.Execute(ctx, invalidID); err != nil || !strings.Contains(result.Text, "exact positive decimal") {
		t.Fatalf("invalid ID result=%+v err=%v", result, err)
	}
	result, err := h.Execute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	attachments := result.Attachments
	if len(attachments) != 1 || attachments[0].MIMEType != "text/plain; charset=utf-8" {
		t.Fatalf("attachments=%+v", attachments)
	}
	listed := string(attachments[0].Data)
	if strings.Count(listed, "ID: ") != 30 || !strings.Contains(listed, "Memory: memory 29") || strings.Contains(listed, "other tenant secret") {
		t.Fatalf("unexpected memory list:\n%s", listed)
	}

	request.RequestID = "forget-one"
	request.Args = []string{"forget", fmt.Sprint(first.ID)}
	result, err = h.Execute(ctx, request)
	if err != nil || result.Invalidation != nil || result.Text != fmt.Sprintf("Memory %d was permanently deleted.", first.ID) {
		t.Fatalf("forget result=%+v err=%v", result, err)
	}

	request.RequestID = "forget-all"
	request.Args = []string{"forget", "all"}
	result, err = h.Execute(ctx, request)
	if err != nil || result.Invalidation == nil || !strings.Contains(result.Text, "All stored information") {
		t.Fatalf("forget all result=%+v err=%v", result, err)
	}
	remaining, err := memory.ListActiveMemories(ctx, userID, time.Now().UTC())
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining=%+v err=%v", remaining, err)
	}
	other, err := memory.ListActiveMemories(ctx, otherID, time.Now().UTC())
	if err != nil || len(other) != 1 {
		t.Fatalf("other tenant memories=%+v err=%v", other, err)
	}
}

func TestMemoriesUsageValidation(t *testing.T) {
	definition := (handler{}).Definition()
	if definition.Name != "memories" || definition.Usage != usage || !definition.UserExclusive {
		t.Fatalf("definition=%+v", definition)
	}
	if result, err := (handler{}).Execute(context.Background(), commands.Request{}); err == nil || result.Text != "" {
		t.Fatalf("nil service result=%+v err=%v", result, err)
	}
}

func TestListResultAlwaysUsesAttachment(t *testing.T) {
	result, err := listResult(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Attachments) != 1 || string(result.Attachments[0].Data) != "No active memories.\n" || result.Text == "" {
		t.Fatalf("result=%+v", result)
	}
}

func TestListResultRejectsInvalidUTF8(t *testing.T) {
	_, err := listResult([]usermemory.ListedMemory{{ID: 1, Category: "notes", Statement: string([]byte{0xff})}})
	if err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("err=%v", err)
	}
}
