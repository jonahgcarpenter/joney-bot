package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
)

// Store persists MCP server metadata and encrypted connection settings.
type Store struct {
	db       *database.DB
	crypto   *cryptoBox
	resolver hostnameResolver
	log      *config.Logger
}

type userMergeConflictError struct{ name string }

func (e userMergeConflictError) Error() string {
	return fmt.Sprintf("MCP server name %q conflicts while merging users", e.name)
}

func (userMergeConflictError) UserMergeConflict() {}

// NewStore opens the shared SQLite database and prepares MCP config encryption.
func NewStore(path string, encryptionKey string, log *config.Logger) (*Store, error) {
	box, err := newCryptoBox(encryptionKey)
	if err != nil {
		return nil, err
	}
	db, err := database.Open(path, log)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, crypto: box, log: log}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) SetResolverForTest(resolver hostnameResolver) {
	s.resolver = resolver
}

// Save validates, encrypts, and upserts one MCP server config.
func (s *Store) Save(ctx context.Context, cfg ServerConfig) (ServerConfig, error) {
	if s == nil || s.db == nil {
		return ServerConfig{}, fmt.Errorf("MCP store is not initialized")
	}
	cfg.Scope = strings.TrimSpace(cfg.Scope)
	cfg.OwnerUserID = strings.TrimSpace(cfg.OwnerUserID)
	cfg.Name = strings.TrimSpace(strings.ToLower(cfg.Name))
	description, err := normalizeServerDescription(cfg.Description)
	if err != nil {
		return ServerConfig{}, err
	}
	cfg.Description = description
	cfg.Transport = strings.TrimSpace(strings.ToLower(cfg.Transport))
	if cfg.Transport == "" {
		cfg.Transport = TransportStreamableHTTP
	}
	if err := validateScope(cfg.Scope, cfg.OwnerUserID); err != nil {
		return ServerConfig{}, err
	}
	if err := validateServerName(cfg.Name); err != nil {
		return ServerConfig{}, err
	}
	if err := validateTransport(cfg.Transport); err != nil {
		return ServerConfig{}, err
	}
	_, err = parseAndValidateURL(ctx, cfg.URL, s.resolver)
	if err != nil {
		return ServerConfig{}, err
	}
	if cfg.Scope == ScopeUser {
		if _, ok, err := s.Get(ctx, ScopeGlobal, "", cfg.Name); err != nil {
			return ServerConfig{}, err
		} else if ok {
			return ServerConfig{}, fmt.Errorf("server name %q collides with a global MCP server", cfg.Name)
		}
	}
	if existing, ok, err := s.Get(ctx, cfg.Scope, cfg.OwnerUserID, cfg.Name); err != nil {
		return ServerConfig{}, err
	} else if ok {
		cfg.ID = existing.ID
	}
	if cfg.ID == "" {
		cfg.ID = newConfigID()
	}
	urlCiphertext, err := s.crypto.encrypt(strings.TrimSpace(cfg.URL), fieldAAD(cfg.Scope, cfg.OwnerUserID, cfg.Name, "url"))
	if err != nil {
		return ServerConfig{}, err
	}
	headersData, err := json.Marshal(cfg.Headers)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("marshal MCP headers: %w", err)
	}
	headersCiphertext, err := s.crypto.encrypt(string(headersData), fieldAAD(cfg.Scope, cfg.OwnerUserID, cfg.Name, "headers"))
	if err != nil {
		return ServerConfig{}, err
	}
	result, err := s.db.SQL().ExecContext(ctx, `
INSERT INTO mcp_servers (id, scope, owner_user_id, name, description, transport, url_ciphertext, headers_ciphertext, enabled)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE ? != 'user' OR EXISTS (SELECT 1 FROM account_users WHERE canonical_user_id = ?)
ON CONFLICT(id) DO UPDATE SET
	scope = excluded.scope,
	owner_user_id = excluded.owner_user_id,
	name = excluded.name,
	description = excluded.description,
	transport = excluded.transport,
	url_ciphertext = excluded.url_ciphertext,
	headers_ciphertext = excluded.headers_ciphertext,
	enabled = excluded.enabled
`, cfg.ID, cfg.Scope, nullableOwner(cfg.OwnerUserID), cfg.Name, cfg.Description, cfg.Transport, urlCiphertext, headersCiphertext, boolToInt(cfg.Enabled), cfg.Scope, cfg.OwnerUserID)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("save MCP server config: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return ServerConfig{}, fmt.Errorf("check saved MCP server config: %w", err)
	}
	if rowsAffected == 0 {
		return ServerConfig{}, fmt.Errorf("MCP server owner %q does not exist", cfg.OwnerUserID)
	}
	return cfg, nil
}

// MergeUsersTx transfers user-scoped MCP configs while preserving their IDs and configuration fields.
func (s *Store) MergeUsersTx(ctx context.Context, tx *sql.Tx, winnerID, loserID string) error {
	if s == nil || s.crypto == nil {
		return fmt.Errorf("MCP store is not initialized")
	}
	if tx == nil {
		return fmt.Errorf("MCP user merge transaction is required")
	}
	winnerID = strings.TrimSpace(winnerID)
	loserID = strings.TrimSpace(loserID)
	if winnerID == "" || loserID == "" || winnerID == loserID {
		return fmt.Errorf("MCP user merge requires distinct non-empty user IDs")
	}
	var winnerExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM account_users WHERE canonical_user_id = ?)`, winnerID).Scan(&winnerExists); err != nil {
		return fmt.Errorf("check MCP user merge winner: %w", err)
	}
	if !winnerExists {
		return fmt.Errorf("MCP server owner %q does not exist", winnerID)
	}

	var conflictName string
	err := tx.QueryRowContext(ctx, `
SELECT loser.name
FROM mcp_servers AS loser
JOIN mcp_servers AS winner ON winner.scope = 'user' AND winner.owner_user_id = ? AND winner.name = loser.name
WHERE loser.scope = 'user' AND loser.owner_user_id = ?
LIMIT 1
`, winnerID, loserID).Scan(&conflictName)
	if err == nil {
		return userMergeConflictError{name: conflictName}
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("preflight MCP user merge conflicts: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, name, url_ciphertext, headers_ciphertext
FROM mcp_servers
WHERE scope = 'user' AND owner_user_id = ?
ORDER BY id
`, loserID)
	if err != nil {
		return fmt.Errorf("load MCP configs for user merge: %w", err)
	}
	type migratedCiphertext struct {
		id, url, headers string
	}
	var migrated []migratedCiphertext
	for rows.Next() {
		var id, name, urlCiphertext, headersCiphertext string
		if err := rows.Scan(&id, &name, &urlCiphertext, &headersCiphertext); err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("scan MCP config for user merge: %w", err)
		}
		urlText, err := s.crypto.decrypt(urlCiphertext, fieldAAD(ScopeUser, loserID, name, "url"))
		if err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("decrypt MCP URL for user merge: %w", err)
		}
		headersText, err := s.crypto.decrypt(headersCiphertext, fieldAAD(ScopeUser, loserID, name, "headers"))
		if err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("decrypt MCP headers for user merge: %w", err)
		}
		urlCiphertext, err = s.crypto.encrypt(urlText, fieldAAD(ScopeUser, winnerID, name, "url"))
		if err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("encrypt MCP URL for user merge: %w", err)
		}
		headersCiphertext, err = s.crypto.encrypt(headersText, fieldAAD(ScopeUser, winnerID, name, "headers"))
		if err != nil {
			rows.Close() // nolint:errcheck
			return fmt.Errorf("encrypt MCP headers for user merge: %w", err)
		}
		migrated = append(migrated, migratedCiphertext{id: id, url: urlCiphertext, headers: headersCiphertext})
	}
	if err := rows.Err(); err != nil {
		rows.Close() // nolint:errcheck
		return fmt.Errorf("load MCP configs for user merge: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close MCP configs for user merge: %w", err)
	}

	for _, cfg := range migrated {
		result, err := tx.ExecContext(ctx, `
UPDATE mcp_servers
SET owner_user_id = ?, url_ciphertext = ?, headers_ciphertext = ?
WHERE id = ? AND scope = 'user' AND owner_user_id = ?
`, winnerID, cfg.url, cfg.headers, cfg.id, loserID)
		if err != nil {
			return fmt.Errorf("transfer MCP config %q: %w", cfg.id, err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check transferred MCP config %q: %w", cfg.id, err)
		}
		if rowsAffected != 1 {
			return fmt.Errorf("MCP config %q changed during user merge", cfg.id)
		}
	}
	return nil
}

// DeleteUserTx removes user-scoped MCP configs in a caller-owned transaction.
func (s *Store) DeleteUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	if s == nil || s.crypto == nil {
		return fmt.Errorf("MCP store is not initialized")
	}
	if tx == nil {
		return fmt.Errorf("MCP user deletion transaction is required")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_servers WHERE scope = 'user' AND owner_user_id = ?`, strings.TrimSpace(userID)); err != nil {
		return fmt.Errorf("delete user MCP configs: %w", err)
	}
	return nil
}

func (s *Store) ListForUser(ctx context.Context, userID string) ([]ServerConfig, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
SELECT id, scope, owner_user_id, name, description, transport, url_ciphertext, headers_ciphertext, enabled
FROM mcp_servers
WHERE scope = 'global' OR (scope = 'user' AND owner_user_id = ?)
ORDER BY scope, name
`, strings.TrimSpace(userID))
	if err != nil {
		return nil, fmt.Errorf("list MCP server configs: %w", err)
	}
	defer rows.Close()
	return s.scanConfigs(rows)
}

func (s *Store) Get(ctx context.Context, scope, ownerUserID, name string) (ServerConfig, bool, error) {
	row := s.db.SQL().QueryRowContext(ctx, `
SELECT id, scope, owner_user_id, name, description, transport, url_ciphertext, headers_ciphertext, enabled
FROM mcp_servers
WHERE scope = ? AND COALESCE(owner_user_id, '') = ? AND name = ?
`, scope, strings.TrimSpace(ownerUserID), strings.TrimSpace(strings.ToLower(name)))
	stored, err := scanStored(row)
	if err == sql.ErrNoRows {
		return ServerConfig{}, false, nil
	}
	if err != nil {
		return ServerConfig{}, false, err
	}
	cfg, err := s.decrypt(stored)
	return cfg, err == nil, err
}

func (s *Store) Delete(ctx context.Context, scope, ownerUserID, name string) error {
	_, err := s.db.SQL().ExecContext(ctx, `DELETE FROM mcp_servers WHERE scope = ? AND COALESCE(owner_user_id, '') = ? AND name = ?`, scope, strings.TrimSpace(ownerUserID), strings.TrimSpace(strings.ToLower(name)))
	if err != nil {
		return fmt.Errorf("delete MCP server config: %w", err)
	}
	return nil
}

func (s *Store) SetEnabled(ctx context.Context, scope, ownerUserID, name string, enabled bool) error {
	_, err := s.db.SQL().ExecContext(ctx, `UPDATE mcp_servers SET enabled = ? WHERE scope = ? AND COALESCE(owner_user_id, '') = ? AND name = ?`, boolToInt(enabled), scope, strings.TrimSpace(ownerUserID), strings.TrimSpace(strings.ToLower(name)))
	if err != nil {
		return fmt.Errorf("update MCP server enabled state: %w", err)
	}
	return nil
}

func (s *Store) scanConfigs(rows *sql.Rows) ([]ServerConfig, error) {
	var out []ServerConfig
	for rows.Next() {
		stored, err := scanStored(rows)
		if err != nil {
			return nil, err
		}
		cfg, err := s.decrypt(stored)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan MCP server configs: %w", err)
	}
	return out, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanStored(row rowScanner) (storedServerConfig, error) {
	var stored storedServerConfig
	var owner sql.NullString
	var enabled int
	if err := row.Scan(&stored.ID, &stored.Scope, &owner, &stored.Name, &stored.Description, &stored.Transport, &stored.URLCiphertext, &stored.HeadersCiphertext, &enabled); err != nil {
		return storedServerConfig{}, err
	}
	stored.OwnerUserID = owner.String
	stored.Enabled = enabled != 0
	return stored, nil
}

func (s *Store) decrypt(stored storedServerConfig) (ServerConfig, error) {
	urlText, err := s.crypto.decrypt(stored.URLCiphertext, fieldAAD(stored.Scope, stored.OwnerUserID, stored.Name, "url"))
	if err != nil {
		return ServerConfig{}, err
	}
	headersText, err := s.crypto.decrypt(stored.HeadersCiphertext, fieldAAD(stored.Scope, stored.OwnerUserID, stored.Name, "headers"))
	if err != nil {
		return ServerConfig{}, err
	}
	headers := map[string]string{}
	if strings.TrimSpace(headersText) != "" && strings.TrimSpace(headersText) != "null" {
		if err := json.Unmarshal([]byte(headersText), &headers); err != nil {
			return ServerConfig{}, fmt.Errorf("unmarshal MCP headers: %w", err)
		}
	}
	return ServerConfig{ID: stored.ID, Scope: stored.Scope, OwnerUserID: stored.OwnerUserID, Name: stored.Name, Description: stored.Description, Transport: stored.Transport, URL: urlText, Headers: headers, Enabled: stored.Enabled}, nil
}

func nullableOwner(owner string) any {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil
	}
	return owner
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func newConfigID() string {
	return fmt.Sprintf("mcp_%d", time.Now().UTC().UnixNano())
}
