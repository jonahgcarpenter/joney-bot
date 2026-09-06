package database

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// LinkedAccount records a single external gateway identity linked to a canonical user.
type LinkedAccount struct {
	Gateway     string `json:"gateway"`
	Identifier  string `json:"identifier"`
	DisplayName string `json:"display_name"`
	Verified    bool   `json:"verified"`
}

// AccountLinkData is the complete account-link dataset persisted in SQLite.
type AccountLinkData struct {
	Users        map[string]AccountUser `json:"users"`
	AccountIndex map[string]string      `json:"account_index"`
}

// LoadAccountLinks reads all canonical users and linked accounts.
func (d *DB) LoadAccountLinks() (AccountLinkData, error) {
	data := AccountLinkData{
		Users:        make(map[string]AccountUser),
		AccountIndex: make(map[string]string),
	}

	userRows, err := d.db.Query(`SELECT canonical_user_id, is_admin, is_banned, ban_reason FROM account_users`)
	if err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read account users: %w", err)
	}
	defer userRows.Close()

	for userRows.Next() {
		var canonicalID string
		var user AccountUser
		var isAdmin, isBanned int
		if err := userRows.Scan(&canonicalID, &isAdmin, &isBanned, &user.BanReason); err != nil {
			return AccountLinkData{}, fmt.Errorf("failed to scan account user: %w", err)
		}
		user.IsAdmin = isAdmin != 0
		user.IsBanned = isBanned != 0
		data.Users[canonicalID] = user
	}
	if err := userRows.Err(); err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read account users: %w", err)
	}

	accountRows, err := d.db.Query(`SELECT gateway, identifier, canonical_user_id, display_name, verified FROM linked_accounts ORDER BY gateway, identifier`)
	if err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read linked accounts: %w", err)
	}
	defer accountRows.Close()

	for accountRows.Next() {
		var account LinkedAccount
		var canonicalID string
		var verified int
		if err := accountRows.Scan(&account.Gateway, &account.Identifier, &canonicalID, &account.DisplayName, &verified); err != nil {
			return AccountLinkData{}, fmt.Errorf("failed to scan linked account: %w", err)
		}
		account.Verified = verified != 0

		user := data.Users[canonicalID]
		user.Accounts = append(user.Accounts, account)
		data.Users[canonicalID] = user
		data.AccountIndex[accountKey(account.Gateway, account.Identifier)] = canonicalID
	}
	if err := accountRows.Err(); err != nil {
		return AccountLinkData{}, fmt.Errorf("failed to read linked accounts: %w", err)
	}

	return data, nil
}

// ReplaceAccountLinks atomically replaces all account-link rows without
// deleting unchanged account_users. User memory rows reference account_users, so
// wholesale deletes would cascade and erase persistent memories.
func (d *DB) ReplaceAccountLinks(data AccountLinkData) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account link transaction: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck

	if _, err := tx.Exec(`DELETE FROM linked_accounts`); err != nil {
		return fmt.Errorf("failed to clear linked accounts: %w", err)
	}

	userStmt, err := tx.Prepare(`
INSERT INTO account_users (canonical_user_id, is_admin, is_banned, ban_reason)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET
	is_admin = excluded.is_admin,
	is_banned = excluded.is_banned,
	ban_reason = excluded.ban_reason
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account user insert: %w", err)
	}
	defer userStmt.Close()

	accountStmt, err := tx.Prepare(`INSERT INTO linked_accounts (gateway, identifier, canonical_user_id, display_name, verified) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("failed to prepare linked account insert: %w", err)
	}
	defer accountStmt.Close()

	userIDs := make([]string, 0, len(data.Users))
	for canonicalID := range data.Users {
		userIDs = append(userIDs, canonicalID)
	}
	sort.Strings(userIDs)

	for _, canonicalID := range userIDs {
		user := data.Users[canonicalID]
		if _, err := userStmt.Exec(canonicalID, boolToInt(user.IsAdmin), boolToInt(user.IsBanned), user.BanReason); err != nil {
			return fmt.Errorf("failed to save account user: %w", err)
		}
		for _, account := range user.Accounts {
			if _, err := accountStmt.Exec(account.Gateway, account.Identifier, canonicalID, account.DisplayName, boolToInt(account.Verified)); err != nil {
				return fmt.Errorf("failed to save linked account: %w", err)
			}
		}
	}
	if err := deleteStaleAccountUsers(tx, userIDs); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account link transaction: %w", err)
	}
	return nil
}

func deleteStaleAccountUsers(tx *sql.Tx, userIDs []string) error {
	if len(userIDs) == 0 {
		if _, err := tx.Exec(`DELETE FROM account_users`); err != nil {
			return fmt.Errorf("failed to remove stale account users: %w", err)
		}
		return nil
	}
	placeholders := make([]string, len(userIDs))
	args := make([]interface{}, len(userIDs))
	for i, userID := range userIDs {
		placeholders[i] = "?"
		args[i] = userID
	}
	query := `DELETE FROM account_users WHERE canonical_user_id NOT IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf("failed to remove stale account users: %w", err)
	}
	return nil
}

func accountKey(gateway, identifier string) string {
	return strings.ToLower(strings.TrimSpace(gateway)) + ":" + strings.TrimSpace(identifier)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
