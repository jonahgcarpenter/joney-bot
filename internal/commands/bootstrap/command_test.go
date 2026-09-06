package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type fakeAccounts struct {
	mu        sync.Mutex
	hasAdmin  bool
	claimErr  error
	claimCall int
}

func (f *fakeAccounts) HasAdmin() (bool, error) { return f.hasAdmin, nil }

func (f *fakeAccounts) ClaimBootstrapAdmin(_ context.Context, principal identity.Principal) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCall++
	if f.claimErr != nil {
		return "", false, f.claimErr
	}
	if f.hasAdmin {
		return "", false, nil
	}
	f.hasAdmin = true
	return principal.CanonicalUserID, true, nil
}

func TestBootstrapRedeemsOnceForChatGateways(t *testing.T) {
	for _, gateway := range []struct {
		name      string
		assurance identity.Assurance
	}{
		{name: "discord", assurance: identity.AssuranceDiscordGateway},
		{name: "imessage", assurance: identity.AssuranceBlueBubblesWebhook},
		{name: "homeassistant", assurance: identity.AssuranceHomeAssistantToken},
	} {
		t.Run(gateway.name, func(t *testing.T) {
			accounts := &fakeAccounts{}
			service, code, err := New(accounts, WithRandom(bytes.NewReader(make([]byte, 8))))
			if err != nil || code != "2222-2222-2222-2222" {
				t.Fatalf("New() code=%q err=%v", code, err)
			}
			principal := identity.Principal{CanonicalUserID: "user", Gateway: gateway.name, ExternalID: "external", Assurance: gateway.assurance}
			result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Args: []string{code}})
			if err != nil || result.Text != "Administrator access granted to account user." {
				t.Fatalf("first redemption result=%+v err=%v", result, err)
			}
			result, err = service.Execute(context.Background(), commands.Request{Principal: principal, Args: []string{code}})
			if err != nil || result.Text != "Bootstrap is unavailable or has already been completed." || accounts.claimCall != 1 {
				t.Fatalf("second redemption result=%+v calls=%d err=%v", result, accounts.claimCall, err)
			}
		})
	}
}

func TestBootstrapRejectsInvalidCodeAndUnauthenticatedPrincipal(t *testing.T) {
	accounts := &fakeAccounts{}
	service, code, err := New(accounts, WithRandom(bytes.NewReader(make([]byte, 8))))
	if err != nil {
		t.Fatal(err)
	}
	discord := identity.Principal{CanonicalUserID: "discord-user", Gateway: "discord", ExternalID: "external", Assurance: identity.AssuranceDiscordGateway}
	result, err := service.Execute(context.Background(), commands.Request{Principal: discord, Args: []string{"WRONG-CODE"}})
	if err != nil || result.Text != "That bootstrap code is invalid." {
		t.Fatalf("invalid result=%+v err=%v", result, err)
	}
	unauthenticated := identity.Principal{CanonicalUserID: "ha-user", Gateway: "homeassistant", ExternalID: "subject", Assurance: identity.AssuranceSelfAsserted}
	result, err = service.Execute(context.Background(), commands.Request{Principal: unauthenticated, Args: []string{code}})
	if err != nil || result.Text != "Bootstrap is available only from an authenticated Discord, iMessage, or Home Assistant account." {
		t.Fatalf("unauthenticated result=%+v err=%v", result, err)
	}
	result, err = service.Execute(context.Background(), commands.Request{Principal: discord, Args: []string{code}})
	if err != nil || result.Text != "Administrator access granted to account discord-user." || accounts.claimCall != 1 {
		t.Fatalf("valid result=%+v calls=%d err=%v", result, accounts.claimCall, err)
	}
}

func TestBootstrapStorageFailureDoesNotConsumeCode(t *testing.T) {
	accounts := &fakeAccounts{claimErr: errors.New("write failed")}
	service, code, err := New(accounts, WithRandom(bytes.NewReader(make([]byte, 8))))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "discord", ExternalID: "external", Assurance: identity.AssuranceDiscordGateway}
	if _, err := service.Execute(context.Background(), commands.Request{Principal: principal, Args: []string{code}}); err == nil {
		t.Fatal("expected storage error")
	}
	accounts.mu.Lock()
	accounts.claimErr = nil
	accounts.mu.Unlock()
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Args: []string{code}})
	if err != nil || result.Text != "Administrator access granted to account user." {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
}

func TestBootstrapConcurrentRedemptionHasOneWinner(t *testing.T) {
	accounts := &fakeAccounts{}
	service, code, err := New(accounts, WithRandom(bytes.NewReader(make([]byte, 8))))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan commands.Result, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for _, userID := range []string{"first", "second"} {
		wait.Add(1)
		go func(userID string) {
			defer wait.Done()
			principal := identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: userID, Assurance: identity.AssuranceDiscordGateway}
			result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Args: []string{code}})
			results <- result
			errors <- err
		}(userID)
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	granted := 0
	unavailable := 0
	for result := range results {
		switch result.Text {
		case "Administrator access granted to account first.", "Administrator access granted to account second.":
			granted++
		case "Bootstrap is unavailable or has already been completed.":
			unavailable++
		default:
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if granted != 1 || unavailable != 1 || accounts.claimCall != 1 {
		t.Fatalf("granted=%d unavailable=%d calls=%d", granted, unavailable, accounts.claimCall)
	}
}

func TestBootstrapDisabledWhenAdminExists(t *testing.T) {
	accounts := &fakeAccounts{hasAdmin: true}
	service, code, err := New(accounts)
	if err != nil || code != "" {
		t.Fatalf("New() code=%q err=%v", code, err)
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "discord", ExternalID: "external", Assurance: identity.AssuranceDiscordGateway}
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Args: []string{"anything"}})
	if err != nil || result.Text != "Bootstrap is unavailable or has already been completed." {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
