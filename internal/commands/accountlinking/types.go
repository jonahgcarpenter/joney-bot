package accountlinking

import "github.com/jonahgcarpenter/oswald-ai/internal/database"

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
