package session

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

// Resetter resets one canonical user's current session context.
type Resetter interface {
	ResetSessionContext(context.Context, string, string) error
}

type handler struct{ sessions Resetter }

// New creates the session reset command.
func New(sessions Resetter) commands.Handler { return &handler{sessions: sessions} }

func (h *handler) Definition() commands.Definition {
	return commands.Definition{Name: "reset", Summary: "Reset this conversation and load your latest profile.", Usage: "/reset"}
}

func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) != 0 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Session reset requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	if h.sessions == nil {
		return commands.Result{}, fmt.Errorf("session reset is not configured")
	}
	if err := h.sessions.ResetSessionContext(ctx, req.Principal.CanonicalUserID, req.SessionKey); err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Text: "Conversation context reset. Your latest profile will be used from now on.", Outcome: commands.Outcome{Status: "ok", Operation: "session.reset", IsChanged: true, AffectedCount: 1}}, nil
}
