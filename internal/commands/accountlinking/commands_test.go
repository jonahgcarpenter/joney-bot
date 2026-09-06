package accountlinking

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func TestCommandHandlerConnectAndDisconnect(t *testing.T) {
	links := newTestService(t)
	userID, err := links.EnsureAccount(context.Background(), "discord", "123", "Alice")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	otherID, err := links.EnsureAccount(context.Background(), "homeassistant", "alice-local", "Alice Local")
	if err != nil {
		t.Fatalf("ensure other account: %v", err)
	}
	var registrations []commands.Command
	for _, handler := range New(links) {
		registrations = append(registrations, commands.Command{Handler: handler})
	}
	service, err := commands.NewServiceWithCommands(registrations...)
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}

	initiator := identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	confirmer := identity.Principal{CanonicalUserID: otherID, Gateway: "homeassistant", ExternalID: "alice-local", Assurance: identity.AssuranceHomeAssistantToken}
	response, err := executeAccountCommand(service, initiator, "/connect")
	if err != nil {
		t.Fatalf("start connect err=%v", err)
	}
	code := regexp.MustCompile(`OSW-(?:[A-Z0-9]{4}-){4}[A-Z0-9]{4}`).FindString(response)
	if code == "" {
		t.Fatalf("unexpected connect menu: %q", response)
	}

	commandTargets, err := (&handler{links: links}).ResolveFenceTargets(context.Background(), commands.Request{
		Name: "connect", Principal: confirmer, Args: []string{code},
	})
	commandTargetSet := make(map[string]bool, len(commandTargets))
	for _, target := range commandTargets {
		commandTargetSet[target] = true
	}
	if err != nil || !commandTargetSet[userID] || !commandTargetSet[otherID] {
		t.Fatalf("resolve command fence targets=%v err=%v", commandTargets, err)
	}

	response, err = executeAccountCommand(service, confirmer, "/connect "+code)
	if err != nil {
		t.Fatalf("connect err=%v", err)
	}
	if !strings.Contains(response, "Accounts connected successfully") {
		t.Fatalf("unexpected connect response: %q", response)
	}

	confirmer.CanonicalUserID = userID
	response, err = executeAccountCommand(service, confirmer, "/disconnect")
	if err != nil {
		t.Fatalf("start disconnect err=%v", err)
	}
	if !strings.Contains(response, "Disconnect an account.") {
		t.Fatalf("unexpected disconnect menu: %q", response)
	}

	result, err := service.Execute(context.Background(), commands.Request{Principal: confirmer, Raw: "/disconnect 2", RequestID: "req_disconnect"})
	if err != nil {
		t.Fatalf("disconnect err=%v", err)
	}
	if !strings.Contains(result.Text, "Disconnected Home Assistant: alice-local.") {
		t.Fatalf("unexpected disconnect response: %q", result.Text)
	}
	if result.Invalidation == nil || !result.Invalidation.CloseConnections || len(result.Invalidation.ExternalIdentities) != 1 || result.Invalidation.ExternalIdentities[0] != "homeassistant:alice-local" {
		t.Fatalf("unexpected disconnect invalidation: %+v", result.Invalidation)
	}
	definition, ok := service.Definition("disconnect")
	if !ok || !definition.UserExclusive {
		t.Fatalf("disconnect definition is not user-exclusive: %+v", definition)
	}
}

func TestConnectCommandRequiresAuthenticatedPrincipal(t *testing.T) {
	links := newTestService(t)
	userID, _ := links.EnsureAccount(context.Background(), "homeassistant", "local", "Local")
	var registrations []commands.Command
	for _, handler := range New(links) {
		registrations = append(registrations, commands.Command{Handler: handler})
	}
	service, err := commands.NewServiceWithCommands(registrations...)
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	selfAsserted := identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: "local", Assurance: identity.AssuranceSelfAsserted}
	result, err := service.Execute(context.Background(), commands.Request{Principal: selfAsserted, Raw: "/connect"})
	if err == nil || result.Text != "" {
		t.Fatalf("self-asserted result=%q err=%v", result.Text, err)
	}
	authenticated := identity.Principal{CanonicalUserID: userID, Gateway: "homeassistant", ExternalID: "local", Assurance: identity.AssuranceHomeAssistantToken}
	result, err = service.Execute(context.Background(), commands.Request{Principal: authenticated, Raw: "/connect"})
	if err != nil || !strings.Contains(result.Text, "On the other account, send:") || !strings.Contains(result.Text, "/connect OSW-") {
		t.Fatalf("authenticated result=%q err=%v", result.Text, err)
	}
}

func executeAccountCommand(service *commands.Service, principal identity.Principal, raw string) (string, error) {
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Raw: raw, RequestID: "req_test"})
	return result.Text, err
}

func newTestService(t *testing.T) *accounts.Service {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	dbPath := filepath.Join(t.TempDir(), "oswald.db")
	memories := memorytest.NewStore(t, dbPath, log)
	t.Cleanup(func() { memories.Close() })
	links := accounts.NewService(dbPath, memories, nil, log)
	t.Cleanup(func() { links.Close() })
	return links
}
