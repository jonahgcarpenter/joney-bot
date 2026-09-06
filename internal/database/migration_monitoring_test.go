package database

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestMigrationLogOnlyAfterChangedOpen(t *testing.T) {
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	path := filepath.Join(t.TempDir(), "monitoring.db")
	db, err := Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	var foreignKeys int
	if err := db.SQL().QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys=%d err=%v", foreignKeys, err)
	}
	if strings.Count(output.String(), `"event":"database.migrations.applied"`) != 1 || !strings.Contains(output.String(), `"applied_count":11`) {
		t.Fatalf("migration logs=%s", output.String())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	db, err = Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if output.Len() != 0 {
		t.Fatalf("unchanged open logged: %s", output.String())
	}
}
