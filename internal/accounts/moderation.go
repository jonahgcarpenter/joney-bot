package accounts

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

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
	var invalidation memory.UserDeletionScope
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
