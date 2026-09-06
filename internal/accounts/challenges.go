package accounts

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const (
	challengeTTL       = 10 * time.Minute
	challengeRetention = 24 * time.Hour
)

var (
	ErrChallengeInvalid   = errors.New("connection code is invalid, expired, or already used")
	ErrChallengeSameActor = errors.New("connection code must be confirmed from another account")
	ErrGatewayConflict    = errors.New("profiles contain different accounts for the same gateway")
	ErrMCPConflict        = errors.New("profiles contain conflicting MCP server names")
	ErrLinkBanned         = errors.New("banned profiles cannot be connected")
	ErrPrincipalMismatch  = errors.New("request identity no longer owns its canonical user")
)

var challengeEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

type userMergeConflict interface {
	error
	UserMergeConflict()
}

type storedChallenge struct {
	ID                  string
	InitiatorUserID     string
	InitiatorGateway    string
	InitiatorIdentifier string
	ConsumedAt          sql.NullString
	ConsumedGateway     sql.NullString
	ConsumedIdentifier  sql.NullString
	ResultUserID        sql.NullString
}

// ResolveChallengeFenceTargets resolves the stored and current owners involved
// in a possible confirmation without consuming the challenge. Confirmation
// re-resolves these identities transactionally after the fences are held.
func (s *Service) ResolveChallengeFenceTargets(ctx context.Context, principal identity.Principal, code string) ([]string, error) {
	if err := validateAuthenticatedPrincipal(principal); err != nil {
		return nil, err
	}
	codeHash, err := hashChallengeCode(code)
	if err != nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureInitializedLocked(); err != nil {
		return nil, err
	}

	ids := []string{principal.CanonicalUserID}
	confirmingOwner, _, err := accountOwnerDB(ctx, s.db.SQL(), principal.Gateway, principal.ExternalID)
	if err != nil && !errors.Is(err, ErrPrincipalMismatch) {
		return nil, err
	}
	if confirmingOwner != "" {
		ids = append(ids, confirmingOwner)
	}
	var initiatorID, gateway, identifier string
	err = s.db.SQL().QueryRowContext(ctx, `SELECT initiator_user_id, initiator_gateway, initiator_identifier FROM account_link_challenges WHERE code_hash = ?`, codeHash).Scan(&initiatorID, &gateway, &identifier)
	if errors.Is(err, sql.ErrNoRows) {
		return ids, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve account-link challenge fences: %w", err)
	}
	ids = append(ids, initiatorID)
	initiatorOwner, _, err := accountOwnerDB(ctx, s.db.SQL(), gateway, identifier)
	if err != nil && !errors.Is(err, ErrPrincipalMismatch) {
		return nil, err
	}
	if initiatorOwner != "" {
		ids = append(ids, initiatorOwner)
	}
	return ids, nil
}

// CreateChallenge creates a short-lived code for another authenticated account to confirm.
func (s *Service) CreateChallenge(ctx context.Context, principal identity.Principal, requestID string) (LinkChallenge, error) {
	if err := validateAuthenticatedPrincipal(principal); err != nil {
		return LinkChallenge{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureInitializedLocked(); err != nil {
		return LinkChallenge{}, err
	}

	code, codeHash, challengeID, err := s.newChallengeSecret()
	if err != nil {
		return LinkChallenge{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(challengeTTL)
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		owner, banned, err := principalOwnerTx(ctx, tx, principal)
		if err != nil {
			return err
		}
		if owner != principal.CanonicalUserID {
			return ErrPrincipalMismatch
		}
		if banned {
			return ErrLinkBanned
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_link_challenges WHERE expires_at <= ?`, formatChallengeTime(now.Add(-challengeRetention))); err != nil {
			return fmt.Errorf("prune account-link challenges: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE account_link_challenges
SET invalidated_at = ?
WHERE initiator_user_id = ? AND consumed_at IS NULL AND invalidated_at IS NULL
`, formatChallengeTime(now), principal.CanonicalUserID); err != nil {
			return fmt.Errorf("invalidate prior account-link challenge: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
	INSERT INTO account_link_challenges (
	id, code_hash, initiator_user_id, initiator_gateway, initiator_identifier,
	expires_at
) VALUES (?, ?, ?, ?, ?, ?)
`, challengeID, codeHash, principal.CanonicalUserID, principal.Gateway, principal.ExternalID, formatChallengeTime(expiresAt))
		if err != nil {
			return fmt.Errorf("create account-link challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return LinkChallenge{}, err
	}
	s.log.With(requestctx.LogFields(ctx)...).Info("account_link.challenge.created", "created account-link challenge", config.F("request_id", requestID), config.F("challenge_id", challengeID), config.F("actor_user_id", principal.CanonicalUserID), config.F("gateway", principal.Gateway), config.F("status", "ok"))
	return LinkChallenge{ID: challengeID, Code: code, ExpiresAt: expiresAt}, nil
}

// CancelChallenge invalidates the caller's active outgoing challenge.
func (s *Service) CancelChallenge(ctx context.Context, principal identity.Principal, requestID string) (bool, error) {
	if err := validateAuthenticatedPrincipal(principal); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureInitializedLocked(); err != nil {
		return false, err
	}
	now := s.now().UTC()
	var cancelled bool
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		owner, _, err := principalOwnerTx(ctx, tx, principal)
		if err != nil {
			return err
		}
		if owner != principal.CanonicalUserID {
			return ErrPrincipalMismatch
		}
		result, err := tx.ExecContext(ctx, `
UPDATE account_link_challenges
SET invalidated_at = ?
WHERE initiator_user_id = ? AND consumed_at IS NULL AND invalidated_at IS NULL AND expires_at > ?
`, formatChallengeTime(now), principal.CanonicalUserID, formatChallengeTime(now))
		if err != nil {
			return fmt.Errorf("cancel account-link challenge: %w", err)
		}
		count, err := result.RowsAffected()
		cancelled = count > 0
		return err
	})
	if err != nil {
		return false, err
	}
	if cancelled {
		s.log.With(requestctx.LogFields(ctx)...).Info("account_link.challenge.cancelled", "cancelled account-link challenge", config.F("request_id", requestID), config.F("actor_user_id", principal.CanonicalUserID), config.F("gateway", principal.Gateway), config.F("status", "ok"))
	}
	return cancelled, nil
}

// ConfirmChallenge verifies the second account and atomically merges its canonical user.
func (s *Service) ConfirmChallenge(ctx context.Context, principal identity.Principal, code, requestID string) (ConfirmResult, error) {
	if err := validateAuthenticatedPrincipal(principal); err != nil {
		return ConfirmResult{}, err
	}
	codeHash, err := hashChallengeCode(code)
	if err != nil {
		return ConfirmResult{}, ErrChallengeInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureInitializedLocked(); err != nil {
		return ConfirmResult{}, err
	}

	now := s.now().UTC()
	var result ConfirmResult
	var challengeID string
	var mergedLoser string
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		confirmingOwner, confirmingBanned, err := principalOwnerTx(ctx, tx, principal)
		if err != nil {
			return err
		}
		if confirmingOwner != principal.CanonicalUserID {
			return ErrPrincipalMismatch
		}

		challenge, claimed, err := claimChallengeTx(ctx, tx, codeHash, principal, now)
		if err != nil {
			return err
		}
		challengeID = challenge.ID
		if !claimed {
			if challenge.ConsumedAt.Valid && challenge.ConsumedGateway.String == principal.Gateway && constantTimeStringEqual(challenge.ConsumedIdentifier.String, principal.ExternalID) && challenge.ResultUserID.String != "" {
				result = ConfirmResult{CanonicalUserID: challenge.ResultUserID.String, Replayed: true}
				return nil
			}
			return ErrChallengeInvalid
		}
		if challenge.InitiatorGateway == principal.Gateway && constantTimeStringEqual(challenge.InitiatorIdentifier, principal.ExternalID) {
			return ErrChallengeSameActor
		}

		initiatorOwner, initiatorBanned, err := accountOwnerTx(ctx, tx, challenge.InitiatorGateway, challenge.InitiatorIdentifier)
		if err != nil && !errors.Is(err, ErrPrincipalMismatch) {
			return err
		}
		if err != nil || initiatorOwner != challenge.InitiatorUserID {
			return ErrChallengeInvalid
		}
		if initiatorBanned || confirmingBanned {
			return ErrLinkBanned
		}
		winnerID := challenge.InitiatorUserID
		loserID := confirmingOwner
		result.CanonicalUserID = winnerID
		if winnerID == loserID {
			if err := markVerifiedTx(ctx, tx, challenge, principal); err != nil {
				return err
			}
			result.AlreadyLinked = true
			return nil
		}

		if conflict, err := gatewayConflictTx(ctx, tx, winnerID, loserID); err != nil {
			return err
		} else if conflict {
			return ErrGatewayConflict
		}
		var winnerAdmin, loserAdmin bool
		if err := tx.QueryRowContext(ctx, `SELECT is_admin != 0 FROM account_users WHERE canonical_user_id = ?`, winnerID).Scan(&winnerAdmin); err != nil {
			return fmt.Errorf("read account-link winner: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT is_admin != 0 FROM account_users WHERE canonical_user_id = ?`, loserID).Scan(&loserAdmin); err != nil {
			return fmt.Errorf("read account-link source: %w", err)
		}
		if s.mcp != nil {
			if err := s.mcp.MergeUsersTx(ctx, tx, winnerID, loserID); err != nil {
				var conflict userMergeConflict
				if errors.As(err, &conflict) {
					return ErrMCPConflict
				}
				return err
			}
		} else {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_servers WHERE scope = 'user' AND owner_user_id = ?`, loserID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("MCP user merger is not configured")
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE linked_accounts SET canonical_user_id = ? WHERE canonical_user_id = ?`, winnerID, loserID); err != nil {
			return fmt.Errorf("move linked accounts: %w", err)
		}
		if err := markVerifiedTx(ctx, tx, challenge, principal); err != nil {
			return err
		}
		intro, err := speakerLineTx(ctx, tx, winnerID)
		if err != nil {
			return err
		}
		if err := s.memories.MergeUsersTx(ctx, tx, winnerID, loserID, intro); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_users SET is_admin = ? WHERE canonical_user_id = ?`, boolInt(winnerAdmin || loserAdmin), winnerID); err != nil {
			return fmt.Errorf("merge account authorization: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_link_challenges SET invalidated_at = ? WHERE initiator_user_id = ? AND id != ? AND consumed_at IS NULL AND invalidated_at IS NULL`, formatChallengeTime(now), loserID, challenge.ID); err != nil {
			return fmt.Errorf("invalidate merged user challenges: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_link_challenges SET initiator_user_id = CASE WHEN initiator_user_id = ? THEN ? ELSE initiator_user_id END, result_user_id = CASE WHEN result_user_id = ? THEN ? ELSE result_user_id END WHERE initiator_user_id = ? OR result_user_id = ?`, loserID, winnerID, loserID, winnerID, loserID, loserID); err != nil {
			return fmt.Errorf("rewrite merged account-link audit ownership: %w", err)
		}
		var remainingChallengeReferences int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_link_challenges WHERE initiator_user_id = ? OR result_user_id = ?`, loserID, loserID).Scan(&remainingChallengeReferences); err != nil {
			return fmt.Errorf("verify merged account-link audit ownership: %w", err)
		}
		if remainingChallengeReferences != 0 {
			return fmt.Errorf("verify merged account-link audit ownership: %d loser references remain", remainingChallengeReferences)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_users WHERE canonical_user_id = ?`, loserID); err != nil {
			return fmt.Errorf("delete merged canonical user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_link_challenges SET result_user_id = ? WHERE id = ?`, winnerID, challenge.ID); err != nil {
			return fmt.Errorf("record account-link result: %w", err)
		}
		result.Merged = true
		mergedLoser = loserID
		return nil
	})
	if err != nil {
		if reason, expected := PolicyReason(err); expected {
			s.log.With(requestctx.LogFields(ctx)...).Info("account_link.challenge.rejected", "rejected account-link challenge", config.F("request_id", requestID), config.F("actor_user_id", principal.CanonicalUserID), config.F("reason_code", reason), config.F("status", "rejected"))
		}
		return ConfirmResult{}, err
	}
	if mergedLoser != "" && s.mcp != nil {
		s.mcp.UserMergeCommitted(result.CanonicalUserID, mergedLoser)
	}
	event := "account_link.challenge.confirmed"
	if result.Replayed {
		event = "account_link.challenge.replayed"
	}
	s.log.With(requestctx.LogFields(ctx)...).Info(event, "confirmed account-link challenge", config.F("request_id", requestID), config.F("challenge_id", challengeID), config.F("actor_user_id", principal.CanonicalUserID), config.F("target_user_id", result.CanonicalUserID), config.F("gateway", principal.Gateway), config.F("status", "ok"))
	return result, nil
}

func validateAuthenticatedPrincipal(principal identity.Principal) error {
	if !principal.Valid() || !principal.Authenticated() {
		return fmt.Errorf("account linking requires an authenticated identity")
	}
	return nil
}

func (s *Service) newChallengeSecret() (string, string, string, error) {
	secret := make([]byte, 12)
	if _, err := io.ReadFull(s.random, secret); err != nil {
		return "", "", "", fmt.Errorf("generate account-link code: %w", err)
	}
	idBytes := make([]byte, 16)
	if _, err := io.ReadFull(s.random, idBytes); err != nil {
		return "", "", "", fmt.Errorf("generate account-link challenge id: %w", err)
	}
	payload := challengeEncoding.EncodeToString(secret)
	parts := make([]string, 0, 5)
	for i := 0; i < len(payload); i += 4 {
		parts = append(parts, payload[i:i+4])
	}
	code := "OSW-" + strings.Join(parts, "-")
	hash, _ := hashChallengeCode(code)
	return code, hash, "chl_" + hex.EncodeToString(idBytes), nil
}

func hashChallengeCode(code string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	normalized = strings.ReplaceAll(normalized, "-", "")
	if !strings.HasPrefix(normalized, "OSW") || len(normalized) != 23 {
		return "", ErrChallengeInvalid
	}
	if _, err := challengeEncoding.DecodeString(strings.TrimPrefix(normalized, "OSW")); err != nil {
		return "", ErrChallengeInvalid
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:]), nil
}

func claimChallengeTx(ctx context.Context, tx *sql.Tx, codeHash string, principal identity.Principal, now time.Time) (storedChallenge, bool, error) {
	row := tx.QueryRowContext(ctx, `
UPDATE account_link_challenges
SET consumed_at = ?, consumed_gateway = ?, consumed_identifier = ?, result_user_id = initiator_user_id
WHERE code_hash = ? AND consumed_at IS NULL AND invalidated_at IS NULL AND expires_at > ?
RETURNING id, initiator_user_id, initiator_gateway, initiator_identifier, consumed_at, consumed_gateway, consumed_identifier, result_user_id
`, formatChallengeTime(now), principal.Gateway, principal.ExternalID, codeHash, formatChallengeTime(now))
	challenge, err := scanStoredChallenge(row)
	if err == nil {
		return challenge, true, nil
	}
	if err != sql.ErrNoRows {
		return storedChallenge{}, false, fmt.Errorf("claim account-link challenge: %w", err)
	}
	challenge, err = scanStoredChallenge(tx.QueryRowContext(ctx, `
SELECT id, initiator_user_id, initiator_gateway, initiator_identifier, consumed_at, consumed_gateway, consumed_identifier, result_user_id
FROM account_link_challenges WHERE code_hash = ?
`, codeHash))
	if errors.Is(err, sql.ErrNoRows) {
		return storedChallenge{}, false, ErrChallengeInvalid
	}
	if err != nil {
		return storedChallenge{}, false, fmt.Errorf("read account-link challenge: %w", err)
	}
	return challenge, false, nil
}

func scanStoredChallenge(row interface{ Scan(...any) error }) (storedChallenge, error) {
	var c storedChallenge
	err := row.Scan(&c.ID, &c.InitiatorUserID, &c.InitiatorGateway, &c.InitiatorIdentifier, &c.ConsumedAt, &c.ConsumedGateway, &c.ConsumedIdentifier, &c.ResultUserID)
	return c, err
}

func principalOwnerTx(ctx context.Context, tx *sql.Tx, principal identity.Principal) (string, bool, error) {
	return accountOwnerTx(ctx, tx, principal.Gateway, principal.ExternalID)
}

func accountOwnerTx(ctx context.Context, tx *sql.Tx, gateway, identifier string) (string, bool, error) {
	return accountOwnerDB(ctx, tx, gateway, identifier)
}

type accountOwnerQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func accountOwnerDB(ctx context.Context, db accountOwnerQuerier, gateway, identifier string) (string, bool, error) {
	var owner string
	var banned bool
	err := db.QueryRowContext(ctx, `SELECT la.canonical_user_id, au.is_banned != 0 FROM linked_accounts la JOIN account_users au ON au.canonical_user_id = la.canonical_user_id WHERE la.gateway = ? AND la.identifier = ?`, gateway, identifier).Scan(&owner, &banned)
	if err == sql.ErrNoRows {
		return "", false, ErrPrincipalMismatch
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve account-link principal: %w", err)
	}
	return owner, banned, nil
}

func gatewayConflictTx(ctx context.Context, tx *sql.Tx, winnerID, loserID string) (bool, error) {
	var gateway string
	err := tx.QueryRowContext(ctx, `SELECT winner.gateway FROM linked_accounts winner JOIN linked_accounts loser ON loser.gateway = winner.gateway AND loser.identifier != winner.identifier WHERE winner.canonical_user_id = ? AND loser.canonical_user_id = ? LIMIT 1`, winnerID, loserID).Scan(&gateway)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check linked-account gateway conflicts: %w", err)
	}
	return true, nil
}

func markVerifiedTx(ctx context.Context, tx *sql.Tx, challenge storedChallenge, confirmer identity.Principal) error {
	result, err := tx.ExecContext(ctx, `UPDATE linked_accounts SET verified = 1 WHERE (gateway = ? AND identifier = ?) OR (gateway = ? AND identifier = ?)`, challenge.InitiatorGateway, challenge.InitiatorIdentifier, confirmer.Gateway, confirmer.ExternalID)
	if err != nil {
		return fmt.Errorf("verify connected accounts: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 2 {
		return fmt.Errorf("verify connected accounts: expected two accounts, updated %d", count)
	}
	return nil
}

func speakerLineTx(ctx context.Context, tx *sql.Tx, userID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT gateway, identifier, display_name, verified FROM linked_accounts WHERE canonical_user_id = ? ORDER BY gateway, identifier`, userID)
	if err != nil {
		return "", fmt.Errorf("read merged linked accounts: %w", err)
	}
	defer rows.Close()
	var accounts []database.LinkedAccount
	for rows.Next() {
		var account database.LinkedAccount
		var verified int
		if err := rows.Scan(&account.Gateway, &account.Identifier, &account.DisplayName, &verified); err != nil {
			return "", err
		}
		account.Verified = verified != 0
		accounts = append(accounts, account)
	}
	return FormatSpeakerLine(accounts), rows.Err()
}

func formatChallengeTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
