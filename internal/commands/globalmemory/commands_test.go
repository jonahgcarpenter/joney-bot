package globalmemory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
)

type fakeAuthorizer struct {
	admins map[string]bool
}

func (a fakeAuthorizer) IsAdminPrincipal(principal identity.Principal) (bool, error) {
	return a.admins[principal.CanonicalUserID], nil
}

func TestCommandsRequireAdmin(t *testing.T) {
	service, _ := newCommandService(t)
	result := executeCommand(t, service, "user", "/global-memory add Oswald uses Go.")
	if result != "You are not allowed to use admin commands." {
		t.Fatalf("non-admin result = %q", result)
	}
	result = executeCommand(t, service, "admin", "/global-memory list")
	if result != "Global memory - page 1\nNo global memories found." {
		t.Fatalf("admin result = %q", result)
	}
}

func TestCommandsAddListForgetAndDuplicateResponse(t *testing.T) {
	service, _ := newCommandService(t)

	if got := executeCommand(t, service, "admin", "/global-memory add Oswald uses Go."); got != "Added global memory 1." {
		t.Fatalf("add result = %q", got)
	}
	if got := executeCommand(t, service, "admin", "/global-memory add   oswald USES go."); got != "Global memory already exists as ID 1." {
		t.Fatalf("duplicate result = %q", got)
	}
	if got := executeCommand(t, service, "admin", "/global-memory add Oswald uses SQLite."); got != "Added global memory 2." {
		t.Fatalf("second add result = %q", got)
	}

	wantList := "Global memory - page 1\n1 | Oswald uses Go.\n2 | Oswald uses SQLite."
	if got := executeCommand(t, service, "admin", "/global-memory list"); got != wantList {
		t.Fatalf("list result = %q, want %q", got, wantList)
	}
	if got := executeCommand(t, service, "admin", "/global-memory forget 1"); got != "Forgot global memory 1." {
		t.Fatalf("forget result = %q", got)
	}
	if got := executeCommand(t, service, "admin", "/global-memory forget 1"); got != "Global memory 1 was not found." {
		t.Fatalf("unknown forget result = %q", got)
	}
	if got := executeCommand(t, service, "admin", "/global-memory list"); got != "Global memory - page 1\n2 | Oswald uses SQLite." {
		t.Fatalf("post-forget list result = %q", got)
	}
}

func TestCommandsListPagination(t *testing.T) {
	service, store := newCommandService(t)
	for i := 1; i <= global.ListPageSize+1; i++ {
		if _, err := store.Add(context.Background(), fmt.Sprintf("Fact %02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	first := executeCommand(t, service, "admin", "/global-memory list 1")
	if !strings.HasPrefix(first, "Global memory - page 1\n1 | Fact 01") || !strings.Contains(first, "25 | Fact 25") || !strings.HasSuffix(first, "More results: /global-memory list 2") {
		t.Fatalf("unexpected first page: %q", first)
	}
	if got := executeCommand(t, service, "admin", "/global-memory list 2"); got != "Global memory - page 2\n26 | Fact 26" {
		t.Fatalf("second page = %q", got)
	}
}

func TestCommandsRejectMalformedIDsAndPages(t *testing.T) {
	service, _ := newCommandService(t)
	for _, value := range []string{"0", "-1", "+1", "1.0", "abc", "999999999999999999999999"} {
		t.Run("page_"+value, func(t *testing.T) {
			got := executeCommand(t, service, "admin", "/global-memory list "+value)
			if got != "Global memory page must be a positive decimal integer." {
				t.Fatalf("result = %q", got)
			}
		})
		t.Run("id_"+value, func(t *testing.T) {
			got := executeCommand(t, service, "admin", "/global-memory forget "+value)
			if got != "Global memory ID must be a positive decimal integer." {
				t.Fatalf("result = %q", got)
			}
		})
	}
}

func newCommandService(t *testing.T) (*commands.Service, *global.Store) {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	store, err := global.NewStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "", log)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := commands.NewServiceWithCommands(commands.Command{
		Handler:    New(store, log),
		Middleware: []commands.Middleware{commands.RequireAdmin(fakeAuthorizer{admins: map[string]bool{"admin": true}})},
	})
	if err != nil {
		t.Fatalf("NewServiceWithCommands: %v", err)
	}
	return service, store
}

func executeCommand(t *testing.T, service *commands.Service, userID, raw string) string {
	t.Helper()
	result, err := service.Execute(context.Background(), commands.Request{
		RequestID: "request-1",
		Principal: identity.Principal{
			CanonicalUserID: userID,
			Gateway:         "discord",
			ExternalID:      "external-" + userID,
			Assurance:       identity.AssuranceDiscordGateway,
		},
		Raw: raw,
	})
	if err != nil {
		t.Fatalf("Execute(%q): %v", raw, err)
	}
	return result.Text
}
