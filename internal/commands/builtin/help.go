package builtin

import (
	"context"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type helpHandler struct {
	commands *commands.Service
	auth     commands.PrincipalAuthorizer
}

func (h helpHandler) Definition() commands.Definition {
	return commands.Definition{Name: "help", Summary: "List commands or show usage for one command.", Usage: "/help [command]"}
}

func (h helpHandler) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	definitions, err := h.visibleDefinitions(req.Principal)
	if err != nil {
		return commands.Result{}, err
	}
	if len(req.Args) > 0 {
		want := strings.TrimPrefix(strings.TrimSpace(req.Args[0]), "/")
		for _, definition := range definitions {
			if definition.Name == want {
				return commands.Result{Text: renderHelpFor(definition)}, nil
			}
			for _, alias := range definition.Aliases {
				if alias == want {
					return commands.Result{Text: renderHelpFor(definition)}, nil
				}
			}
		}
		return commands.Result{Text: "Unknown command: /" + want, Outcome: commands.Outcome{Status: "rejected", ReasonCode: "unknown_command"}}, nil
	}

	lines := make([]string, 0, len(definitions)+1)
	lines = append(lines, "Commands:")
	for _, definition := range definitions {
		line := "/" + definition.Name
		if definition.Summary != "" {
			line += " - " + definition.Summary
		}
		lines = append(lines, line)
	}
	return commands.Result{Text: strings.Join(lines, "\n")}, nil
}

func (h helpHandler) visibleDefinitions(principal identity.Principal) ([]commands.Definition, error) {
	definitions := h.commands.Definitions()
	isAdmin, err := commands.IsPrincipalAdmin(h.auth, principal)
	if err != nil {
		return nil, err
	}
	return filterAdminDefinitions(definitions, isAdmin), nil
}

func filterAdminDefinitions(definitions []commands.Definition, includeAdmin bool) []commands.Definition {
	filtered := make([]commands.Definition, 0, len(definitions))
	for _, definition := range definitions {
		if definition.AdminOnly && !includeAdmin {
			continue
		}
		filtered = append(filtered, definition)
	}
	return filtered
}

func renderHelpFor(definition commands.Definition) string {
	lines := []string{"/" + definition.Name}
	if definition.Summary != "" {
		lines = append(lines, definition.Summary)
	}
	if definition.Usage != "" {
		lines = append(lines, "Use: "+definition.Usage)
	}
	return strings.Join(lines, "\n")
}
