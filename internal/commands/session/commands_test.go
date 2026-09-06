package session

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestResetUsesCanonicalUserAndCurrentSession(t *testing.T) {
	resetter := &fakeResetter{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(resetter)})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "external", Assurance: identity.AssuranceHomeAssistantToken}
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, SessionKey: "session", Raw: "/reset"})
	if err != nil {
		t.Fatal(err)
	}
	if resetter.userID != "user" || resetter.sessionID != "session" {
		t.Fatalf("reset scope user=%q session=%q", resetter.userID, resetter.sessionID)
	}
	if result.Text != "Conversation context reset. Your latest profile will be used from now on." {
		t.Fatalf("unexpected response %q", result.Text)
	}
}

func TestResetRejectsUnauthenticatedPrincipal(t *testing.T) {
	resetter := &fakeResetter{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(resetter)})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "external", Assurance: identity.AssuranceSelfAsserted}
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, SessionKey: "session", Raw: "/reset"})
	if err == nil || resetter.userID != "" || result.Text != "" {
		t.Fatalf("result=%q resetter=%+v err=%v", result.Text, resetter, err)
	}
}

type fakeResetter struct{ userID, sessionID string }

func (f *fakeResetter) ResetSessionContext(_ context.Context, userID, sessionID string) error {
	f.userID = userID
	f.sessionID = sessionID
	return nil
}
