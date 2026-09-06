package indexing

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func TestTranscriptSchemaUpgradeRecoversInterruptedOldBuild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "source", "caller")
	profile, err := store.ResolveSessionProfile(ctx, "source", "source-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := store.ResolveSessionProfile(ctx, "caller", "caller-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{
		UserID: "source", SessionID: "source-session", Generation: profile.Generation,
		UserText: "privateenrichment", PublicUserText: "publicmarker", AssistantText: "publicanswer",
		GroupGateway: "discord", GroupChatID: "chat", TTL: time.Hour,
		Pressure: memory.SessionPromptPressure{Tokens: 1, Limit: 100, Version: "test"},
	})
	if err != nil || turn.ID == 0 {
		t.Fatalf("append turn=%+v err=%v", turn, err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "source", turn.ID); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Reconstruct both physical schema-2 tables, not just old revision metadata.
	var oldLive, interrupted memory.DerivedIndexRevision
	for _, target := range []*memory.DerivedIndexRevision{&oldLive, &interrupted} {
		*target, err = store.CreateIndexRevision(ctx, memory.IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		state := "building"
		if target == &oldLive {
			state = "live"
		}
		if _, err := db.SQL().Exec(`DROP TABLE `+target.TableName+`;
CREATE VIRTUAL TABLE `+target.TableName+` USING fts5(canonical_user_id, session_id, session_generation, user_text, assistant_text, tool_text);
UPDATE derived_index_revisions SET schema_version = 2, state = ? WHERE id = ?`, state, target.ID); err != nil {
			t.Fatal(err)
		}
		target.SchemaVersion, target.State = 2, state
	}
	record, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "source")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, oldLive, record); err != nil {
		t.Fatal(err)
	}
	assertOldServing := func() {
		t.Helper()
		live, err := store.LiveIndexRevision(ctx, memory.IndexKindTranscriptFTS)
		if err != nil || live.ID != oldLive.ID || live.SchemaVersion != 2 {
			t.Fatalf("old serving revision=%+v err=%v", live, err)
		}
		private, err := store.SearchTranscript(ctx, "source", "source-session", profile.Generation, "privateenrichment", 5)
		if err != nil || len(private) != 1 || private[0].TurnID != turn.ID {
			t.Fatalf("old private search=%+v err=%v", private, err)
		}
		_, err = store.SearchGroupTranscript(ctx, "caller", "caller-session", caller.Generation, "discord", "chat", "publicmarker", 5)
		if !errors.Is(err, memory.ErrTranscriptSearchUnavailable) {
			t.Fatalf("group search before v3 publication: %v", err)
		}
	}
	assertOldServing()

	service := NewService(store, nil, nil, "", config.NewLogger(config.LevelError))
	// The resumed old build fails schema validation without displacing old live.
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertOldServing()
	var state string
	if err := db.SQL().QueryRow(`SELECT state FROM derived_index_revisions WHERE id = ?`, interrupted.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("interrupted revision state=%q err=%v", state, err)
	}
	if _, err := store.BuildingIndexRevision(ctx, memory.IndexKindTranscriptFTS); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("obsolete build still blocks replacement: %v", err)
	}

	// A subsequent real cycle creates, populates, validates, and publishes v3.
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(ctx, memory.IndexKindTranscriptFTS)
	if err != nil || live.SchemaVersion != 3 || live.Revision <= interrupted.Revision || live.TableName == oldLive.TableName || live.TableName == interrupted.TableName || live.ExpectedCount != 1 || live.IndexedCount != 1 {
		t.Fatalf("replacement revision=%+v err=%v", live, err)
	}
	if err := db.SQL().QueryRow(`SELECT state FROM derived_index_revisions WHERE id = ?`, oldLive.ID).Scan(&state); err != nil || state != "retired" {
		t.Fatalf("old live state=%q err=%v", state, err)
	}
	group, err := store.SearchGroupTranscript(ctx, "caller", "caller-session", caller.Generation, "discord", "chat", "publicmarker", 5)
	if err != nil || len(group) != 1 || group[0].TurnID != turn.ID || group[0].CanonicalUserID != "source" || group[0].SessionID != "" || len(group[0].Records) != 2 || group[0].Records[0].Content != "publicmarker" || group[0].Records[1].Content != "publicanswer" {
		t.Fatalf("rebuilt public search=%+v err=%v", group, err)
	}
	group, err = store.SearchGroupTranscript(ctx, "caller", "caller-session", caller.Generation, "discord", "chat", "privateenrichment", 5)
	if err != nil || len(group) != 0 {
		t.Fatalf("private enrichment matched public projection=%+v err=%v", group, err)
	}
	private, err := store.SearchTranscript(ctx, "source", "source-session", profile.Generation, "privateenrichment", 5)
	if err != nil || len(private) != 1 || private[0].TurnID != turn.ID {
		t.Fatalf("rebuilt private search=%+v err=%v", private, err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.LiveIndexRevision(ctx, memory.IndexKindTranscriptFTS)
	if err != nil || unchanged.ID != live.ID {
		t.Fatalf("healthy v3 unnecessarily replaced=%+v err=%v", unchanged, err)
	}
}
