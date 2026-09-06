package accounts

import (
	"context"
	"database/sql"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/database"
)

// MCPUserMerger moves encrypted user-owned MCP configuration in a shared transaction.
type MCPUserMerger interface {
	MergeUsersTx(context.Context, *sql.Tx, string, string) error
	UserMergeCommitted(string, string)
	DeleteUserTx(context.Context, *sql.Tx, string) error
	UserDeleteCommitted(string)
}

// LinkChallenge is a newly issued one-time account connection code.
type LinkChallenge struct {
	ID        string
	Code      string
	ExpiresAt time.Time
}

// ConfirmResult describes a completed or idempotently replayed confirmation.
type ConfirmResult struct {
	CanonicalUserID string
	Merged          bool
	AlreadyLinked   bool
	Replayed        bool
}

// UserDeletionDescriptor identifies runtime state removed with an account.
type UserDeletionDescriptor struct {
	ExternalIdentities []string
	SessionIDs         []string
}

// DisconnectDescriptor identifies runtime state invalidated by a completed disconnect.
type DisconnectDescriptor struct {
	ExternalIdentities []string
	SessionIDs         []string
}

// UserSummary is the command-facing view of a canonical user.
type UserSummary struct {
	CanonicalUserID string
	Intro           string
	Accounts        []database.LinkedAccount
	IsAdmin         bool
	IsBanned        bool
	BanReason       string
}
