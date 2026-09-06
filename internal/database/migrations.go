package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

//go:embed migrations/*.sql
var permanentMigrationFiles embed.FS

var permanentMigrationName = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.sql$`)

type schemaMigration struct {
	version int
	name    string
	sql     string
	major   int
	minor   int
	patch   int
}

func orderedMigrations() []schemaMigration {
	migrations, err := discoverPermanentMigrations(permanentMigrationFiles)
	if err != nil {
		panic(err)
	}
	return migrations
}

func discoverPermanentMigrations(files fs.FS) ([]schemaMigration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read permanent schema migrations: %w", err)
	}
	migrations := make([]schemaMigration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("permanent migration directory contains subdirectory %q", entry.Name())
		}
		matches := permanentMigrationName.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid permanent migration filename %q", entry.Name())
		}
		major, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("invalid permanent migration major version in %q: %w", entry.Name(), err)
		}
		minor, err := strconv.Atoi(matches[2])
		if err != nil {
			return nil, fmt.Errorf("invalid permanent migration minor version in %q: %w", entry.Name(), err)
		}
		patch, err := strconv.Atoi(matches[3])
		if err != nil {
			return nil, fmt.Errorf("invalid permanent migration patch version in %q: %w", entry.Name(), err)
		}
		definition, err := fs.ReadFile(files, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read permanent migration %q: %w", entry.Name(), err)
		}
		if strings.TrimSpace(string(definition)) == "" {
			return nil, fmt.Errorf("permanent migration %q is empty", entry.Name())
		}
		migrations = append(migrations, schemaMigration{
			name:  strings.TrimSuffix(entry.Name(), ".sql"),
			sql:   string(definition),
			major: major,
			minor: minor,
			patch: patch,
		})
	}
	sort.Slice(migrations, func(i, j int) bool {
		left, right := migrations[i], migrations[j]
		if left.major != right.major {
			return left.major < right.major
		}
		if left.minor != right.minor {
			return left.minor < right.minor
		}
		return left.patch < right.patch
	})
	for i := range migrations {
		migrations[i].version = i + 1
	}
	if err := validateMigrationRegistry(migrations); err != nil {
		return nil, err
	}
	return migrations, nil
}

func migrationChecksum(m schemaMigration) string {
	sum := sha256.Sum256([]byte(m.name + "\n" + m.sql))
	return hex.EncodeToString(sum[:])
}

func (d *DB) runSchemaMigrations(ctx context.Context, registry []schemaMigration) (err error) {
	started := time.Now()
	appliedCount := 0
	committed := false
	if err := validateMigrationRegistry(registry); err != nil {
		return err
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire schema migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for schema migrations: %w", err)
	}
	defer func() {
		_, enableErr := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`)
		if enableErr != nil {
			err = errors.Join(err, fmt.Errorf("restore foreign keys after schema migrations: %w", enableErr))
		}
		if err == nil && committed && appliedCount < len(registry) && d.log != nil {
			prior := "empty"
			if appliedCount > 0 {
				prior = registry[appliedCount-1].name
			}
			d.log.Server("database").Info("database.migrations.applied", "schema migrations committed and foreign keys restored", config.F("applied_count", len(registry)-appliedCount), config.F("prior_release", prior), config.F("target_release", registry[len(registry)-1].name), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", "ok"))
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin immediate schema migration transaction: %w", err)
	}
	defer func() {
		if !committed {
			if _, rollbackErr := conn.ExecContext(context.Background(), `ROLLBACK`); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback schema migrations: %w", rollbackErr))
			}
		}
	}()

	var ledgerExists, schemaObjectCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migration_versions'`).Scan(&ledgerExists); err != nil {
		return fmt.Errorf("inspect schema migration ledger: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&schemaObjectCount); err != nil {
		return fmt.Errorf("inspect existing database schema: %w", err)
	}

	if ledgerExists == 0 && schemaObjectCount != 0 {
		return fmt.Errorf("unsupported nonempty database without a permanent migration ledger")
	}

	if _, err := conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migration_versions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL CHECK (length(checksum) = 64),
	applied_at TEXT NOT NULL
)`); err != nil {
		return fmt.Errorf("initialize ordered schema migration ledger: %w", err)
	}

	appliedCount, err = validateAppliedMigrationPrefix(ctx, conn, registry)
	if err != nil {
		return err
	}
	if ledgerExists != 0 && appliedCount == 0 && schemaObjectCount != 1 {
		return fmt.Errorf("empty schema migration ledger accompanies an unknown nonempty schema")
	}
	for _, migration := range registry[appliedCount:] {
		if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
			return fmt.Errorf("apply schema migration %d %q: %w", migration.version, migration.name, err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migration_versions (version, name, checksum, applied_at) VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`, migration.version, migration.name, migrationChecksum(migration)); err != nil {
			return fmt.Errorf("record schema migration %d %q: %w", migration.version, migration.name, err)
		}
	}
	if err := foreignKeyCheck(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit schema migrations: %w", err)
	}
	committed = true
	return nil
}

func validateAppliedMigrationPrefix(ctx context.Context, conn *sql.Conn, registry []schemaMigration) (int, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migration_versions ORDER BY version`)
	if err != nil {
		return 0, fmt.Errorf("read applied schema migrations: %w", err)
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return 0, fmt.Errorf("scan applied schema migration: %w", err)
		}
		if version != applied+1 {
			return 0, fmt.Errorf("schema migration ledger is not contiguous: expected version %d, found %d", applied+1, version)
		}
		if applied >= len(registry) {
			return 0, fmt.Errorf("database has unknown schema migration version %d", version)
		}
		migration := registry[applied]
		expected := migrationChecksum(migration)
		if name != migration.name || checksum != expected {
			return 0, fmt.Errorf("schema migration checksum drift at version %d: database has %q/%q, registry has %q/%q", version, name, checksum, migration.name, expected)
		}
		applied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read applied schema migrations: %w", err)
	}
	return applied, nil
}

func validateMigrationRegistry(registry []schemaMigration) error {
	if len(registry) == 0 || registry[0].name != "v4.0.0" {
		return errors.New("permanent schema migration history must start at v4.0.0")
	}
	for i, migration := range registry {
		if migration.version != i+1 || strings.TrimSpace(migration.sql) == "" {
			return fmt.Errorf("invalid schema migration registry entry at version %d", migration.version)
		}
		if i > 0 {
			previous := registry[i-1]
			if migration.major < previous.major ||
				(migration.major == previous.major && migration.minor < previous.minor) ||
				(migration.major == previous.major && migration.minor == previous.minor && migration.patch <= previous.patch) {
				return fmt.Errorf("permanent schema migrations are not in strict semantic order at %q", migration.name)
			}
		}
	}
	return nil
}

func foreignKeyCheck(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check schema migration foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID sql.NullInt64
		var parent string
		var foreignKey int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
			return fmt.Errorf("read schema migration foreign key violation: %w", err)
		}
		return fmt.Errorf("schema migration foreign key violation in %s row %v referencing %s", table, rowID, parent)
	}
	return rows.Err()
}
