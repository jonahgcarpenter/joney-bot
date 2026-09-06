package accountlinking

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/testutil"
)

func TestServiceEnsureLinkDisconnectAndSpeakerLine(t *testing.T) {
	links := newTestService(t)

	userID, err := links.EnsureAccount("discord", "123", "Alice")
	if err != nil {
		t.Fatalf("ensure discord: %v", err)
	}
	again, err := links.EnsureAccount("discord", "123", "Alice Updated")
	if err != nil {
		t.Fatalf("ensure existing discord: %v", err)
	}
	if again != userID {
		t.Fatalf("expected same canonical user, got %q then %q", userID, again)
	}

	localID, err := links.EnsureAccount("homeassistant", "alice-local", "")
	if err != nil {
		t.Fatalf("ensure websocket: %v", err)
	}
	connectTestAccounts(t, links,
		identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway},
		identity.Principal{CanonicalUserID: localID, Gateway: "homeassistant", ExternalID: "alice-local", Assurance: identity.AssuranceHomeAssistantToken})

	accounts, err := links.AccountsForUser(userID)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(accounts) != 2 || accounts[0].Gateway != "discord" || accounts[1].Gateway != "homeassistant" {
		t.Fatalf("unexpected sorted accounts: %+v", accounts)
	}

	user, found, err := links.User(userID)
	if err != nil || !found {
		t.Fatalf("user: found=%t err=%v", found, err)
	}
	if user.Intro != "You are speaking with Alice Updated." {
		t.Fatalf("unexpected speaker line %q", user.Intro)
	}

	descriptor, err := links.DisconnectAccountAs(context.Background(), identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}, "homeassistant", "alice-local", "disconnect-store-test")
	if err != nil {
		t.Fatalf("disconnect websocket: %v", err)
	}
	if len(descriptor.ExternalIdentities) != 1 || descriptor.ExternalIdentities[0] != "homeassistant:alice-local" {
		t.Fatalf("unexpected invalidation descriptor: %+v", descriptor)
	}
	if _, err := links.DisconnectAccountAs(context.Background(), identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}, "discord", "123", "disconnect-last-test"); err == nil {
		t.Fatal("expected error disconnecting last account")
	}
}

func TestClaimBootstrapAdminUsesAuthenticatedChatOwner(t *testing.T) {
	links := newTestService(t)
	userID, err := links.EnsureAccount("discord", "123", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if hasAdmin, err := links.HasAdmin(); err != nil || hasAdmin {
		t.Fatalf("HasAdmin()=%t err=%v", hasAdmin, err)
	}
	principal := identity.Principal{CanonicalUserID: "stale-user", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	claimedID, claimed, err := links.ClaimBootstrapAdmin(principal)
	if err != nil || !claimed || claimedID != userID {
		t.Fatalf("ClaimBootstrapAdmin() id=%q claimed=%t err=%v", claimedID, claimed, err)
	}
	if admin, err := links.IsAdmin(userID); err != nil || !admin {
		t.Fatalf("IsAdmin()=%t err=%v", admin, err)
	}
	otherID, err := links.EnsureAccount("imessage", "+15555550100", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	claimedID, claimed, err = links.ClaimBootstrapAdmin(identity.Principal{CanonicalUserID: otherID, Gateway: "imessage", ExternalID: "+15555550100", Assurance: identity.AssuranceBlueBubblesWebhook})
	if err != nil || claimed || claimedID != "" {
		t.Fatalf("second ClaimBootstrapAdmin() id=%q claimed=%t err=%v", claimedID, claimed, err)
	}
}

func TestCommandHandlerConnectAndDisconnect(t *testing.T) {
	links := newTestService(t)
	userID, err := links.EnsureAccount("discord", "123", "Alice")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	otherID, err := links.EnsureAccount("homeassistant", "alice-local", "Alice Local")
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

func TestDisconnectRejectsStalePrincipalAndNonOwnedExactTarget(t *testing.T) {
	links := newTestService(t)
	userID, _ := links.EnsureAccount("discord", "702", "Owner")
	localID, _ := links.EnsureAccount("homeassistant", "owner-local", "Owner Local")
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "702", Assurance: identity.AssuranceDiscordGateway}
	connectTestAccounts(t, links, principal, identity.Principal{CanonicalUserID: localID, Gateway: "homeassistant", ExternalID: "owner-local", Assurance: identity.AssuranceHomeAssistantToken})
	_, _ = links.EnsureAccount("imessage", "outsider@example.com", "Outsider")

	stale := principal
	stale.CanonicalUserID = "usr_stale"
	if _, err := links.DisconnectAccountAs(context.Background(), stale, "homeassistant", "owner-local", "req-stale"); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("stale principal error=%v", err)
	}
	if _, err := links.DisconnectAccountAs(context.Background(), principal, "imessage", "outsider@example.com", "req-target"); err == nil || !strings.Contains(err.Error(), "linked account not found") {
		t.Fatalf("non-owned exact target error=%v", err)
	}
	accounts, err := links.AccountsForUser(userID)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("rejected disconnect mutated accounts: accounts=%+v err=%v", accounts, err)
	}
}

func executeAccountCommand(service *commands.Service, principal identity.Principal, raw string) (string, error) {
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Raw: raw, RequestID: "req_test"})
	return result.Text, err
}

func TestServicePersistsSQLiteAccounts(t *testing.T) {
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	dbPath := filepath.Join(dir, "oswald.db")
	memories := testutil.NewMemoryStore(t, dbPath, log)

	links := NewService(dbPath, memories, nil, log)
	userID, err := links.EnsureAccount("discord", "123", "Alice")
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	localID, err := links.EnsureAccount("homeassistant", "alice-local", "")
	if err != nil {
		t.Fatalf("ensure websocket: %v", err)
	}
	connectTestAccounts(t, links,
		identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway},
		identity.Principal{CanonicalUserID: localID, Gateway: "homeassistant", ExternalID: "alice-local", Assurance: identity.AssuranceHomeAssistantToken})

	reopened := NewService(dbPath, memories, nil, log)
	accounts, err := reopened.AccountsForUser(userID)
	if err != nil {
		t.Fatalf("accounts after reopen: %v", err)
	}
	if len(accounts) != 2 || accounts[0].Gateway != "discord" || accounts[1].Gateway != "homeassistant" {
		t.Fatalf("unexpected persisted accounts: %+v", accounts)
	}
}

func TestServiceAdminBanAndListUsers(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "100", "Admin")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	targetID, err := links.EnsureAccount("discord", "200", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}

	admin := identity.Principal{CanonicalUserID: adminID, Gateway: "discord", ExternalID: "100", Assurance: identity.AssuranceDiscordGateway}
	claimTestAdmin(t, links, admin)
	if err := links.SetAdminAs(admin, adminID, false); err == nil || !strings.Contains(err.Error(), "cannot remove admin from yourself") {
		t.Fatalf("expected self unadmin error, got %v", err)
	}
	if err := links.BanUserAs(admin, adminID, "bad"); err == nil || !strings.Contains(err.Error(), "cannot ban yourself") {
		t.Fatalf("expected self ban error, got %v", err)
	}
	if err := links.BanUserAs(admin, targetID, "spam"); err != nil {
		t.Fatalf("ban target: %v", err)
	}

	isAdmin, err := links.IsAdmin(adminID)
	if err != nil || !isAdmin {
		t.Fatalf("expected admin true, got %v err=%v", isAdmin, err)
	}
	isBanned, _, err := links.BanStatus(targetID)
	if err != nil || !isBanned {
		t.Fatalf("expected banned true, got %v err=%v", isBanned, err)
	}

	users, err := links.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %+v", users)
	}
	foundTarget := false
	for _, user := range users {
		if user.CanonicalUserID == targetID {
			foundTarget = true
			if !user.IsBanned || user.BanReason != "spam" || !strings.Contains(user.Intro, "Target") {
				t.Fatalf("unexpected target summary: %+v", user)
			}
		}
	}
	if !foundTarget {
		t.Fatalf("target not found in users: %+v", users)
	}

	if err := links.UnbanUserAs(admin, targetID); err != nil {
		t.Fatalf("unban target: %v", err)
	}
	isBanned, _, err = links.BanStatus(targetID)
	if err != nil || isBanned {
		t.Fatalf("expected banned false, got %v err=%v", isBanned, err)
	}
	users, err = links.ListUsers()
	if err != nil {
		t.Fatalf("list users after unban: %v", err)
	}
	foundTarget = false
	for _, user := range users {
		if user.CanonicalUserID == targetID {
			foundTarget = true
			if user.IsBanned || user.BanReason != "" {
				t.Fatalf("expected cleared ban fields after unban, got %+v", user)
			}
		}
	}
	if !foundTarget {
		t.Fatalf("target not found after unban: %+v", users)
	}
}

func TestServiceAdminAuthorizationRebindsExternalAccountOwner(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "810", "Admin")
	if err != nil {
		t.Fatal(err)
	}
	claimTestAdmin(t, links, identity.Principal{CanonicalUserID: adminID, Gateway: "discord", ExternalID: "810", Assurance: identity.AssuranceDiscordGateway})
	if _, err := links.EnsureAccount("discord", "811", "User"); err != nil {
		t.Fatal(err)
	}
	stale := identity.Principal{CanonicalUserID: adminID, Gateway: "discord", ExternalID: "811", Assurance: identity.AssuranceDiscordGateway}
	isAdmin, err := links.IsAdminPrincipal(stale)
	if err != nil || isAdmin {
		t.Fatalf("stale principal authorized: admin=%t err=%v", isAdmin, err)
	}
	missing := identity.Principal{CanonicalUserID: adminID, Gateway: "discord", ExternalID: "812", Assurance: identity.AssuranceDiscordGateway}
	isAdmin, err = links.IsAdminPrincipal(missing)
	if err != nil || isAdmin {
		t.Fatalf("unowned principal authorized: admin=%t err=%v", isAdmin, err)
	}
}

func TestServiceVerifiedMergePreservesAdminState(t *testing.T) {
	links := newTestService(t)
	targetID, err := links.EnsureAccount("discord", "300", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}
	sourceID, err := links.EnsureAccount("homeassistant", "source", "Source")
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	claimTestAdmin(t, links, identity.Principal{CanonicalUserID: sourceID, Gateway: "homeassistant", ExternalID: "source", Assurance: identity.AssuranceHomeAssistantToken})
	result := connectTestAccounts(t, links,
		identity.Principal{CanonicalUserID: targetID, Gateway: "discord", ExternalID: "300", Assurance: identity.AssuranceDiscordGateway},
		identity.Principal{CanonicalUserID: sourceID, Gateway: "homeassistant", ExternalID: "source", Assurance: identity.AssuranceHomeAssistantToken})
	if !result.Merged {
		t.Fatalf("expected merge result: %+v", result)
	}
	isAdmin, err := links.IsAdmin(targetID)
	if err != nil || !isAdmin {
		t.Fatalf("expected merged admin true, got %v err=%v", isAdmin, err)
	}
}

func TestServiceDeleteUserRemovesAccountsMemoryAndSessions(t *testing.T) {
	links := newTestService(t)
	adminID, err := links.EnsureAccount("discord", "400", "Admin")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	targetID, err := links.EnsureAccount("discord", "500", "Target")
	if err != nil {
		t.Fatalf("ensure target: %v", err)
	}
	localID, err := links.EnsureAccount("homeassistant", "target-local", "Target Local")
	if err != nil {
		t.Fatalf("ensure websocket: %v", err)
	}
	connectTestAccounts(t, links,
		identity.Principal{CanonicalUserID: targetID, Gateway: "discord", ExternalID: "500", Assurance: identity.AssuranceDiscordGateway},
		identity.Principal{CanonicalUserID: localID, Gateway: "homeassistant", ExternalID: "target-local", Assurance: identity.AssuranceHomeAssistantToken})
	admin := identity.Principal{CanonicalUserID: adminID, Gateway: "discord", ExternalID: "400", Assurance: identity.AssuranceDiscordGateway}
	claimTestAdmin(t, links, admin)

	db := links.db.SQL()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE account_users SET speaker_intro = ? WHERE canonical_user_id = ?`, "You are speaking with Target.", targetID); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_entries (canonical_user_id, scope, category, statement, claim_slot, claim_value, confidence, importance, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, targetID, "long_term", "durable_preferences", "The user likes green.", "durable_preferences.fact", "the_user_likes_green", 0.9, 3, "active", now, now); err != nil {
		t.Fatalf("insert memory entry: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_turns (session_id, canonical_user_id, user_text, assistant_text, created_at) VALUES (?, ?, ?, ?, ?)`, "session-target", targetID, "hello", "hi", now); err != nil {
		t.Fatalf("insert session turn: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO mcp_servers (id, scope, owner_user_id, name, transport, url_ciphertext) VALUES (?, ?, ?, ?, ?, ?)`, "mcp-target", "user", targetID, "target-tools", "streamable_http", "ciphertext"); err != nil {
		t.Fatalf("insert mcp server: %v", err)
	}

	if err := links.DeleteUserAs(admin, adminID); err == nil || !strings.Contains(err.Error(), "cannot delete yourself") {
		t.Fatalf("expected self delete error, got %v", err)
	}
	if err := links.DeleteUserAs(admin, targetID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	if _, ok, err := links.User(targetID); err != nil || ok {
		t.Fatalf("expected deleted user missing, ok=%v err=%v", ok, err)
	}
	accounts, err := links.AccountsForUser(targetID)
	if err != nil {
		t.Fatalf("accounts after delete: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("expected no accounts after delete, got %+v", accounts)
	}

	assertRowCount(t, db, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM linked_accounts WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM account_users WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = ?`, targetID, 0)
	assertRowCount(t, db, `SELECT COUNT(*) FROM mcp_servers WHERE owner_user_id = ?`, targetID, 0)

	recreatedID, err := links.EnsureAccount("discord", "500", "Target Recreated")
	if err != nil {
		t.Fatalf("recreate deleted account: %v", err)
	}
	if recreatedID == targetID {
		t.Fatalf("expected deleted external account to create a new canonical user, got original %s", recreatedID)
	}
}

func assertRowCount(t *testing.T, db interface {
	QueryRow(string, ...interface{}) *sql.Row
}, query, userID string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query, userID).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("count query %q got %d, want %d", query, got, want)
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	log := config.NewLogger(config.LevelError)
	dbPath := filepath.Join(dir, "oswald.db")
	memories := testutil.NewMemoryStore(t, dbPath, log)
	t.Cleanup(func() { memories.Close() })
	links := NewService(dbPath, memories, nil, log)
	t.Cleanup(func() { links.Close() })
	return links
}

func claimTestAdmin(t *testing.T, links *Service, principal identity.Principal) {
	t.Helper()
	if owner, claimed, err := links.ClaimBootstrapAdmin(principal); err != nil || !claimed || owner != principal.CanonicalUserID {
		t.Fatalf("claim bootstrap admin: owner=%q claimed=%t err=%v", owner, claimed, err)
	}
}

func connectTestAccounts(t *testing.T, links *Service, initiator, confirmer identity.Principal) ConfirmResult {
	t.Helper()
	challenge, err := links.CreateChallenge(context.Background(), initiator, "req_create")
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}
	result, err := links.ConfirmChallenge(context.Background(), confirmer, challenge.Code, "req_confirm")
	if err != nil {
		t.Fatalf("confirm challenge: %v", err)
	}
	return result
}
