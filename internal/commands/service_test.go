package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func TestServiceExecutesKnownUnknownAndAdminCommands(t *testing.T) {
	auth := fakeAuthorizer{admins: map[string]bool{"admin": true}}
	service, err := NewServiceWithCommands(
		Command{Handler: fakeCommand{name: "ping"}},
		Command{Handler: fakeCommand{name: "secret", admin: true}, Middleware: []Middleware{RequireAdmin(auth)}},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	result, err := service.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/ping one two"})
	if err != nil || result.Text != "ran ping:one two" {
		t.Fatalf("known command result=%+v err=%v", result, err)
	}

	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/missing"})
	if err != nil || result.Text != "Unknown command: /missing" {
		t.Fatalf("unknown command result=%+v err=%v", result, err)
	}

	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("user"), Raw: "/secret"})
	if err != nil || result.Text != "You are not allowed to use admin commands." {
		t.Fatalf("non-admin result=%+v err=%v", result, err)
	}

	result, err = service.Execute(context.Background(), Request{Principal: testPrincipal("admin"), Raw: "/secret"})
	if err != nil || result.Text != "ran secret:" {
		t.Fatalf("admin result=%+v err=%v", result, err)
	}
}

func TestServiceRejectsInvalidPrincipal(t *testing.T) {
	service, err := NewServiceWithCommands(Command{Handler: fakeCommand{name: "ping"}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := service.Execute(context.Background(), Request{Raw: "/ping"}); err == nil {
		t.Fatal("expected invalid principal error")
	}
}

func TestRequireAdminRejectsUnauthenticatedPrincipalBeforeLookup(t *testing.T) {
	auth := &countingAuthorizer{isAdmin: true}
	service, err := NewServiceWithCommands(Command{
		Handler:    fakeCommand{name: "secret", admin: true},
		Middleware: []Middleware{RequireAdmin(auth)},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	principal := identity.Principal{
		CanonicalUserID: "admin",
		Gateway:         "homeassistant",
		ExternalID:      "admin",
		Assurance:       identity.AssuranceSelfAsserted,
	}
	result, err := service.Execute(context.Background(), Request{Principal: principal, Raw: "/secret"})
	if err == nil || result.Text != "" || auth.calls != 0 {
		t.Fatalf("result=%+v err=%v admin lookups=%d", result, err, auth.calls)
	}
}

func TestRequireAdminUsesPrincipalAuthorizer(t *testing.T) {
	auth := &principalAuthorizer{admin: true}
	service, err := NewServiceWithCommands(Command{Handler: fakeCommand{name: "secret"}, Middleware: []Middleware{RequireAdmin(auth)}})
	if err != nil {
		t.Fatal(err)
	}
	principal := testPrincipal("stale")
	result, err := service.Execute(context.Background(), Request{Principal: principal, Raw: "/secret"})
	if err != nil || result.Text != "ran secret:" || auth.principal != principal {
		t.Fatalf("result=%+v err=%v auth=%+v", result, err, auth)
	}
}

func TestServicePropagatesPrincipalToHandler(t *testing.T) {
	want := identity.Principal{
		CanonicalUserID: "user",
		Gateway:         "discord",
		ExternalID:      "123",
		Assurance:       identity.AssuranceDiscordGateway,
	}
	var got identity.Principal
	service, err := NewServiceWithCommands(Command{Handler: HandlerFunc{
		DefinitionValue: Definition{Name: "principal"},
		ExecuteFunc: func(_ context.Context, req Request) (Result, error) {
			got = req.Principal
			return Result{Text: "ok"}, nil
		},
	}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := service.Execute(context.Background(), Request{Principal: want, Raw: "/principal"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != want {
		t.Fatalf("principal = %+v, want %+v", got, want)
	}
}

func TestServiceRejectsDuplicateNamesAndAliases(t *testing.T) {
	_, err := NewServiceWithCommands(Command{Handler: fakeCommand{name: "ping"}}, Command{Handler: fakeCommand{name: "ping"}})
	if !errors.Is(err, ErrDuplicateCommand) {
		t.Fatalf("expected duplicate command error, got %v", err)
	}

	_, err = NewServiceWithCommands(Command{Handler: fakeCommand{name: "ping", aliases: []string{"p"}}}, Command{Handler: fakeCommand{name: "pong", aliases: []string{"p"}}})
	if !errors.Is(err, ErrDuplicateAlias) {
		t.Fatalf("expected duplicate alias error, got %v", err)
	}
}

func TestServiceDefinitionResolvesAliasesAndExclusivity(t *testing.T) {
	service, err := NewServiceWithCommands(Command{Handler: HandlerFunc{DefinitionValue: Definition{Name: "memories", Aliases: []string{"memory"}, UserExclusive: true}}})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := service.Definition("/memory")
	if !ok || definition.Name != "memories" || !definition.UserExclusive {
		t.Fatalf("definition=%+v found=%v", definition, ok)
	}
	if _, ok := service.Definition("missing"); ok {
		t.Fatal("unknown command returned a definition")
	}
}

func TestServiceResolvesFenceTargetsWithParsedRequestBeforeMiddleware(t *testing.T) {
	service, err := NewServiceWithCommands(Command{
		Handler: HandlerFunc{
			DefinitionValue: Definition{Name: "deleteuser", Aliases: []string{"removeuser"}},
			ExecuteFunc:     func(context.Context, Request) (Result, error) { return Result{}, nil },
			ResolveFenceTargetsFunc: func(_ context.Context, req Request) ([]string, error) {
				if req.Name != "deleteuser" || req.ArgsText != "target" || len(req.Args) != 1 {
					t.Fatalf("resolver received unparsed request: %+v", req)
				}
				return []string{req.Args[0]}, nil
			},
		},
		Middleware: []Middleware{RequireAdmin(fakeAuthorizer{admins: map[string]bool{"admin": true}})},
	})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := service.ResolveFenceTargets(context.Background(), Request{Principal: testPrincipal("admin"), Raw: "/removeuser target"})
	if err != nil || len(targets) != 1 || targets[0] != "target" {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
}

type fakeCommand struct {
	name    string
	aliases []string
	admin   bool
}

func (c fakeCommand) Definition() Definition {
	return Definition{Name: c.name, Aliases: c.aliases, AdminOnly: c.admin}
}

func (c fakeCommand) Execute(_ context.Context, req Request) (Result, error) {
	return Result{Text: "ran " + req.Name + ":" + req.ArgsText}, nil
}

type fakeAuthorizer struct {
	admins map[string]bool
}

type countingAuthorizer struct {
	isAdmin bool
	calls   int
}

type principalAuthorizer struct {
	admin     bool
	principal identity.Principal
}

func testPrincipal(userID string) identity.Principal {
	return identity.Principal{CanonicalUserID: userID, Gateway: "discord", ExternalID: "external-" + userID, Assurance: identity.AssuranceDiscordGateway}
}

func (a fakeAuthorizer) IsAdminPrincipal(principal identity.Principal) (bool, error) {
	return a.admins[principal.CanonicalUserID], nil
}

func (a *countingAuthorizer) IsAdminPrincipal(identity.Principal) (bool, error) {
	a.calls++
	return a.isAdmin, nil
}

func (a *principalAuthorizer) IsAdminPrincipal(principal identity.Principal) (bool, error) {
	a.principal = principal
	return a.admin, nil
}
