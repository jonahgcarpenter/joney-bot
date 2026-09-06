package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
)

type fakeAuth struct{ admin bool }

type staticResolver map[string][]string

func (r staticResolver) LookupHost(context.Context, string) ([]string, error) {
	return r["example.com"], nil
}

func (a fakeAuth) IsAdminPrincipal(identity.Principal) (bool, error) {
	return a.admin, nil
}

type fullFakeAuth struct {
	fakeAuth
	principal identity.Principal
}

func (a *fullFakeAuth) IsAdminPrincipal(principal identity.Principal) (bool, error) {
	a.principal = principal
	return a.admin, nil
}

func TestGlobalCommandsRequireAdmin(t *testing.T) {
	h := New(nil, nil, fakeAuth{admin: false})
	result, err := h.Execute(context.Background(), commands.Request{Principal: identity.Principal{CanonicalUserID: "user_1"}, Args: []string{"global", "servers"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Text != "You are not allowed to use admin commands." {
		t.Fatalf("unexpected result: %q", result.Text)
	}
}

func TestGlobalCommandsUsePrincipalAuthorization(t *testing.T) {
	auth := &fullFakeAuth{fakeAuth: fakeAuth{admin: false}}
	principal := identity.Principal{CanonicalUserID: "stale", Gateway: "discord", ExternalID: "current", Assurance: identity.AssuranceDiscordGateway}
	result, err := New(nil, nil, auth).Execute(context.Background(), commands.Request{Principal: principal, Args: []string{"global", "servers"}})
	if err != nil || result.Text != "You are not allowed to use admin commands." || auth.principal != principal {
		t.Fatalf("result=%+v err=%v principal=%+v", result, err, auth.principal)
	}
}

func TestParseHeadersSupportsBearerAndHeader(t *testing.T) {
	headers, err := parseHeaders([]string{"auth-bearer=abc123", "header:X-Test=value"})
	if err != nil {
		t.Fatalf("parse headers: %v", err)
	}
	if headers["Authorization"] != "Bearer abc123" || headers["X-Test"] != "value" {
		t.Fatalf("unexpected headers: %+v", headers)
	}
}

func TestParseAddOptionsSeparatesHeadersAndDescription(t *testing.T) {
	headers, description, err := parseAddOptions([]string{"Manage", "GitHub", "repositories", "auth-bearer=abc123", "and", "issues", "header:X-Test=value"})
	if err != nil {
		t.Fatalf("parse add options: %v", err)
	}
	if description != "Manage GitHub repositories and issues" {
		t.Fatalf("description = %q", description)
	}
	if headers["Authorization"] != "Bearer abc123" || headers["X-Test"] != "value" {
		t.Fatalf("headers = %+v", headers)
	}
}

func TestParseAddOptionsRequiresDescription(t *testing.T) {
	if _, _, err := parseAddOptions([]string{"auth-bearer=abc123"}); err == nil {
		t.Fatal("expected missing description error")
	}
}

func TestGlobalAddPersistsAndListsDescription(t *testing.T) {
	ctx := context.Background()
	store, err := mcpmanager.NewStore(filepath.Join(t.TempDir(), "oswald.db"), "12345678901234567890123456789012", config.NewLogger(config.LevelError).Server("test"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	store.SetResolverForTest(staticResolver{"example.com": {"93.184.216.34"}})
	manager := mcpmanager.NewManagerFromStore(store, config.NewLogger(config.LevelError))
	h := New(store, manager, fakeAuth{admin: true})
	principal := identity.Principal{CanonicalUserID: "admin", Gateway: "discord", ExternalID: "admin", Assurance: identity.AssuranceDiscordGateway}

	result, err := h.Execute(ctx, commands.Request{Principal: principal, Args: []string{"global", "add", "github", "https://example.com/mcp", "Manage", "GitHub", "auth-bearer=secret", "repositories", "and", "issues."}})
	if err != nil || !strings.Contains(result.Text, `MCP server "github" saved`) {
		t.Fatalf("add result=%+v err=%v", result, err)
	}
	cfg, ok, err := store.Get(ctx, mcpmanager.ScopeGlobal, "", "github")
	if err != nil || !ok {
		t.Fatalf("get saved server ok=%t err=%v", ok, err)
	}
	if cfg.Description != "Manage GitHub repositories and issues." || cfg.Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("saved config = %+v", cfg)
	}

	result, err = h.Execute(ctx, commands.Request{Principal: principal, Args: []string{"global", "servers"}})
	if err != nil || !strings.Contains(result.Text, "description: Manage GitHub repositories and issues.") {
		t.Fatalf("list result=%+v err=%v", result, err)
	}
}

func TestMCPCommandDefinition(t *testing.T) {
	h := New((*mcpmanager.Store)(nil), (*mcpmanager.Manager)(nil), fakeAuth{})
	if h.Definition().Name != "mcp" {
		t.Fatalf("unexpected definition: %+v", h.Definition())
	}
}
