package accounts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// EnsureAccount resolves an external account to a canonical user ID, creating one when needed.
func (s *Service) EnsureAccount(ctx context.Context, gateway, identifier, displayName string) (string, error) {
	identifier, err := NormalizeIdentifier(gateway, identifier)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return "", err
	}

	key := accountKey(gateway, identifier)
	if canonicalID, ok := data.AccountIndex[key]; ok {
		user := data.Users[canonicalID]
		updated := false
		for i := range user.Accounts {
			if user.Accounts[i].Gateway == gateway && user.Accounts[i].Identifier == identifier {
				if displayName != "" && user.Accounts[i].DisplayName != displayName {
					user.Accounts[i].DisplayName = displayName
					updated = true
				}
				break
			}
		}
		if updated {
			data.Users[canonicalID] = user
			if err := s.saveLocked(data); err != nil {
				return "", err
			}
		}
		if err := s.memories.SyncSpeakerIntro(canonicalID, FormatSpeakerLine(user.Accounts)); err != nil {
			return "", err
		}
		return canonicalID, nil
	}

	canonicalID, err := newCanonicalUserID()
	if err != nil {
		return "", err
	}
	data.Users[canonicalID] = database.AccountUser{
		Accounts: []database.LinkedAccount{{
			Gateway:     strings.ToLower(gateway),
			Identifier:  identifier,
			DisplayName: displayName,
			Verified:    false,
		}},
	}
	data.AccountIndex[key] = canonicalID

	if err := s.saveLocked(data); err != nil {
		return "", err
	}
	s.log.With(requestctx.LogFields(ctx)...).Info("account_link.canonical_user.created", "created canonical user", config.F("target_user_id", canonicalID), config.F("status", "ok"))
	if err := s.memories.SyncSpeakerIntro(canonicalID, FormatSpeakerLine(data.Users[canonicalID].Accounts)); err != nil {
		return "", err
	}

	return canonicalID, nil
}

// ResolveAccount returns the current canonical owner without creating an account.
func (s *Service) ResolveAccount(gateway, identifier string) (string, bool, error) {
	identifier, err := NormalizeIdentifier(gateway, identifier)
	if err != nil {
		return "", false, err
	}
	gateway = strings.ToLower(strings.TrimSpace(gateway))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureInitializedLocked(); err != nil {
		return "", false, err
	}
	var owner string
	err = s.db.SQL().QueryRow(`SELECT canonical_user_id FROM linked_accounts WHERE gateway = ? AND identifier = ?`, gateway, identifier).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve linked account: %w", err)
	}
	return owner, true, nil
}

// ResolvePrincipal re-resolves an authenticated external identity to its current
// canonical owner. The canonical ID carried by principal is intentionally ignored.
func (s *Service) ResolvePrincipal(principal identity.Principal) (string, error) {
	if !principal.Valid() || !principal.Authenticated() {
		return "", fmt.Errorf("operation requires an authenticated identity")
	}
	owner, found, err := s.ResolveAccount(principal.Gateway, principal.ExternalID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrPrincipalMismatch
	}
	return owner, nil
}

// RunAuthenticatedCanonicalMutation serializes an authenticated mutation with account
// creation, linking, merging, and display-name updates while re-resolving the
// external identity under the same account graph lock.
func (s *Service) RunAuthenticatedCanonicalMutation(principal identity.Principal, fn func(string) error) error {
	if !principal.Valid() || !principal.Authenticated() {
		return fmt.Errorf("operation requires an authenticated identity")
	}
	identifier, err := NormalizeIdentifier(principal.Gateway, principal.ExternalID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	owner, ok := data.AccountIndex[accountKey(principal.Gateway, identifier)]
	if !ok {
		return ErrPrincipalMismatch
	}
	return fn(owner)
}

// AccountsForUser returns the linked accounts for a canonical user.
func (s *Service) AccountsForUser(canonicalUserID string) ([]database.LinkedAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	user, ok := data.Users[canonicalUserID]
	if !ok {
		return nil, nil
	}

	accounts := append([]database.LinkedAccount(nil), user.Accounts...)
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Gateway == accounts[j].Gateway {
			return accounts[i].Identifier < accounts[j].Identifier
		}
		return accounts[i].Gateway < accounts[j].Gateway
	})
	return accounts, nil
}

// DisconnectAccountAs removes one exact linked account after atomically
// re-resolving the authenticated initiating principal and durably queues its
// runtime invalidation scope.
func (s *Service) DisconnectAccountAs(ctx context.Context, principal identity.Principal, gateway, identifier, requestID string) (DisconnectDescriptor, error) {
	if !principal.Valid() || !principal.Authenticated() {
		return DisconnectDescriptor{}, fmt.Errorf("disconnect requires an authenticated identity")
	}
	principalIdentifier, err := NormalizeIdentifier(principal.Gateway, principal.ExternalID)
	if err != nil {
		return DisconnectDescriptor{}, err
	}
	identifier, err = NormalizeIdentifier(gateway, identifier)
	if err != nil {
		return DisconnectDescriptor{}, err
	}
	gateway = strings.ToLower(strings.TrimSpace(gateway))
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return DisconnectDescriptor{}, fmt.Errorf("disconnect request ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureInitializedLocked(); err != nil {
		return DisconnectDescriptor{}, err
	}

	canonicalUserID := principal.CanonicalUserID
	descriptor := DisconnectDescriptor{
		ExternalIdentities: []string{accountKey(gateway, identifier)},
	}
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		owner, _, err := accountOwnerTx(ctx, tx, principal.Gateway, principalIdentifier)
		if err != nil {
			return err
		}
		if owner != canonicalUserID {
			return ErrPrincipalMismatch
		}

		var targetOwner string
		if err := tx.QueryRowContext(ctx, `SELECT canonical_user_id FROM linked_accounts WHERE gateway = ? AND identifier = ?`, gateway, identifier).Scan(&targetOwner); err == sql.ErrNoRows {
			return policyError("not_found", "linked account not found")
		} else if err != nil {
			return fmt.Errorf("resolve disconnect target: %w", err)
		}
		if targetOwner != canonicalUserID {
			return policyError("not_found", "linked account not found")
		}

		var accountCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM linked_accounts WHERE canonical_user_id = ?`, canonicalUserID).Scan(&accountCount); err != nil {
			return fmt.Errorf("count linked accounts: %w", err)
		}
		if accountCount <= 1 {
			return policyError("last_account", "cannot disconnect the last linked account")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM linked_accounts WHERE canonical_user_id = ? AND gateway = ? AND identifier = ?`, canonicalUserID, gateway, identifier); err != nil {
			return fmt.Errorf("delete linked account: %w", err)
		}

		rows, err := tx.QueryContext(ctx, `SELECT gateway, identifier, display_name FROM linked_accounts WHERE canonical_user_id = ? ORDER BY gateway, identifier`, canonicalUserID)
		if err != nil {
			return fmt.Errorf("read remaining linked accounts: %w", err)
		}
		remaining := make([]database.LinkedAccount, 0, accountCount-1)
		for rows.Next() {
			var account database.LinkedAccount
			if err := rows.Scan(&account.Gateway, &account.Identifier, &account.DisplayName); err != nil {
				rows.Close()
				return fmt.Errorf("scan remaining linked account: %w", err)
			}
			remaining = append(remaining, account)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_users SET speaker_intro = ? WHERE canonical_user_id = ?`, FormatSpeakerLine(remaining), canonicalUserID); err != nil {
			return fmt.Errorf("update speaker intro: %w", err)
		}

		sessionRows, err := tx.QueryContext(ctx, `SELECT session_id FROM sessions WHERE canonical_user_id = ? ORDER BY session_id`, canonicalUserID)
		if err != nil {
			return fmt.Errorf("read disconnect sessions: %w", err)
		}
		for sessionRows.Next() {
			var sessionID string
			if err := sessionRows.Scan(&sessionID); err != nil {
				sessionRows.Close()
				return err
			}
			descriptor.SessionIDs = append(descriptor.SessionIDs, sessionID)
		}
		if err := sessionRows.Close(); err != nil {
			return err
		}
		if err := sessionRows.Err(); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return DisconnectDescriptor{}, err
	}
	s.log.With(requestctx.LogFields(ctx)...).Info("account_link.account.disconnected", "disconnected account", config.F("request_id", requestID), config.F("actor_user_id", canonicalUserID), config.F("target_user_id", canonicalUserID), config.F("status", "ok"))
	return descriptor, nil
}

func newCanonicalUserID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate canonical user ID: %w", err)
	}
	return "usr_" + hex.EncodeToString(b), nil
}

func summarizeUser(canonicalID string, user database.AccountUser) UserSummary {
	accounts := append([]database.LinkedAccount(nil), user.Accounts...)
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Gateway == accounts[j].Gateway {
			return accounts[i].Identifier < accounts[j].Identifier
		}
		return accounts[i].Gateway < accounts[j].Gateway
	})

	return UserSummary{
		CanonicalUserID: canonicalID,
		Intro:           FormatSpeakerLine(accounts),
		Accounts:        accounts,
		IsAdmin:         user.IsAdmin,
		IsBanned:        user.IsBanned,
		BanReason:       user.BanReason,
	}
}

// FormatSpeakerLine formats a stable speaker line from linked gateway accounts.
func FormatSpeakerLine(accounts []database.LinkedAccount) string {
	var imessageName string
	var discordName string
	var homeAssistantName string

	for _, account := range accounts {
		name := strings.TrimSpace(account.DisplayName)
		if name == "" {
			continue
		}

		switch account.Gateway {
		case "imessage":
			if imessageName == "" {
				imessageName = name
			}
		case "discord":
			if discordName == "" {
				discordName = name
			}
		case "homeassistant":
			if homeAssistantName == "" {
				homeAssistantName = name
			}
		}
	}

	switch {
	case imessageName != "" && discordName != "":
		if strings.EqualFold(imessageName, discordName) {
			return fmt.Sprintf("You are speaking with %s.", imessageName)
		}
		return fmt.Sprintf("You are speaking with %s aka %s.", imessageName, discordName)
	case imessageName != "":
		return fmt.Sprintf("You are speaking with %s.", imessageName)
	case discordName != "":
		return fmt.Sprintf("You are speaking with %s.", discordName)
	case homeAssistantName != "":
		return fmt.Sprintf("You are speaking with %s.", homeAssistantName)
	default:
		return "You are speaking with a returning user."
	}
}
