package accountlinking

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
)

type handler struct {
	definition commands.Definition
	links      *accounts.Service
}

// New creates account-link command handlers backed by the shared account-link service.
func New(links *accounts.Service) []commands.Handler {
	return []commands.Handler{
		&handler{links: links, definition: commands.Definition{Name: "connect", Summary: "Securely connect another authenticated account.", Usage: "/connect [code|cancel]"}},
		&handler{links: links, definition: commands.Definition{Name: "disconnect", Summary: "Disconnect a linked gateway account.", Usage: "/disconnect [account_number]", UserExclusive: true}},
	}
}

// Definition describes the command handled by h.
func (h *handler) Definition() commands.Definition {
	return h.definition
}

// ResolveFenceTargets resolves both current account owners before a connection
// confirmation enters the broker fence.
func (h *handler) ResolveFenceTargets(ctx context.Context, req commands.Request) ([]string, error) {
	if req.Name != "connect" || !req.Principal.Authenticated() || len(req.Args) != 1 || strings.EqualFold(req.Args[0], "cancel") {
		return nil, nil
	}
	return h.links.ResolveChallengeFenceTargets(ctx, req.Principal, req.Args[0])
}

// Execute processes one account-link command.
func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Account changes require an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	switch req.Name {
	case "connect":
		return h.handleConnect(ctx, req)
	case "disconnect":
		return h.handleDisconnect(ctx, req)
	default:
		return commands.Result{Text: "Unknown command: /" + req.Name, Outcome: commands.Outcome{Status: "rejected", ReasonCode: "unknown_command"}}, nil
	}
}

func (h *handler) handleConnect(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) == 0 {
		challenge, err := h.links.CreateChallenge(ctx, req.Principal, req.RequestID)
		if err != nil {
			return linkErrorResult(err)
		}
		return commands.Result{Text: fmt.Sprintf("On the other account, send:\n\n/connect %s\n\nThis code expires in 10 minutes and can be used once. Do not share it.", challenge.Code)}, nil
	}
	if len(req.Args) != 1 {
		return commands.Result{Text: commands.UsageText(h.definition), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if strings.EqualFold(req.Args[0], "cancel") {
		cancelled, err := h.links.CancelChallenge(ctx, req.Principal, req.RequestID)
		if err != nil {
			return linkErrorResult(err)
		}
		if !cancelled {
			return commands.Result{Text: "There is no active connection code to cancel.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op"}}, nil
		}
		return commands.Result{Text: "The active connection code was cancelled."}, nil
	}
	result, err := h.links.ConfirmChallenge(ctx, req.Principal, req.Args[0], req.RequestID)
	if err != nil {
		return linkErrorResult(err)
	}
	if result.AlreadyLinked {
		return commands.Result{Text: "These accounts are already connected and are now verified."}, nil
	}
	if result.Replayed {
		return commands.Result{Text: "These accounts were already connected successfully.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op"}}, nil
	}
	return commands.Result{Text: "Accounts connected successfully. This account now uses the profile that created the connection code."}, nil
}

func linkErrorResult(err error) (result commands.Result, resultErr error) {
	defer func() {
		if reason, expected := accounts.PolicyReason(err); expected {
			result.Outcome = commands.Outcome{Status: "rejected", ReasonCode: reason}
		}
	}()
	switch {
	case errors.Is(err, accounts.ErrChallengeInvalid):
		return commands.Result{Text: "That connection code is invalid, expired, or has already been used. Start again with /connect on the account you want to keep."}, nil
	case errors.Is(err, accounts.ErrChallengeSameActor):
		return commands.Result{Text: "Enter this code from the other account you want to connect."}, nil
	case errors.Is(err, accounts.ErrGatewayConflict):
		return commands.Result{Text: "These profiles cannot be connected because both contain different accounts for the same gateway."}, nil
	case errors.Is(err, accounts.ErrMCPConflict):
		return commands.Result{Text: "These profiles have MCP servers with the same name. Rename or remove one of the conflicting servers, then try again."}, nil
	case errors.Is(err, accounts.ErrLinkBanned):
		return commands.Result{Text: "Banned profiles cannot be connected."}, nil
	case errors.Is(err, accounts.ErrPrincipalMismatch):
		return commands.Result{Text: "Your account identity changed. Send the command again."}, nil
	default:
		return commands.Result{}, err
	}
}

func (h *handler) handleDisconnect(ctx context.Context, req commands.Request) (commands.Result, error) {
	canonicalUserID := req.Principal.CanonicalUserID
	args := req.Args
	if len(args) == 0 {
		return h.startDisconnect(canonicalUserID)
	}
	if len(args) != 1 {
		return disconnectUsage(h.definition, canonicalUserID, h.links)
	}

	selection, err := strconv.Atoi(args[0])
	if err != nil {
		return disconnectUsage(h.definition, canonicalUserID, h.links)
	}

	linkedAccounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	if selection < 1 || selection > len(linkedAccounts) {
		return disconnectUsage(h.definition, canonicalUserID, h.links)
	}

	account := linkedAccounts[selection-1]
	descriptor, err := h.links.DisconnectAccountAs(ctx, req.Principal, account.Gateway, account.Identifier, req.RequestID)
	if err != nil {
		if errors.Is(err, accounts.ErrPrincipalMismatch) {
			return commands.Result{Text: "Your account identity changed. Send the command again.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "principal_mismatch"}}, nil
		}
		if reason, expected := accounts.PolicyReason(err); expected {
			return commands.Result{Text: fmt.Sprintf("Could not disconnect %s: %v", gatewayLabel(account.Gateway), err), Outcome: commands.Outcome{Status: "rejected", ReasonCode: reason}}, nil
		}
		return commands.Result{}, err
	}

	remaining, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	message := fmt.Sprintf("Disconnected %s: %s.\n\nRemaining linked accounts:\n%s", gatewayLabel(account.Gateway), account.Identifier, renderLinkedAccounts(remaining))
	return commands.Result{Text: message, Invalidation: &invalidation.Event{ExternalIdentities: descriptor.ExternalIdentities, SessionIDs: descriptor.SessionIDs, CloseConnections: true}}, nil
}

func (h *handler) startDisconnect(canonicalUserID string) (commands.Result, error) {
	accounts, err := h.links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	if len(accounts) <= 1 {
		return commands.Result{Text: "You only have one linked account. Oswald will not disconnect the last account.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "last_account"}}, nil
	}

	var lines []string
	lines = append(lines, "Disconnect an account.")
	for i, account := range accounts {
		lines = append(lines, fmt.Sprintf("%d. %s: %s", i+1, gatewayLabel(account.Gateway), account.Identifier))
	}
	lines = append(lines, "")
	lines = append(lines, "Use: /disconnect <number>")
	lines = append(lines, "The last linked account cannot be removed.")

	return commands.Result{Text: strings.Join(lines, "\n")}, nil
}

func gatewayLabel(key string) string {
	if option, ok := accounts.GatewayOptionByKey(key); ok {
		return option.Label
	}
	return key
}

func renderLinkedAccounts(accounts []database.LinkedAccount) string {
	if len(accounts) == 0 {
		return "- none"
	}
	lines := make([]string, 0, len(accounts))
	for _, account := range accounts {
		status := "unverified"
		if account.Verified {
			status = "verified"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s (%s)", gatewayLabel(account.Gateway), account.Identifier, status))
	}
	return strings.Join(lines, "\n")
}

func disconnectUsage(definition commands.Definition, canonicalUserID string, links *accounts.Service) (commands.Result, error) {
	accounts, err := links.AccountsForUser(canonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	if len(accounts) == 0 {
		return commands.Result{Text: commands.UsageText(definition), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	lines := []string{definition.Summary, "Use: /disconnect <number>"}
	for i, account := range accounts {
		lines = append(lines, fmt.Sprintf("%d. %s: %s", i+1, gatewayLabel(account.Gateway), account.Identifier))
	}
	return commands.Result{Text: strings.Join(lines, "\n"), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
}
