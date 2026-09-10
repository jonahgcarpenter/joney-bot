package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestImageVersionsMigrateLegacyAssets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	registry := orderedMigrations()
	if err := db.runSchemaMigrations(context.Background(), registry[:13]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO account_users(canonical_user_id) VALUES('user');
 INSERT INTO session_turns(id,canonical_user_id,session_id,user_text,assistant_text,created_at) VALUES(1,'user','session','synthetic','image','2026-09-01T00:00:00Z');
 INSERT INTO session_images(id,turn_id,ordinal,mime_type,data) VALUES('legacy',1,0,'image/png',X'0102');`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	broken := orderedMigrations()
	broken[13].sql += "\ninvalid statement;"
	if err := db.runSchemaMigrations(context.Background(), broken); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	if schemaSnapshot(t, raw) != before {
		t.Fatal("failed migration changed schema")
	}
	if err := db.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var id, parent string
	var version, high int
	var data []byte
	if err := reopened.SQL().QueryRow(`SELECT image_id,version,parent_source_image_id,version_highwater,data FROM session_images WHERE id='legacy'`).Scan(&id, &version, &parent, &high, &data); err != nil {
		t.Fatal(err)
	}
	if id != "legacy" || version != 1 || high != 1 || parent != "" || len(data) != 2 {
		t.Fatal("legacy image not preserved as version one")
	}
}
