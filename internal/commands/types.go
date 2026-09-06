package commands

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
)

const (
	// MaxAttachmentBytes is the maximum size of one command attachment.
	MaxAttachmentBytes = media.MaxOutputAttachmentBytes
	// MaxAttachments is the maximum number of attachments in one command response.
	MaxAttachments = media.MaxOutputAttachments
	// MaxTotalAttachmentBytes is the maximum combined attachment size in one command response.
	MaxTotalAttachmentBytes = media.MaxTotalOutputAttachmentBytes
)

// Definition describes a command registered with the command service.
type Definition struct {
	Name          string
	Aliases       []string
	Summary       string
	Usage         string
	AdminOnly     bool
	UserExclusive bool
	OutOfBand     bool
}

// Request is the gateway-neutral command execution context. Conversation scope
// is resolved before dispatch so handlers cannot distinguish groups from DMs.
type Request struct {
	RequestID   string
	Principal   identity.Principal
	ChatID      string
	SessionKey  string
	DisplayName string
	ClientID    string

	Raw      string
	Name     string
	Args     []string
	ArgsText string
}

// Attachment is an in-memory file delivered with a command response.
type Attachment = media.OutputAttachment

// Result is the user-facing command response.
type Result struct {
	Outcome      Outcome `json:"-"`
	Text         string
	Attachments  []Attachment
	Invalidation *invalidation.Event `json:"-"`
}

// Outcome is bounded operational telemetry, independent of user-facing text.
// IsChanged is set only after a committed mutation or an effective cancellation.
type Outcome struct {
	Status, ReasonCode, Operation                           string
	IsChanged                                               bool
	AffectedCount, ActiveCanceledCount, QueuedCanceledCount int
}

// ValidateAttachments validates per-file and aggregate transport limits.
func (r Result) ValidateAttachments() error {
	if err := media.ValidateOutputAttachments(r.Attachments); err != nil {
		return fmt.Errorf("invalid command attachments: %w", err)
	}
	return nil
}

// UsageText renders the standard command usage response.
func UsageText(definition Definition) string {
	if definition.Summary == "" {
		return "Use: " + definition.Usage
	}
	return definition.Summary + "\nUse: " + definition.Usage
}

// Handler executes one registered command.
type Handler interface {
	Definition() Definition
	Execute(context.Context, Request) (Result, error)
}

// FenceTargetResolver resolves canonical users whose normal work must be
// excluded while a command executes. The service passes a parsed Request.
type FenceTargetResolver interface {
	ResolveFenceTargets(context.Context, Request) ([]string, error)
}

// HandlerFunc adapts a function to a command handler.
type HandlerFunc struct {
	DefinitionValue         Definition
	ExecuteFunc             func(context.Context, Request) (Result, error)
	ResolveFenceTargetsFunc func(context.Context, Request) ([]string, error)
}

// Definition returns the function handler's command metadata.
func (h HandlerFunc) Definition() Definition {
	return h.DefinitionValue
}

// Execute runs the wrapped function.
func (h HandlerFunc) Execute(ctx context.Context, req Request) (Result, error) {
	return h.ExecuteFunc(ctx, req)
}

// ResolveFenceTargets resolves optional command-specific mutation fences.
func (h HandlerFunc) ResolveFenceTargets(ctx context.Context, req Request) ([]string, error) {
	if h.ResolveFenceTargetsFunc == nil {
		return nil, nil
	}
	return h.ResolveFenceTargetsFunc(ctx, req)
}

// Middleware wraps a command handler with cross-cutting behavior.
type Middleware func(Handler) Handler

// Command registers a handler and its middleware with the command service.
type Command struct {
	Handler    Handler
	Middleware []Middleware
}

// PrincipalAuthorizer re-resolves an authenticated external account before
// checking permissions.
type PrincipalAuthorizer interface {
	IsAdminPrincipal(principal identity.Principal) (bool, error)
}

// IsPrincipalAdmin requires account-bound authorization for permission checks.
func IsPrincipalAdmin(auth PrincipalAuthorizer, principal identity.Principal) (bool, error) {
	if auth == nil || !principal.Authenticated() {
		return false, nil
	}
	return auth.IsAdminPrincipal(principal)
}
