package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
)

var errAdminRequired = errors.New("You are not allowed to use admin commands.")

// New returns the /mcp command handler.
func New(store *mcpmanager.Store, manager *mcpmanager.Manager, auth commands.PrincipalAuthorizer) commands.Handler {
	return handler{store: store, manager: manager, auth: auth}
}

type handler struct {
	store   *mcpmanager.Store
	manager *mcpmanager.Manager
	auth    commands.PrincipalAuthorizer
}

func (h handler) Definition() commands.Definition {
	return commands.Definition{Name: "mcp", Summary: "Manage your MCP server connections.", Usage: "/mcp servers|add|remove|enable|disable|test ... (add requires a description)"}
}

func (h handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) == 0 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	args := append([]string(nil), req.Args...)
	actorID := req.Principal.CanonicalUserID
	scope := mcpmanager.ScopeUser
	owner := actorID
	if args[0] == "global" {
		if err := h.requireAdmin(req.Principal); err != nil {
			if errors.Is(err, errAdminRequired) {
				return commands.Result{Text: err.Error(), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "admin_required"}}, nil
			}
			return commands.Result{}, err
		}
		scope = mcpmanager.ScopeGlobal
		owner = ""
		args = args[1:]
		if len(args) == 0 {
			return commands.Result{Text: "Use: /mcp global servers|add|remove|enable|disable|test ...", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
		}
	}
	if h.store == nil || h.manager == nil {
		return commands.Result{}, fmt.Errorf("MCP configuration is unavailable")
	}
	switch args[0] {
	case "servers", "list":
		return h.list(ctx, actorID, scope)
	case "add":
		return h.add(ctx, scope, owner, args[1:])
	case "remove", "delete":
		return h.remove(ctx, scope, owner, args[1:])
	case "enable":
		return h.setEnabled(ctx, scope, owner, args[1:], true)
	case "disable":
		return h.setEnabled(ctx, scope, owner, args[1:], false)
	case "test":
		return h.test(ctx, actorID, scope, owner, args[1:])
	default:
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
}

func (h handler) requireAdmin(principal identity.Principal) error {
	if h.auth == nil {
		return errAdminRequired
	}
	isAdmin, err := commands.IsPrincipalAdmin(h.auth, principal)
	if err != nil {
		return err
	}
	if !isAdmin {
		return errAdminRequired
	}
	return nil
}

func (h handler) list(ctx context.Context, userID, scope string) (commands.Result, error) {
	infos := h.manager.ServerInfos(ctx, userID)
	lines := []string{"MCP servers:"}
	for _, info := range infos {
		if scope != "" && info.Scope != scope {
			continue
		}
		line := fmt.Sprintf("%s (%s): %s", info.Name, info.Scope, info.Status)
		if info.ToolCount > 0 {
			line += fmt.Sprintf(", tools: %d", info.ToolCount)
		}
		if info.Reason != "" {
			line += ", reason: " + info.Reason
		}
		if info.Description == "" {
			line += ", description: required"
		} else {
			line += ", description: " + info.Description
		}
		lines = append(lines, line)
	}
	if len(lines) == 1 {
		return commands.Result{Text: "No MCP servers configured."}, nil
	}
	return commands.Result{Text: strings.Join(lines, "\n")}, nil
}

func (h handler) add(ctx context.Context, scope, owner string, args []string) (commands.Result, error) {
	if len(args) < 3 {
		return commands.Result{Text: addUsage(scope), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	name := args[0]
	url := args[1]
	headers, description, err := parseAddOptions(args[2:])
	if err != nil {
		return commands.Result{Text: err.Error(), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	_, err = h.store.Save(ctx, mcpmanager.ServerConfig{Scope: scope, OwnerUserID: owner, Name: name, Description: description, Transport: mcpmanager.TransportStreamableHTTP, URL: url, Headers: headers, Enabled: true})
	if err != nil {
		return commands.Result{}, err
	}
	h.manager.Invalidate(scope, owner, strings.ToLower(name))
	return commands.Result{Text: fmt.Sprintf("MCP server %q saved. URL and headers are encrypted at rest.", strings.ToLower(name)), Outcome: commands.Outcome{Status: "ok", Operation: "mcp.save", IsChanged: true, AffectedCount: 1}}, nil
}

func addUsage(scope string) string {
	prefix := "/mcp add"
	if scope == mcpmanager.ScopeGlobal {
		prefix = "/mcp global add"
	}
	return "Use: " + prefix + " <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>] <description>"
}

func parseAddOptions(args []string) (map[string]string, string, error) {
	var optionArgs, descriptionParts []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "auth-bearer=") || strings.HasPrefix(arg, "header:") {
			optionArgs = append(optionArgs, arg)
			continue
		}
		descriptionParts = append(descriptionParts, arg)
	}
	if len(descriptionParts) == 0 {
		return nil, "", fmt.Errorf("MCP server description cannot be empty")
	}
	headers, err := parseHeaders(optionArgs)
	if err != nil {
		return nil, "", err
	}
	return headers, strings.Join(descriptionParts, " "), nil
}

func (h handler) remove(ctx context.Context, scope, owner string, args []string) (commands.Result, error) {
	if len(args) < 1 {
		return commands.Result{Text: "Use: /mcp remove <name>", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	changed, err := h.store.Delete(ctx, scope, owner, args[0])
	if err != nil {
		return commands.Result{}, err
	}
	h.manager.Invalidate(scope, owner, strings.ToLower(args[0]))
	outcome := commands.Outcome{Status: "ok", Operation: "mcp.remove", IsChanged: changed}
	if changed {
		outcome.AffectedCount = 1
	} else {
		outcome.ReasonCode = "no_op"
	}
	return commands.Result{Text: fmt.Sprintf("MCP server %q removed.", strings.ToLower(args[0])), Outcome: outcome}, nil
}

func (h handler) setEnabled(ctx context.Context, scope, owner string, args []string, enabled bool) (commands.Result, error) {
	if len(args) < 1 {
		return commands.Result{Text: "Use: /mcp enable <name>", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	changed, err := h.store.SetEnabled(ctx, scope, owner, args[0], enabled)
	if err != nil {
		return commands.Result{}, err
	}
	h.manager.Invalidate(scope, owner, strings.ToLower(args[0]))
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	operation := "mcp.disable"
	if enabled {
		operation = "mcp.enable"
	}
	outcome := commands.Outcome{Status: "ok", Operation: operation, IsChanged: changed}
	if outcome.IsChanged {
		outcome.AffectedCount = 1
	} else {
		outcome.ReasonCode = "no_op"
	}
	return commands.Result{Text: fmt.Sprintf("MCP server %q %s.", strings.ToLower(args[0]), state), Outcome: outcome}, nil
}

func (h handler) test(ctx context.Context, userID, scope, owner string, args []string) (commands.Result, error) {
	_ = owner
	if len(args) < 1 {
		return commands.Result{Text: "Use: /mcp test <name>", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	lookupUser := userID
	if scope == mcpmanager.ScopeGlobal {
		lookupUser = ""
	}
	tools, info, err := h.manager.ServerToolSpecs(ctx, lookupUser, args[0])
	if err != nil {
		return commands.Result{}, err
	}
	if info.Status != "connected" {
		if info.Reason != "" {
			return commands.Result{Text: fmt.Sprintf("MCP server %q is %s: %s", info.Name, info.Status, info.Reason), Outcome: commands.Outcome{Status: "degraded", ReasonCode: "unavailable"}}, nil
		}
		return commands.Result{Text: fmt.Sprintf("MCP server %q is %s.", info.Name, info.Status), Outcome: commands.Outcome{Status: "degraded", ReasonCode: "unavailable"}}, nil
	}
	return commands.Result{Text: fmt.Sprintf("MCP server %q connected. Tools: %d.", info.Name, len(tools))}, nil
}

func parseHeaders(args []string) (map[string]string, error) {
	headers := map[string]string{}
	for _, arg := range args {
		if token, ok := strings.CutPrefix(arg, "auth-bearer="); ok {
			if strings.TrimSpace(token) == "" {
				return nil, fmt.Errorf("auth-bearer value cannot be empty")
			}
			headers["Authorization"] = "Bearer " + token
			continue
		}
		nameValue, ok := strings.CutPrefix(arg, "header:")
		if !ok {
			return nil, fmt.Errorf("unsupported MCP add option %q", arg)
		}
		name, value, ok := strings.Cut(nameValue, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("headers must use header:<name>=<value>")
		}
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return headers, nil
}
