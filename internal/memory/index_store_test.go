package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestCreateIndexRevisionValidatesWithoutPersistingRemovedMetadata(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	if _, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "wrong", "", 0); err == nil {
		t.Fatal("expected invalid provider to fail")
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"provider", "build_started_at", "last_successful_rebuild_at", "published_at", "completed_at"} {
		var count int
		if err := store.sql.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('derived_index_revisions') WHERE name = ?`, column).Scan(&count); err != nil || count != 0 {
			t.Fatalf("removed column %s count=%d err=%v", column, count, err)
		}
	}
}

func TestDeleteIndexRecordCannotCrossTenant(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-a", "user-b")
	memory, err := store.publishFixtureMemory(ctx, "user-b", memoryFixture{Scope: ScopeLongTerm, Statement: "Tenant B private memory"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user-b")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteIndexRecord(ctx, revision, memory.ID, "user-a"); err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = 'user-b'`, 1, memory.ID)
}

func TestStalePrivateIndexWriteCannotDeleteAnotherTenantRow(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-a", "user-b")
	memory, err := store.publishFixtureMemory(ctx, "user-a", memoryFixture{Scope: ScopeLongTerm, Statement: "stale tenant A fact"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO `+revision.TableName+`(rowid, canonical_user_id, statement, evidence) VALUES (?, 'user-b', 'tenant B row', '')`, memory.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET status = 'expired' WHERE id = ? AND canonical_user_id = 'user-a'`, memory.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); !errors.Is(err, ErrStaleIndexRecord) {
		t.Fatalf("stale write error=%v", err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = 'user-b'`, 1, memory.ID)
}

func TestStaleMemoryIndexWriteCannotRepublishAfterHardDelete(t *testing.T) {
	for _, mutation := range []string{"memory", "user"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
			defer store.Close() // nolint:errcheck
			seedAccountUsers(t, store, "user")
			memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Statement: "private stale content"})
			if err != nil {
				t.Fatal(err)
			}
			record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			switch mutation {
			case "memory":
				err = store.HardDeleteMemory(ctx, "user", memory.ID, now)
			case "user":
				tx, beginErr := store.sql.BeginTx(ctx, nil)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				_, err = store.DeleteUserTx(ctx, tx, "user", now)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); !errors.Is(err, ErrStaleIndexRecord) {
				t.Fatalf("write error = %v, want stale record", err)
			}
			assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, memory.ID)
		})
	}
}

func TestHardDeleteSerializesWithIndexRecheck(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Statement: "serialized private content"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	rechecked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.indexWriteHook = func(point string) {
		if point == "after_recheck" {
			once.Do(func() {
				close(rechecked)
				<-release
			})
		}
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- store.WriteMemoryIndexRecord(ctx, revision, record, nil) }()
	<-rechecked
	forgetDone := make(chan error, 1)
	go func() {
		forgetDone <- store.HardDeleteMemory(ctx, "user", memory.ID, time.Now().UTC())
	}()
	select {
	case err := <-forgetDone:
		t.Fatalf("hard-delete transaction did not wait for index transaction: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-forgetDone; err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, memory.ID)
}

func TestStaleTranscriptIndexWriteCannotRepublishDeletedSession(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.appendFixturePendingTurn(ctx, "session", "user", profile.Generation, "private transcript", "ack", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = created_at WHERE id = ?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	record, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetUserDataPreservingAccount(ctx, "user", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, revision, record); !errors.Is(err, ErrStaleIndexRecord) {
		t.Fatalf("write error = %v, want stale record", err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, turn.ID)
}

func TestDeliveryFailedTurnIsIneligibleForTranscriptIndex(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.appendFixturePendingTurn(ctx, "session", "user", profile.Generation, "private failed transcript", "ack", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = created_at WHERE id = ?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	record, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivery_failed_at = created_at WHERE id = ?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	records, err := store.DeliveredTranscriptIndexRecords(ctx, 0, 100)
	if err != nil || len(records) != 0 {
		t.Fatalf("delivery-failed bulk records=%+v err=%v", records, err)
	}
	if _, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "user"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delivery-failed point read error=%v", err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, revision, record); !errors.Is(err, ErrStaleIndexRecord) {
		t.Fatalf("delivery-failed stale write error=%v", err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName, 0)
}

func TestMaintenanceSkipsBuildingRevision(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO ` + revision.TableName + `(rowid, canonical_user_id, statement, evidence) VALUES (999, 'user', 'building', '')`); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if counts.RowsDeleted != 0 || counts.RevisionsDegraded != 0 {
		t.Fatalf("maintenance touched building revision: %+v", counts)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = 999`, 1)
	var state, code string
	if err := store.sql.QueryRow(`SELECT state, last_error_code FROM derived_index_revisions WHERE id = ?`, revision.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "building" || code != "" {
		t.Fatalf("building health changed: state=%q code=%q", state, code)
	}
}

func TestIndexMaintenanceRepairsOrphansAndDegradesMissingCoverage(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Statement: "canonical fact"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO ` + revision.TableName + `(rowid, canonical_user_id, statement, evidence) VALUES (9999, 'user', 'orphan', '')`); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RowsDeleted != 1 || counts.RevisionsDegraded != 0 {
		t.Fatalf("orphan repair counts=%+v err=%v", counts, err)
	}
	if _, err := store.LiveIndexRevision(ctx, IndexKindMemoryFTS); err != nil {
		t.Fatalf("orphan repair displaced live revision: %v", err)
	}
	if _, err := store.sql.Exec(`DELETE FROM `+revision.TableName+` WHERE rowid = ?`, memory.ID); err != nil {
		t.Fatal(err)
	}
	counts, err = store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RevisionsDegraded != 1 {
		t.Fatalf("coverage counts=%+v err=%v", counts, err)
	}
	var state, code string
	if err := store.sql.QueryRow(`SELECT state, last_error_code FROM derived_index_revisions WHERE id = ?`, revision.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "live" || code != "coverage_mismatch" {
		t.Fatalf("state=%q code=%q", state, code)
	}
}

func TestRevisionValidationRejectsStaleCanonicalContent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Statement: "original canonical fact"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET statement = 'updated canonical fact', updated_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(time.Second)), memory.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err == nil {
		t.Fatal("stale shadow revision was published")
	}
}

func TestRevisionPublicationRejectsMismatchedGeneratedTableIdentity(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	wrongTable := "derived_index_transcript_fts_r999"
	if _, err := store.sql.Exec(`CREATE VIRTUAL TABLE `+wrongTable+` USING fts5(canonical_user_id, statement, evidence); UPDATE derived_index_revisions SET table_name = ? WHERE id = ?`, wrongTable, revision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err == nil {
		t.Fatal("revision with mismatched generated table identity was published")
	}
}

func TestRetiredCleanupDoesNotDropMismatchedGeneratedTableIdentity(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	wrongTable := "derived_index_transcript_fts_r999"
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`CREATE VIRTUAL TABLE `+wrongTable+` USING fts5(canonical_user_id, statement, evidence); UPDATE derived_index_revisions SET table_name = ?, state = 'failed', updated_at = ? WHERE id = ?`, wrongTable, formatTime(now.Add(-2*time.Hour)), revision.ID); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, now, time.Hour, 100)
	if err != nil || counts.TablesDropped != 0 {
		t.Fatalf("corrupt identity cleanup counts=%+v err=%v", counts, err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, 1, wrongTable)
}

func TestMemoryVectorValidationRejectsCorruptedCanonicalMetadata(t *testing.T) {
	for _, corruption := range []struct {
		name   string
		column string
		value  string
	}{
		{name: "canonical version", column: "canonical_version", value: "stale"},
		{name: "scope", column: "scope", value: ScopeShortTerm},
		{name: "category", column: "category", value: "identity"},
	} {
		t.Run(corruption.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
			defer store.Close() // nolint:errcheck
			seedAccountUsers(t, store, "user")
			memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "canonical vector fact"})
			if err != nil {
				t.Fatal(err)
			}
			record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryVector, "llm_gateway", "model", 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.WriteMemoryIndexRecord(ctx, revision, record, []float64{1, 0}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.sql.Exec(`UPDATE `+revision.TableName+` SET `+corruption.column+` = ? WHERE rowid = ?`, corruption.value, memory.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err == nil {
				t.Fatalf("revision with corrupted %s was published", corruption.column)
			}
		})
	}
}

func TestVectorMaintenanceDeletesCorruptedCanonicalMetadata(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Category: "projects", Statement: "maintained vector fact"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryVector, "llm_gateway", "model", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, []float64{1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE `+revision.TableName+` SET scope = 'short_term' WHERE rowid = ?`, memory.ID); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RowsDeleted != 1 || counts.RevisionsDegraded != 1 {
		t.Fatalf("maintenance counts=%+v err=%v", counts, err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, memory.ID)
}

func TestGlobalVectorMaintenanceDeletesStaleCanonicalVersion(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	result, err := store.sql.Exec(`INSERT INTO global_memories(memory, memory_key, created_at) VALUES ('canonical global fact', 'canonical global fact', ?)`, formatTime(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	memoryID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.GlobalMemoryIndexRecordByID(ctx, memoryID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindGlobalMemoryVector, "llm_gateway", "model", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteGlobalMemoryIndexRecord(ctx, revision, record, []float64{1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE `+revision.TableName+` SET canonical_version = 'stale' WHERE rowid = ?`, memoryID); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RowsDeleted != 1 || counts.RevisionsDegraded != 1 {
		t.Fatalf("maintenance counts=%+v err=%v", counts, err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, memoryID)
}

func TestIndexMaintenanceNeverDropsLiveAndRetainsRetiredUntilDue(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	now := time.Now().UTC()
	first, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE derived_index_revisions SET updated_at = ? WHERE id = ?`, formatTime(now.Add(-48*time.Hour)), first.ID); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.MaintainDerivedIndexes(ctx, now, time.Hour, 100); err != nil || counts.TablesDropped != 0 {
		t.Fatalf("live cleanup counts=%+v err=%v", counts, err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, 1, first.TableName)

	second, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE derived_index_revisions SET updated_at = ? WHERE id = ?`, formatTime(now.Add(-30*time.Minute)), first.ID); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.MaintainDerivedIndexes(ctx, now, time.Hour, 100); err != nil || counts.TablesDropped != 0 {
		t.Fatalf("early retired cleanup counts=%+v err=%v", counts, err)
	}
	if _, err := store.sql.Exec(`UPDATE derived_index_revisions SET updated_at = ? WHERE id = ?`, formatTime(now.Add(-time.Hour)), first.ID); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.MaintainDerivedIndexes(ctx, now, time.Hour, 100); err != nil || counts.TablesDropped != 1 {
		t.Fatalf("due retired cleanup counts=%+v err=%v", counts, err)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, 0, first.TableName)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, 1, second.TableName)
	memory, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Statement: "delete after retired cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HardDeleteMemory(ctx, "user", memory.ID, now); err != nil {
		t.Fatalf("hard deletion consulted dropped revision table: %v", err)
	}
}

func TestPartialLiveRevisionRemainsQueryableWhileUnhealthy(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	first, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Statement: "Project surviving-index remains active."})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Statement: "Project missing-index remains active."})
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	live, err := store.LiveIndexRevision(ctx, IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`DELETE FROM `+live.TableName+` WHERE rowid = ?`, second.ID); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RevisionsDegraded != 1 {
		t.Fatalf("maintenance counts=%+v err=%v", counts, err)
	}
	stillLive, err := store.LiveIndexRevision(ctx, IndexKindMemoryFTS)
	if err != nil || stillLive.ID != live.ID {
		t.Fatalf("serving pointer changed: live=%+v err=%v", stillLive, err)
	}
	results, stats := store.Recall(ctx, "user", "surviving-index", RecallRequest{TopK: 2})
	if !errors.Is(stats.LexicalError, ErrDerivedIndexDegraded) || len(results) != 1 || results[0].Entry.ID != first.ID {
		t.Fatalf("partial live recall results=%+v stats=%+v", results, stats)
	}
}

func assertStoreCount(t *testing.T, db *sql.DB, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("row count=%d want=%d query=%s", got, want, query)
	}
}
