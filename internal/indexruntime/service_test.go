package indexruntime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/testutil"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type lifecycleEmbedder struct {
	mu          sync.Mutex
	dimensions  map[string]int
	failContent bool
	failProbe   bool
	probeCount  int
	hook        func()
	hooked      bool
}

func TestMissingLiveTableTriggersShadowRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "user")
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "Rebuild missing physical table."}); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, "", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(old.TableName, "derived_index_memory_fts_r") {
		t.Fatalf("startup did not create a generated FTS revision: %+v", old)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`DROP TABLE ` + old.TableName); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Revision <= old.Revision || rebuilt.IndexedCount != 1 {
		t.Fatalf("missing table was not rebuilt: old=%+v rebuilt=%+v", old, rebuilt)
	}
}

func TestMaintenanceDuringBuildDoesNotBlockPublication(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "Concurrent maintenance build."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
	embedder.hook = func() {
		if _, err := store.MaintenanceSweep(context.Background(), time.Now().UTC(), config.RetentionPolicy{RetiredIndexRetention: time.Hour, BatchSize: 100}); err != nil {
			t.Errorf("maintenance during build: %v", err)
		}
	}
	if err := NewService(store, nil, embedder, "model", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil || live.State != "live" || live.IndexedCount != 1 {
		t.Fatalf("concurrent maintenance blocked publication: live=%+v err=%v", live, err)
	}
}

func (f *lifecycleEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.mu.Lock()
	dimension := f.dimensions[req.Model]
	isProbe := req.Input == "derived index dimension probe"
	if isProbe {
		f.probeCount++
	}
	fail := (f.failContent && !isProbe) || (f.failProbe && isProbe)
	hook := f.hook
	shouldHook := !isProbe && hook != nil && !f.hooked
	if shouldHook {
		f.hooked = true
	}
	f.mu.Unlock()
	if shouldHook {
		hook()
	}
	if fail {
		return nil, errors.New("embedding unavailable")
	}
	if dimension == 0 {
		return nil, errors.New("unknown model")
	}
	vector := make([]float64, dimension)
	if dimension > 1 && (strings.Contains(strings.ToLower(req.Input), "hardware") || strings.Contains(strings.ToLower(req.Input), "rtx")) {
		vector[1] = 1
	} else {
		vector[0] = 1
	}
	return &llm.EmbedResponse{Model: req.Model, Embeddings: [][]float64{vector}}, nil
}

func (f *lifecycleEmbedder) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probeCount
}

func TestVectorDimensionProbeCachedAcrossCycles(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "First indexed memory."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
	service := NewService(store, nil, embedder, "model", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "Second indexed memory."}); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := embedder.probes(); got != 1 {
		t.Fatalf("dimension probe calls = %d, want 1", got)
	}
}

func TestVectorDimensionProbeFailureRetriesUntilSuccess(t *testing.T) {
	store := newLifecycleStore(t, "user")
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}, failProbe: true}
	service := NewService(store, nil, embedder, "model", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	embedder.mu.Lock()
	embedder.failProbe = false
	embedder.mu.Unlock()
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := embedder.probes(); got != 2 {
		t.Fatalf("dimension probe calls = %d, want one failure and one success", got)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil || live.Dimension != 2 {
		t.Fatalf("live vector revision = %+v, err = %v", live, err)
	}
}

func TestVectorRevisionModelAndDimensionLifecycle(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Category: "projects", Statement: "Project Atlas is active."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model-a": 2, "model-b": 3}}
	service := NewService(store, nil, embedder, "model-a", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if first.Model != "model-a" || first.Dimension != 2 || first.ExpectedCount != 1 || first.IndexedCount != 1 {
		t.Fatalf("first revision=%+v", first)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	same, _ := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if same.Revision != first.Revision {
		t.Fatalf("same configuration rebuilt revision: %d -> %d", first.Revision, same.Revision)
	}
	service = NewService(store, nil, embedder, "model-b", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision <= first.Revision || changed.Model != "model-b" || changed.Dimension != 3 {
		t.Fatalf("changed revision=%+v", changed)
	}
}

func TestWriteArrivingDuringVectorBuildIsReconciled(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "First canonical record."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
	embedder.hook = func() {
		if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "Record written during build."}); err != nil {
			t.Errorf("write during build: %v", err)
		}
	}
	service := NewService(store, nil, embedder, "model", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if live.ExpectedCount != 2 || live.IndexedCount != 2 {
		t.Fatalf("live coverage=%+v", live)
	}
}

func TestWriteDuringModelChangeUpdatesOldLiveAndNewShadow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "user")
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "First canonical record."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"old": 2, "new": 3}}
	if err := NewService(store, nil, embedder, "old", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	embedder.hook = func() {
		if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "Record written during model change."}); err != nil {
			t.Errorf("write during model change: %v", err)
		}
	}
	if err := NewService(store, nil, embedder, "new", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // nolint:errcheck
	var oldRows int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + old.TableName).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 2 {
		t.Fatalf("old live revision row count = %d, want 2", oldRows)
	}
	newLive, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if newLive.Model != "new" || newLive.IndexedCount != 2 {
		t.Fatalf("new live revision = %+v, want model new with 2 rows", newLive)
	}
}

func TestFailedShadowBuildPreservesOldLiveRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "user")
	if _, err := testutil.PublishMemory(context.Background(), store, "user", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "Stable canonical record."}); err != nil {
		t.Fatal(err)
	}
	good := &lifecycleEmbedder{dimensions: map[string]int{"old": 2}}
	if err := NewService(store, nil, good, "old", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, _ := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	failing := &lifecycleEmbedder{dimensions: map[string]int{"new": 3}, failContent: true}
	if err := NewService(store, nil, failing, "new", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if live.ID != old.ID || live.Model != "old" {
		t.Fatalf("failed build replaced live revision: old=%+v live=%+v", old, live)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // nolint:errcheck
	var failed int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM derived_index_revisions WHERE index_kind = 'memory_vector' AND model = 'new' AND state = 'failed'`).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("failed shadow revision count=%d, want 1", failed)
	}
}

func TestRevisionValidationRejectsCrossTenantAndOrphanRows(t *testing.T) {
	store := newLifecycleStore(t, "user-a", "user-b")
	memory, err := testutil.PublishMemory(context.Background(), store, "user-a", testutil.MemoryFixture{Scope: usermemory.ScopeLongTerm, Statement: "Tenant A secret."})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(context.Background(), revision, usermemory.MemoryIndexRecord{ID: memory.ID, UserID: "user-b", Statement: "wrong owner"}, nil); !errors.Is(err, usermemory.ErrStaleIndexRecord) {
		t.Fatalf("cross-tenant write error = %v, want stale record", err)
	}
	revision, err = store.CreateIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(context.Background(), revision, usermemory.MemoryIndexRecord{ID: 99999, UserID: "user-a", Statement: "orphan"}, nil); !errors.Is(err, usermemory.ErrStaleIndexRecord) {
		t.Fatalf("orphan write error = %v, want stale record", err)
	}
}

func TestGlobalMemoryFTSAndVectorLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path)
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
	globalStore, err := globalmemory.NewStore(path, embedder, "model", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	first, err := globalStore.Add(ctx, "Oswald uses a shared derived index.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := globalStore.Add(ctx, "Global facts are searchable."); err != nil {
		t.Fatal(err)
	}
	hardware, err := globalStore.Add(ctx, "Oswald runs on an RTX 4090 with 64 GB of system memory.")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, globalStore, embedder, "model", config.NewLogger(config.LevelError))
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fts, err := store.LiveIndexRevision(ctx, usermemory.IndexKindGlobalMemoryFTS)
	if err != nil || fts.ExpectedCount != 3 || fts.IndexedCount != 3 {
		t.Fatalf("global FTS revision=%+v err=%v", fts, err)
	}
	vector, err := store.LiveIndexRevision(ctx, usermemory.IndexKindGlobalMemoryVector)
	if err != nil || vector.ExpectedCount != 3 || vector.IndexedCount != 3 || vector.Model != "model" || vector.Dimension != 2 {
		t.Fatalf("global vector revision=%+v err=%v", vector, err)
	}
	results, stats := globalStore.Search(ctx, "searchable", 5)
	if !stats.LexicalAvailable || len(results) == 0 {
		t.Fatalf("global search results=%+v stats=%+v", results, stats)
	}
	semanticResults, semanticStats := globalStore.Search(ctx, "hardware specifications", 1)
	if !semanticStats.SemanticAvailable || len(semanticResults) != 1 || semanticResults[0].Memory.ID != hardware.Memory.ID || semanticResults[0].LexicalScore != 0 || semanticResults[0].SemanticScore == 0 {
		t.Fatalf("semantic hardware results=%+v stats=%+v", semanticResults, semanticStats)
	}
	added, err := globalStore.Add(ctx, "A post-build global fact.")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // nolint:errcheck
	for _, revision := range []usermemory.DerivedIndexRevision{fts, vector} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, added.Memory.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("new global memory missing from %s", revision.TableName)
		}
	}
	if forgotten, err := globalStore.Forget(ctx, first.Memory.ID); err != nil || !forgotten {
		t.Fatalf("forget=%v err=%v", forgotten, err)
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []usermemory.DerivedIndexRevision{fts, vector} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, first.Memory.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("deleted global memory remains in %s", revision.TableName)
		}
	}
}

func TestGlobalVectorValidationRejectsStaleCanonicalVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path)
	globalStore, err := globalmemory.NewStore(path, nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	added, err := globalStore.Add(ctx, "Version-fenced global fact.")
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.GlobalMemoryIndexRecordByID(ctx, added.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, usermemory.IndexKindGlobalMemoryVector, "llm_gateway", "model", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteGlobalMemoryIndexRecord(ctx, revision, record, []float64{1, 0}); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`UPDATE `+revision.TableName+` SET canonical_version = 'stale' WHERE rowid = ?`, record.ID); err != nil {
		db.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err == nil {
		t.Fatal("global vector revision with stale canonical version was published")
	}
}

func newLifecycleStore(t *testing.T, users ...string) *usermemory.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oswald.db")
	return newLifecycleStoreAt(t, path, users...)
}

func newLifecycleStoreAt(t *testing.T, path string, users ...string) *usermemory.Store {
	t.Helper()
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES (?)`, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := usermemory.NewSQLiteStore(path, nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
