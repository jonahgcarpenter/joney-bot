package usermanagement

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func TestCommandHandlerRequiresAdmin(t *testing.T) {
	links := newTestService(t)
	userID, err := links.EnsureAccount("discord", "100", "User")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	service := newAdminCommandService(t, links)

	for _, input := range []string{"/users", "/user " + userID, "/deleteuser " + userID} {
		result, err := service.Execute(context.Background(), commands.Request{Principal: commandPrincipal(t, links, userID), Raw: input})
		if err != nil {
			t.Fatalf("execute %s err=%v", input, err)
		}
		if result.Text != "You are not allowed to use admin commands." {
			t.Fatalf("unexpected response for %s: %q", input, result.Text)
		}
	}
}

func TestDeleteFenceReResolvesPrincipal(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "901", "Admin")
	if err != nil {
		t.Fatal(err)
	}
	admin := commandPrincipal(t, links, adminID)
	if _, claimed, err := links.ClaimBootstrapAdmin(admin); err != nil || !claimed {
		t.Fatalf("claim admin: claimed=%t err=%v", claimed, err)
	}
	userID, err := links.EnsureAccount("discord", "902", "User")
	if err != nil {
		t.Fatal(err)
	}
	stale := commandPrincipal(t, links, userID)
	stale.CanonicalUserID = adminID
	service := newAdminCommandService(t, links)
	if targets, err := service.ResolveFenceTargets(context.Background(), commands.Request{Principal: stale, Raw: "/deleteuser " + userID}); err == nil || len(targets) != 0 {
		t.Fatalf("stale admin ID authorized: targets=%v err=%v", targets, err)
	}
	admin.CanonicalUserID = userID
	if targets, err := service.ResolveFenceTargets(context.Background(), commands.Request{Principal: admin, Raw: "/deleteuser " + userID}); err != nil || len(targets) != 1 || targets[0] != userID {
		t.Fatalf("current admin owner denied: targets=%v err=%v", targets, err)
	}
}

func TestCommandHandlerUsersAdminBanAndUnban(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "200", "Admin")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	targetID, err := links.EnsureAccount("discord", "300", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}
	if _, claimed, err := links.ClaimBootstrapAdmin(commandPrincipal(t, links, adminID)); err != nil || !claimed {
		t.Fatalf("claim bootstrap admin: claimed=%t err=%v", claimed, err)
	}
	service := newAdminCommandService(t, links)

	response, err := executeCommand(t, service, links, adminID, "/users")
	if err != nil {
		t.Fatalf("users err=%v", err)
	}
	if !strings.Contains(response, targetID) || !strings.Contains(response, "You are speaking with Target.") {
		t.Fatalf("unexpected users response: %q", response)
	}

	response, err = executeCommand(t, service, links, adminID, "/user "+targetID)
	if err != nil {
		t.Fatalf("user err=%v", err)
	}
	if !strings.Contains(response, targetID) || !strings.Contains(response, "You are speaking with Target.") || !strings.Contains(response, "discord:300 (Target)") {
		t.Fatalf("unexpected user response: %q", response)
	}

	response, err = executeCommand(t, service, links, adminID, "/user")
	if err != nil || response != "Show one canonical user.\nUse: /user <canonical_id>" {
		t.Fatalf("usage response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/user usr_missing")
	if err != nil || response != "User usr_missing not found." {
		t.Fatalf("missing response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/admin "+targetID)
	if err != nil || !strings.Contains(response, "Marked "+targetID+" as admin.") {
		t.Fatalf("admin response=%q err=%v", response, err)
	}
	isAdmin, err := links.IsAdmin(targetID)
	if err != nil || !isAdmin {
		t.Fatalf("expected target admin, got %v err=%v", isAdmin, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/unadmin "+adminID)
	if err != nil || !strings.Contains(response, "cannot remove admin from yourself") {
		t.Fatalf("self unadmin response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/ban "+targetID+" spam")
	if err != nil || !strings.Contains(response, "Banned "+targetID+".") {
		t.Fatalf("ban response=%q err=%v", response, err)
	}
	isBanned, _, err := links.BanStatus(targetID)
	if err != nil || !isBanned {
		t.Fatalf("expected target banned, got %v err=%v", isBanned, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/unban "+targetID)
	if err != nil || !strings.Contains(response, "Unbanned "+targetID+".") {
		t.Fatalf("unban response=%q err=%v", response, err)
	}
	isBanned, _, err = links.BanStatus(targetID)
	if err != nil || isBanned {
		t.Fatalf("expected target unbanned, got %v err=%v", isBanned, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/deleteuser")
	if err != nil || response != "Delete a canonical user.\nUse: /deleteuser <canonical_id>" {
		t.Fatalf("delete usage response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/deleteuser "+adminID)
	if err != nil || !strings.Contains(response, "cannot delete yourself") {
		t.Fatalf("self delete response=%q err=%v", response, err)
	}

	response, err = executeCommand(t, service, links, adminID, "/deleteuser "+targetID)
	if err != nil || !strings.Contains(response, "Deleted "+targetID+".") {
		t.Fatalf("delete response=%q err=%v", response, err)
	}
	if _, ok, err := links.User(targetID); err != nil || ok {
		t.Fatalf("expected target deleted, ok=%v err=%v", ok, err)
	}
}

func newAdminCommandService(t *testing.T, links *accounts.Service) *commands.Service {
	t.Helper()
	registrations := make([]commands.Command, 0)
	for _, handler := range New(links) {
		registrations = append(registrations, commands.Command{Handler: handler, Middleware: []commands.Middleware{commands.RequireAdmin(links)}})
	}
	service, err := commands.NewServiceWithCommands(registrations...)
	if err != nil {
		t.Fatalf("new command service: %v", err)
	}
	return service
}

func executeCommand(t *testing.T, service *commands.Service, links *accounts.Service, userID, raw string) (string, error) {
	t.Helper()
	result, err := service.Execute(context.Background(), commands.Request{Principal: commandPrincipal(t, links, userID), Raw: raw})
	return result.Text, err
}

func commandPrincipal(t *testing.T, links *accounts.Service, userID string) identity.Principal {
	t.Helper()
	accounts, err := links.AccountsForUser(userID)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("resolve command principal for %s: accounts=%+v err=%v", userID, accounts, err)
	}
	account := accounts[0]
	return identity.Principal{CanonicalUserID: userID, Gateway: account.Gateway, ExternalID: account.Identifier, Assurance: identity.AssuranceDiscordGateway}
}

func newTestService(t *testing.T) *accounts.Service {
	t.Helper()
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	dbPath := filepath.Join(dir, "oswald.db")
	memories := memorytest.NewStore(t, dbPath, log)
	return accounts.NewService(dbPath, memories, nil, log)
}
