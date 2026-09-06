package stop

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type fakeCanceler struct {
	activeReport broker.CancelReport
	allReport    broker.CancelReport
	userID       string
	sessionID    string
	allCalled    bool
}

func (f *fakeCanceler) CancelActiveAgentWork(userID, sessionID string) broker.CancelReport {
	f.userID, f.sessionID = userID, sessionID
	return f.activeReport
}

func (f *fakeCanceler) CancelAllAgentWork() broker.CancelReport {
	f.allCalled = true
	return f.allReport
}

type fakeAuth struct{ admin bool }

func (f fakeAuth) IsAdminPrincipal(identity.Principal) (bool, error) { return f.admin, nil }

func TestStopCurrentAndAdminAll(t *testing.T) {
	canceler := &fakeCanceler{activeReport: broker.CancelReport{ActiveSignaled: 1}, allReport: broker.CancelReport{ActiveSignaled: 2, QueuedCanceled: 3}}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(canceler, fakeAuth{admin: true}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, SessionKey: "session", Raw: "/stop"})
	if err != nil || result.Text != "Stopped the current response." || canceler.userID != "user" || canceler.sessionID != "session" {
		t.Fatalf("current result=%+v err=%v canceler=%+v", result, err, canceler)
	}
	result, err = service.Execute(context.Background(), commands.Request{Principal: principal, Raw: "/stop all"})
	if err != nil || result.Text != "Stopped 2 active and 3 queued foreground requests." || !canceler.allCalled {
		t.Fatalf("all result=%+v err=%v canceler=%+v", result, err, canceler)
	}
}

func TestStopAllRequiresAdmin(t *testing.T) {
	canceler := &fakeCanceler{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(canceler, fakeAuth{}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{CanonicalUserID: "user", Gateway: "homeassistant", ExternalID: "user", Assurance: identity.AssuranceHomeAssistantToken}
	result, err := service.Execute(context.Background(), commands.Request{Principal: principal, Raw: "/stop all"})
	if err != nil || result.Text != "You are not allowed to use admin commands." || canceler.allCalled {
		t.Fatalf("result=%+v err=%v canceler=%+v", result, err, canceler)
	}
}
