package accountlinking

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// Service manages canonical user IDs and linked gateway accounts.
type Service struct {
	path     string
	memories *usermemory.Store
	log      *config.Logger
	db       *database.DB
	mcp      MCPUserMerger
	now      func() time.Time
	random   io.Reader
	mu       sync.Mutex
	initOnce sync.Once
	initErr  error
}

// NewService creates a new account-link service backed by a SQLite database on disk.
func NewService(path string, memories *usermemory.Store, mcp MCPUserMerger, log *config.Logger) *Service {
	return &Service{path: path, memories: memories, mcp: mcp, log: log, now: time.Now, random: rand.Reader}
}

// Initialize prepares the account-link database.
func (s *Service) Initialize() error {
	s.initOnce.Do(func() {
		s.initErr = s.initialize()
	})
	return s.initErr
}

// Close releases the account-link service's database handle.
func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// EnsureAccount resolves an external account to a canonical user ID, creating one when needed.
func (s *Service) EnsureAccount(gateway, identifier, displayName string) (string, error) {
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
	if err := s.memories.SyncSpeakerIntro(canonicalID, FormatSpeakerLine(data.Users[canonicalID].Accounts)); err != nil {
		return "", err
	}

	s.log.Info("account_link.canonical_user.created", "created canonical user", config.F("target_user_id", canonicalID), config.F("account", key))
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

// UserDataResetCommitted clears runtime state for deleted user-owned MCP configuration.
func (s *Service) UserDataResetCommitted(canonicalUserID string) {
	if s.mcp != nil {
		s.mcp.UserDeleteCommitted(canonicalUserID)
	}
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

// ListUsers returns all canonical users with compact account and status details.
func (s *Service) ListUsers() ([]UserSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return nil, err
	}

	userIDs := make([]string, 0, len(data.Users))
	for canonicalID := range data.Users {
		userIDs = append(userIDs, canonicalID)
	}
	sort.Strings(userIDs)

	users := make([]UserSummary, 0, len(userIDs))
	for _, canonicalID := range userIDs {
		users = append(users, summarizeUser(canonicalID, data.Users[canonicalID]))
	}
	return users, nil
}

// User returns one canonical user's summary.
func (s *Service) User(canonicalUserID string) (UserSummary, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return UserSummary{}, false, err
	}
	user, ok := data.Users[canonicalUserID]
	if !ok {
		return UserSummary{}, false, nil
	}
	return summarizeUser(canonicalUserID, user), true, nil
}

// IsAdmin reports a canonical user's administrator state. Permission checks
// must use IsAdminPrincipal to re-resolve the authenticated account owner.
func (s *Service) IsAdmin(canonicalUserID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	user, ok := data.Users[canonicalUserID]
	return ok && user.IsAdmin, nil
}

// HasAdmin reports whether any canonical user currently has administrator access.
func (s *Service) HasAdmin() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	for _, user := range data.Users {
		if user.IsAdmin {
			return true, nil
		}
	}
	return false, nil
}

// ClaimBootstrapAdmin promotes the current owner of a supported authenticated
// principal only while no administrator exists.
func (s *Service) ClaimBootstrapAdmin(principal identity.Principal) (string, bool, error) {
	if !principal.Authenticated() || (principal.Gateway != "discord" && principal.Gateway != "imessage" && principal.Gateway != "homeassistant") {
		return "", false, nil
	}
	identifier, err := NormalizeIdentifier(principal.Gateway, principal.ExternalID)
	if err != nil {
		return "", false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return "", false, err
	}
	for _, user := range data.Users {
		if user.IsAdmin {
			return "", false, nil
		}
	}
	userID, ok := data.AccountIndex[accountKey(principal.Gateway, identifier)]
	if !ok {
		return "", false, nil
	}
	user, ok := data.Users[userID]
	if !ok {
		return "", false, nil
	}
	user.IsAdmin = true
	data.Users[userID] = user
	if err := s.saveLocked(data); err != nil {
		return "", false, err
	}
	s.log.Info("account_link.user.admin_bootstrapped", "granted initial administrator access", config.F("target_user_id", userID), config.F("gateway", principal.Gateway), config.F("status", "ok"))
	return userID, true, nil
}

// IsAdminPrincipal checks admin status for the current owner of the principal's
// authenticated external account, ignoring any stale canonical ID it carries.
func (s *Service) IsAdminPrincipal(principal identity.Principal) (bool, error) {
	if !principal.Authenticated() {
		return false, nil
	}
	identifier, err := NormalizeIdentifier(principal.Gateway, principal.ExternalID)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	owner, ok := data.AccountIndex[accountKey(principal.Gateway, identifier)]
	if !ok {
		return false, nil
	}
	user, ok := data.Users[owner]
	return ok && user.IsAdmin, nil
}

// BanStatus returns whether a canonical user is banned and the stored reason.
func (s *Service) BanStatus(canonicalUserID string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return false, "", err
	}
	user, ok := data.Users[canonicalUserID]
	if !ok || !user.IsBanned {
		return false, "", nil
	}
	return true, user.BanReason, nil
}

// SetAdminAs updates admin state after atomically re-resolving the authenticated actor.
func (s *Service) SetAdminAs(principal identity.Principal, targetID string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return err
	}
	return s.setAdminLocked(data, actorID, targetID, isAdmin)
}

func (s *Service) setAdminLocked(data database.AccountLinkData, actorID, targetID string, isAdmin bool) error {
	if actorID == targetID && !isAdmin {
		return fmt.Errorf("cannot remove admin from yourself")
	}
	user, ok := data.Users[targetID]
	if !ok {
		return fmt.Errorf("canonical user %q not found", targetID)
	}
	if user.IsAdmin == isAdmin {
		return nil
	}
	user.IsAdmin = isAdmin
	data.Users[targetID] = user
	if err := s.saveLocked(data); err != nil {
		return err
	}
	if isAdmin {
		s.log.Info("account_link.user.admin_granted", "granted user admin access", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	} else {
		s.log.Info("account_link.user.admin_revoked", "revoked user admin access", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	}
	return nil
}

// BanUserAs bans a user after atomically re-resolving the authenticated actor.
func (s *Service) BanUserAs(principal identity.Principal, targetID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return err
	}
	return s.banUserLocked(data, actorID, targetID, reason)
}

func (s *Service) banUserLocked(data database.AccountLinkData, actorID, targetID, reason string) error {
	if actorID == targetID {
		return fmt.Errorf("cannot ban yourself")
	}
	user, ok := data.Users[targetID]
	if !ok {
		return fmt.Errorf("canonical user %q not found", targetID)
	}
	user.IsBanned = true
	user.BanReason = strings.TrimSpace(reason)
	data.Users[targetID] = user
	if err := s.saveLocked(data); err != nil {
		return err
	}
	s.log.Info("account_link.user.banned", "banned user", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	return nil
}

// UnbanUserAs unbans a user after atomically re-resolving the authenticated actor.
func (s *Service) UnbanUserAs(principal identity.Principal, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return err
	}
	return s.unbanUserLocked(data, actorID, targetID)
}

func (s *Service) unbanUserLocked(data database.AccountLinkData, actorID, targetID string) error {
	user, ok := data.Users[targetID]
	if !ok {
		return fmt.Errorf("canonical user %q not found", targetID)
	}
	if !user.IsBanned && user.BanReason == "" {
		return nil
	}
	user.IsBanned = false
	user.BanReason = ""
	data.Users[targetID] = user
	if err := s.saveLocked(data); err != nil {
		return err
	}
	s.log.Info("account_link.user.unbanned", "unbanned user", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("status", "ok"))
	return nil
}

// DeleteUserAs deletes a user after atomically re-resolving the authenticated actor.
func (s *Service) DeleteUserAs(principal identity.Principal, targetID string) error {
	_, err := s.DeleteUserAsWithRuntimeInvalidation(principal, targetID)
	return err
}

// DeleteUserAsWithRuntimeInvalidation deletes a user and returns its runtime scope.
func (s *Service) DeleteUserAsWithRuntimeInvalidation(principal identity.Principal, targetID string) (UserDeletionDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked()
	if err != nil {
		return UserDeletionDescriptor{}, err
	}
	actorID, err := authenticatedAdminActor(data, principal)
	if err != nil {
		return UserDeletionDescriptor{}, err
	}
	return s.deleteUserLockedWithInvalidation(data, actorID, strings.TrimSpace(targetID))
}

func (s *Service) deleteUserLockedWithInvalidation(data database.AccountLinkData, actorID, targetID string) (UserDeletionDescriptor, error) {
	if targetID == "" {
		return UserDeletionDescriptor{}, fmt.Errorf("canonical user ID cannot be empty")
	}
	if actorID == targetID {
		return UserDeletionDescriptor{}, fmt.Errorf("cannot delete yourself")
	}
	user, ok := data.Users[targetID]
	if !ok {
		return UserDeletionDescriptor{}, fmt.Errorf("canonical user %q not found", targetID)
	}

	ctx := context.Background()
	var invalidation usermemory.UserDeletionScope
	if err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if s.mcp != nil {
			if err := s.mcp.DeleteUserTx(ctx, tx, targetID); err != nil {
				return err
			}
		}
		var err error
		invalidation, err = s.memories.DeleteUserTx(ctx, tx, targetID, time.Now().UTC())
		return err
	}); err != nil {
		return UserDeletionDescriptor{}, err
	}
	if s.mcp != nil {
		s.mcp.UserDeleteCommitted(targetID)
	}

	s.log.Info("account_link.user.deleted", "deleted user", config.F("actor_user_id", actorID), config.F("target_user_id", targetID), config.F("account_count", len(user.Accounts)), config.F("status", "ok"))
	return UserDeletionDescriptor{ExternalIdentities: invalidation.ExternalIdentities, SessionIDs: invalidation.SessionIDs}, nil
}

func authenticatedAdminActor(data database.AccountLinkData, principal identity.Principal) (string, error) {
	if !principal.Valid() || !principal.Authenticated() {
		return "", fmt.Errorf("admin command requires an authenticated identity")
	}
	identifier, err := NormalizeIdentifier(principal.Gateway, principal.ExternalID)
	if err != nil {
		return "", err
	}
	actorID, ok := data.AccountIndex[accountKey(principal.Gateway, identifier)]
	if !ok {
		return "", ErrPrincipalMismatch
	}
	actor, ok := data.Users[actorID]
	if !ok {
		return "", ErrPrincipalMismatch
	}
	if !actor.IsAdmin {
		return "", fmt.Errorf("canonical user %q is not an admin", actorID)
	}
	return actorID, nil
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
			return fmt.Errorf("linked account not found")
		} else if err != nil {
			return fmt.Errorf("resolve disconnect target: %w", err)
		}
		if targetOwner != canonicalUserID {
			return fmt.Errorf("linked account not found")
		}

		var accountCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM linked_accounts WHERE canonical_user_id = ?`, canonicalUserID).Scan(&accountCount); err != nil {
			return fmt.Errorf("count linked accounts: %w", err)
		}
		if accountCount <= 1 {
			return fmt.Errorf("cannot disconnect the last linked account")
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
	s.log.Info("account_link.account.disconnected", "disconnected account", config.F("account", descriptor.ExternalIdentities[0]), config.F("target_user_id", canonicalUserID), config.F("status", "ok"))
	return descriptor, nil
}

func (s *Service) loadLocked() (database.AccountLinkData, error) {
	if err := s.Initialize(); err != nil {
		return database.AccountLinkData{}, err
	}
	return s.db.LoadAccountLinks()
}

func (s *Service) saveLocked(data database.AccountLinkData) error {
	if err := s.Initialize(); err != nil {
		return err
	}
	return s.db.ReplaceAccountLinks(data)
}

func (s *Service) initialize() error {
	db, err := database.Open(s.path, s.log)
	if err != nil {
		return err
	}
	s.db = db
	return nil
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
