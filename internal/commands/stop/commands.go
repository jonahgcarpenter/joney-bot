package stop

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// Canceler stops foreground agent work without shutting down the broker.
type Canceler interface {
	CancelActiveAgentWork(canonicalUserID, sessionID string) broker.CancelReport
	CancelAllAgentWork() broker.CancelReport
}

type principalResolver interface {
	ResolvePrincipal(identity.Principal) (string, error)
}

type handler struct {
	canceler Canceler
	auth     commands.PrincipalAuthorizer
	resolver principalResolver
}

// New creates the out-of-band stop command.
func New(canceler Canceler, auth commands.PrincipalAuthorizer, resolver principalResolver) commands.Handler {
	return handler{canceler: canceler, auth: auth, resolver: resolver}
}

func (handler) Definition() commands.Definition {
	return commands.Definition{
		Name: "stop", Summary: "Stop a running response.", Usage: "/stop [all]", OutOfBand: true,
	}
}

func (h handler) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	if h.canceler == nil {
		return commands.Result{}, fmt.Errorf("stop command is unavailable")
	}
	if len(req.Args) == 0 {
		userID := req.Principal.CanonicalUserID
		if h.resolver != nil {
			resolved, err := h.resolver.ResolvePrincipal(req.Principal)
			if err != nil {
				return commands.Result{}, err
			}
			userID = resolved
		}
		report := h.canceler.CancelActiveAgentWork(userID, req.SessionKey)
		if report.ActiveSignaled == 0 {
			return commands.Result{Text: "Nothing is currently running in this conversation.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op", Operation: "stop.session"}}, nil
		}
		return commands.Result{Text: "Stopped the current response.", Outcome: commands.Outcome{Status: "ok", Operation: "stop.session", IsChanged: true, AffectedCount: report.ActiveSignaled, ActiveCanceledCount: report.ActiveSignaled}}, nil
	}
	if len(req.Args) != 1 || req.Args[0] != "all" {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	isAdmin, err := commands.IsPrincipalAdmin(h.auth, req.Principal)
	if err != nil {
		return commands.Result{}, err
	}
	if !isAdmin {
		return commands.Result{Text: "You are not allowed to use admin commands.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "admin_required"}}, nil
	}
	report := h.canceler.CancelAllAgentWork()
	total := report.ActiveSignaled + report.QueuedCanceled
	if total == 0 {
		return commands.Result{Text: "No foreground requests are currently running or queued.", Outcome: commands.Outcome{Status: "ok", ReasonCode: "no_op", Operation: "stop.all"}}, nil
	}
	return commands.Result{Text: fmt.Sprintf("Stopped %d active and %d queued foreground requests.", report.ActiveSignaled, report.QueuedCanceled), Outcome: commands.Outcome{Status: "ok", Operation: "stop.all", IsChanged: true, AffectedCount: total, ActiveCanceledCount: report.ActiveSignaled, QueuedCanceledCount: report.QueuedCanceled}}, nil
}
