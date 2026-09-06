package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type helpAuthorizer struct{}

func (helpAuthorizer) IsAdminPrincipal(principal identity.Principal) (bool, error) {
	return principal.ExternalID == "admin-account", nil
}

func TestHelpUsesPrincipalAuthorization(t *testing.T) {
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: commands.HandlerFunc{DefinitionValue: commands.Definition{Name: "secret", Usage: "/secret", AdminOnly: true}}})
	if err != nil {
		t.Fatal(err)
	}
	help := helpHandler{commands: service, auth: helpAuthorizer{}}
	for _, test := range []struct {
		name, canonicalID, externalID string
		admin                         bool
	}{
		{name: "stale admin ID denied", canonicalID: "admin", externalID: "user-account"},
		{name: "current account owner authorized", canonicalID: "stale", externalID: "admin-account", admin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			principal := identity.Principal{CanonicalUserID: test.canonicalID, Gateway: "discord", ExternalID: test.externalID, Assurance: identity.AssuranceDiscordGateway}
			for _, args := range [][]string{nil, {"secret"}} {
				result, err := help.Execute(context.Background(), commands.Request{Principal: principal, Args: args})
				if err != nil {
					t.Fatal(err)
				}
				visible := strings.Contains(result.Text, "/secret") && !strings.Contains(result.Text, "Unknown command")
				if visible != test.admin {
					t.Fatalf("args=%v result=%q admin=%t", args, result.Text, test.admin)
				}
			}
		})
	}
	help.auth = nil
	if definitions, err := help.visibleDefinitions(identity.Principal{}); err != nil || len(definitions) != 0 {
		t.Fatalf("nil authorizer definitions=%v err=%v", definitions, err)
	}
}
