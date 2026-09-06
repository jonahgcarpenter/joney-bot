package usermanagement

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
)

const bannedMessage = "You are banned from using Oswald."

type handler struct {
	definition commands.Definition
	users      *accounts.Service
}

// New creates admin command handlers backed by canonical users.
func New(users *accounts.Service) []commands.Handler {
	return []commands.Handler{
		&handler{users: users, definition: commands.Definition{Name: "admin", Summary: "Grant admin access to a user.", Usage: "/admin <canonical_id>", AdminOnly: true}},
		&handler{users: users, definition: commands.Definition{Name: "ban", Summary: "Ban a user from using Oswald.", Usage: "/ban <canonical_id> [reason]", AdminOnly: true}},
		&handler{users: users, definition: commands.Definition{Name: "deleteuser", Summary: "Delete a canonical user.", Usage: "/deleteuser <canonical_id>", AdminOnly: true, UserExclusive: true}},
		&handler{users: users, definition: commands.Definition{Name: "unadmin", Summary: "Remove admin access from a user.", Usage: "/unadmin <canonical_id>", AdminOnly: true}},
		&handler{users: users, definition: commands.Definition{Name: "unban", Summary: "Unban a user.", Usage: "/unban <canonical_id>", AdminOnly: true}},
		&handler{users: users, definition: commands.Definition{Name: "user", Summary: "Show one canonical user.", Usage: "/user <canonical_id>", AdminOnly: true}},
		&handler{users: users, definition: commands.Definition{Name: "users", Summary: "List canonical users.", Usage: "/users", AdminOnly: true}},
	}
}

// BannedMessage returns the canonical response for banned users.
func BannedMessage(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "No reason provided."
	}
	return bannedMessage + "\nReason: " + reason
}

// Definition describes the command handled by h.
func (h *handler) Definition() commands.Definition {
	return h.definition
}

// ResolveFenceTargets fences the deletion target as well as the admin actor.
func (h *handler) ResolveFenceTargets(_ context.Context, req commands.Request) ([]string, error) {
	if req.Name != "deleteuser" || len(req.Args) != 1 {
		return nil, nil
	}
	isAdmin, err := commands.IsPrincipalAdmin(h.users, req.Principal)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		return nil, fmt.Errorf("admin access required")
	}
	targetID := strings.TrimSpace(req.Args[0])
	if targetID == "" {
		return nil, nil
	}
	return []string{targetID}, nil
}

// Execute processes one admin command.
func (h *handler) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	switch req.Name {
	case "users":
		return h.handleUsers()
	case "user":
		return h.handleUser(req.Args)
	case "admin":
		return h.handleSetAdmin(req.Principal, req.Args, true)
	case "unadmin":
		return h.handleSetAdmin(req.Principal, req.Args, false)
	case "ban":
		return h.handleBan(req.Principal, req.Args)
	case "deleteuser":
		return h.handleDeleteUser(req.Principal, req.Args)
	case "unban":
		return h.handleUnban(req.Principal, req.Args)
	default:
		return commands.Result{Text: "Unknown command: /" + req.Name}, nil
	}
}

func (h *handler) handleUsers() (commands.Result, error) {
	users, err := h.users.ListUsers()
	if err != nil {
		return commands.Result{}, err
	}
	if len(users) == 0 {
		return commands.Result{Text: "No users found."}, nil
	}

	lines := make([]string, 0, len(users)+1)
	lines = append(lines, "Users:")
	for _, user := range users {
		lines = append(lines, renderUser(user))
	}
	return commands.Result{Text: strings.Join(lines, "\n")}, nil
}

func (h *handler) handleUser(args []string) (commands.Result, error) {
	if len(args) != 1 {
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
	targetID := strings.TrimSpace(args[0])
	user, ok, err := h.users.User(targetID)
	if err != nil {
		return commands.Result{}, err
	}
	if !ok {
		return commands.Result{Text: fmt.Sprintf("User %s not found.", targetID)}, nil
	}
	return commands.Result{Text: renderUser(user)}, nil
}

func (h *handler) handleSetAdmin(principal identity.Principal, args []string, isAdmin bool) (commands.Result, error) {
	if len(args) != 1 {
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
	targetID := strings.TrimSpace(args[0])
	if err := h.users.SetAdminAs(principal, targetID, isAdmin); err != nil {
		return commands.Result{Text: fmt.Sprintf("Could not update admin status: %v", err)}, nil
	}
	if isAdmin {
		return commands.Result{Text: fmt.Sprintf("Marked %s as admin.", targetID)}, nil
	}
	return commands.Result{Text: fmt.Sprintf("Removed admin from %s.", targetID)}, nil
}

func (h *handler) handleBan(principal identity.Principal, args []string) (commands.Result, error) {
	if len(args) < 1 {
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
	targetID := strings.TrimSpace(args[0])
	reason := ""
	if len(args) > 1 {
		reason = strings.Join(args[1:], " ")
	}
	if err := h.users.BanUserAs(principal, targetID, reason); err != nil {
		return commands.Result{Text: fmt.Sprintf("Could not ban user: %v", err)}, nil
	}
	return commands.Result{Text: fmt.Sprintf("Banned %s.", targetID)}, nil
}

func (h *handler) handleUnban(principal identity.Principal, args []string) (commands.Result, error) {
	if len(args) != 1 {
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
	targetID := strings.TrimSpace(args[0])
	if err := h.users.UnbanUserAs(principal, targetID); err != nil {
		return commands.Result{Text: fmt.Sprintf("Could not unban user: %v", err)}, nil
	}
	return commands.Result{Text: fmt.Sprintf("Unbanned %s.", targetID)}, nil
}

func (h *handler) handleDeleteUser(principal identity.Principal, args []string) (commands.Result, error) {
	if len(args) != 1 {
		return commands.Result{Text: commands.UsageText(h.definition)}, nil
	}
	targetID := strings.TrimSpace(args[0])
	descriptor, err := h.users.DeleteUserAsWithRuntimeInvalidation(principal, targetID)
	if err != nil {
		return commands.Result{Text: fmt.Sprintf("Could not delete user: %v", err)}, nil
	}
	event := invalidation.Event{ExternalIdentities: descriptor.ExternalIdentities, SessionIDs: descriptor.SessionIDs, CloseConnections: true}
	return commands.Result{Text: fmt.Sprintf("Deleted %s.", targetID), Invalidation: &event}, nil
}

func renderAccounts(accounts []database.LinkedAccount) string {
	if len(accounts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(accounts))
	for _, account := range accounts {
		label := account.Gateway + ":" + account.Identifier
		if account.DisplayName != "" {
			label += " (" + account.DisplayName + ")"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

func renderUser(user accounts.UserSummary) string {
	line := fmt.Sprintf("%s | admin=%t | banned=%t | %s | accounts: %s", user.CanonicalUserID, user.IsAdmin, user.IsBanned, user.Intro, renderAccounts(user.Accounts))
	if user.IsBanned && strings.TrimSpace(user.BanReason) != "" {
		line += " | ban_reason: " + user.BanReason
	}
	return line
}
