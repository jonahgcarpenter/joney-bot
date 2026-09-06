package indexing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/lease"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const (
	batchSize = 32
	leaseTime = time.Minute
)

// Service serially applies durable index changes and builds shadow revisions.
type Service struct {
	store       *memory.Store
	globalStore *global.Store
	embedder    llm.Embedder
	model       string
	dimension   int
	log         *config.Logger
	wake        chan struct{}
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// NewService creates a derived-index lifecycle service.
func NewService(store *memory.Store, globalStore *global.Store, embedder llm.Embedder, model string, log *config.Logger) *Service {
	return &Service{store: store, globalStore: globalStore, embedder: embedder, model: model, log: log, wake: make(chan struct{}, 1)}
}

// Signal nonblockingly wakes the worker; startup and polling reconcile missed signals.
func (s *Service) Signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start launches the serialized worker.
func (s *Service) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.store.SetDerivedIndexNotifier(s.Signal)
	if s.globalStore != nil {
		s.globalStore.SetDerivedIndexNotifier(s.Signal)
	}
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop stops without discarding durable pending work.
func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// RunOnce performs startup reconciliation and one complete serialized cycle.
// It is useful for deterministic maintenance runs and tests.
func (s *Service) RunOnce(ctx context.Context) error {
	if err := s.store.ReconcileDerivedIndexChanges(ctx); err != nil {
		return err
	}
	s.cycle(ctx)
	return ctx.Err()
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()
	if s.log != nil {
		s.log.Server("indexruntime").Info("index.worker.started", "index worker started", config.F("workload", "indexing"))
		defer s.log.Server("indexruntime").Info("index.worker.stopped", "index worker stopped", config.F("workload", "indexing"))
	}
	if err := s.store.ReconcileDerivedIndexChanges(ctx); err != nil {
		s.warn("index.outbox.reconcile_failed", "reconcile", err)
	}
	s.cycle(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.cycle(ctx)
		case <-ticker.C:
			if err := s.store.ReconcileDerivedIndexChanges(ctx); err != nil {
				s.warn("index.outbox.reconcile_failed", "reconcile", err)
			}
			s.snapshot(ctx)
			s.cycle(ctx)
		}
	}
}

func (s *Service) cycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	meta := requestctx.MetadataFromContext(ctx)
	meta.ParentOperationID, meta.OperationID = meta.OperationID, rand.Text()
	meta.Workload = "indexing"
	ctx = requestctx.WithMetadata(ctx, meta)
	parent := s
	worker := &Service{store: s.store, globalStore: s.globalStore, embedder: s.embedder, model: s.model, dimension: s.dimension, log: s.log}
	if worker.log != nil {
		worker.log = worker.log.With(requestctx.LogFields(ctx)...)
	}
	// Retain the successful dimension probe without sharing cycle log scope.
	defer func() { parent.dimension = worker.dimension }()
	s = worker
	s.ensureFTS(ctx, memory.IndexKindMemoryFTS)
	s.ensureFTS(ctx, memory.IndexKindTranscriptFTS)
	s.ensureFTS(ctx, memory.IndexKindGlobalMemoryFTS)
	if s.model != "" && s.embedder != nil {
		if _, err := s.vectorDimension(ctx); err != nil {
			s.warn("index.vector.probe_failed", "vector", err)
		} else {
			s.ensureVector(ctx, memory.IndexKindMemoryVector)
			s.ensureVector(ctx, memory.IndexKindGlobalMemoryVector)
		}
	}
	s.drain(ctx)
}

func (s *Service) ensureFTS(ctx context.Context, kind string) {
	needsRebuild, healthErr := s.store.IndexRevisionNeedsRebuild(ctx, kind)
	if healthErr == nil && !needsRebuild {
		return
	}
	if healthErr != nil && !errors.Is(healthErr, sql.ErrNoRows) {
		s.warn("index.health.failed", kind, healthErr)
		return
	}
	revision, err := s.store.BuildingIndexRevision(ctx, kind)
	if errors.Is(err, sql.ErrNoRows) {
		revision, err = s.store.CreateIndexRevision(ctx, kind, "sqlite_fts5", "", 0)
	}
	if err != nil {
		s.warn("index.rebuild.prepare_failed", kind, err)
		return
	}
	started := time.Now()
	if kind == memory.IndexKindMemoryFTS {
		err = s.buildMemoryFTS(ctx, revision)
	} else if kind == memory.IndexKindTranscriptFTS {
		err = s.buildTranscriptFTS(ctx, revision)
	} else {
		err = s.buildGlobalMemory(ctx, revision)
	}
	if err == nil {
		err = s.publishAfterDrain(ctx, revision)
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			markErr := s.store.FailIndexRevision(markCtx, revision.ID, "rebuild_failed")
			cancel()
			if markErr != nil {
				s.warn("index.rebuild.mark_failed", kind, markErr, config.F("revision", revision.Revision), config.F("phase", "failure_mark"))
			}
		}
		s.health("index.rebuild.failed", revision, 0, 0, "degraded", time.Since(started), err)
		return
	}
	live, readErr := s.store.LiveIndexRevision(ctx, kind)
	if readErr != nil {
		s.warn("index.rebuild.live_read_failed", kind, readErr, config.F("phase", "post_publish"))
		return
	}
	s.health("index.rebuild.complete", live, live.ExpectedCount, live.IndexedCount, "ok", time.Since(started), nil)
}

func (s *Service) ensureVector(ctx context.Context, kind string) {
	if s.model == "" || s.embedder == nil {
		return
	}
	dimension, err := s.vectorDimension(ctx)
	if err != nil {
		s.warn("index.vector.probe_failed", kind, err)
		return
	}
	live, liveErr := s.store.LiveIndexRevision(ctx, kind)
	needsRebuild, healthErr := s.store.IndexRevisionNeedsRebuild(ctx, kind)
	if healthErr != nil && !errors.Is(healthErr, sql.ErrNoRows) {
		s.warn("index.health.failed", kind, healthErr)
		return
	}
	if liveErr == nil && live.Model == s.model && live.Dimension == dimension && live.SchemaVersion >= 2 && !needsRebuild {
		return
	}
	revision, err := s.store.BuildingIndexRevision(ctx, kind)
	if err == nil && (revision.Model != s.model || revision.Dimension != dimension) {
		if markErr := s.store.FailIndexRevision(ctx, revision.ID, "configuration_changed"); markErr != nil {
			s.warn("index.rebuild.mark_failed", kind, markErr, config.F("phase", "configuration_changed"))
			return
		}
		err = sql.ErrNoRows
	}
	if errors.Is(err, sql.ErrNoRows) {
		revision, err = s.store.CreateIndexRevision(ctx, kind, "llm_gateway", s.model, dimension)
	}
	if err != nil {
		s.warn("index.rebuild.failed", kind, err)
		return
	}
	started := time.Now()
	if kind == memory.IndexKindMemoryVector {
		err = s.buildMemoryVector(ctx, revision)
	} else {
		err = s.buildGlobalMemory(ctx, revision)
	}
	if err == nil {
		err = s.publishAfterDrain(ctx, revision)
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			markErr := s.store.FailIndexRevision(markCtx, revision.ID, "rebuild_failed")
			cancel()
			if markErr != nil {
				s.warn("index.rebuild.mark_failed", kind, markErr, config.F("revision", revision.Revision), config.F("phase", "failure_mark"))
			}
		}
		s.health("index.rebuild.failed", revision, 0, 0, "degraded", time.Since(started), err)
		return
	}
	live, liveErr = s.store.LiveIndexRevision(ctx, kind)
	if liveErr != nil {
		s.warn("index.rebuild.live_read_failed", kind, liveErr, config.F("phase", "post_publish"))
		return
	}
	s.health("index.rebuild.complete", live, live.ExpectedCount, live.IndexedCount, "ok", time.Since(started), nil)
}

func (s *Service) buildMemoryFTS(ctx context.Context, revision memory.DerivedIndexRevision) error {
	var after int64
	for {
		records, err := s.store.ActiveMemoryIndexRecords(ctx, after, batchSize)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := s.writeCurrentMemory(ctx, revision, record); err != nil {
				return err
			}
			after = record.ID
		}
		if len(records) < batchSize {
			return nil
		}
	}
}

func (s *Service) buildTranscriptFTS(ctx context.Context, revision memory.DerivedIndexRevision) error {
	var after int64
	for {
		records, err := s.store.DeliveredTranscriptIndexRecords(ctx, after, batchSize)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := s.writeCurrentTranscript(ctx, revision, record); err != nil {
				return err
			}
			after = record.ID
		}
		if len(records) < batchSize {
			return nil
		}
	}
}

func (s *Service) buildMemoryVector(ctx context.Context, revision memory.DerivedIndexRevision) error {
	var after int64
	for {
		records, err := s.store.ActiveMemoryIndexRecords(ctx, after, batchSize)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := s.writeCurrentMemory(ctx, revision, record); err != nil {
				return err
			}
			after = record.ID
		}
		if len(records) < batchSize {
			return nil
		}
	}
}

func (s *Service) buildGlobalMemory(ctx context.Context, revision memory.DerivedIndexRevision) error {
	var after int64
	for {
		records, err := s.store.GlobalMemoryIndexRecords(ctx, after, batchSize)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := s.writeCurrentGlobalMemory(ctx, revision, record); err != nil {
				return err
			}
			after = record.ID
		}
		if len(records) < batchSize {
			return nil
		}
	}
}

func (s *Service) publishAfterDrain(ctx context.Context, revision memory.DerivedIndexRevision) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		s.drain(ctx)
		_, err = s.store.ValidateAndPublishIndexRevision(ctx, revision.ID)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		change, err := s.store.ClaimDerivedIndexChange(ctx, leaseTime)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			s.warn("index.outbox.claim_failed", "outbox", err)
			return
		}
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		meta.ParentOperationID, meta.OperationID = meta.OperationID, rand.Text()
		meta.Workload, meta.JobID = "indexing", change.Sequence
		jobCtx := requestctx.WithMetadata(ctx, meta)
		jobCtx = requestctx.WithPrincipal(jobCtx, identity.Principal{CanonicalUserID: change.UserID})
		jobCtx = requestctx.WithUsageCollector(jobCtx, requestctx.NewUsageCollector())
		fields := append(requestctx.LogFields(jobCtx), config.F("job_kind", "derived_index"), config.F("entity_kind", change.EntityKind), config.F("entity_id", change.EntityID), config.F("operation", change.Operation), config.F("attempt_count", change.AttemptCount))
		err = lease.Run(jobCtx, leaseTime,
			func(renewCtx context.Context) error {
				return s.store.RenewDerivedIndexChangeLease(renewCtx, change, leaseTime)
			},
			func(workCtx context.Context) error { return s.applyChange(workCtx, change) },
		)
		fields = append(fields, config.F("record_kind", "summary"), config.F("duration_ms", time.Since(started).Milliseconds()))
		if err != nil {
			// Bookkeeping must survive worker cancellation, retaining exact lease ownership.
			bookCtx, cancel := context.WithTimeout(context.WithoutCancel(jobCtx), 10*time.Second)
			retryErr := s.store.RetryDerivedIndexChange(bookCtx, change, "index_apply_failed")
			cancel()
			if retryErr != nil {
				s.warn("index.outbox.retry_failed", change.EntityKind, retryErr, fields...)
				return
			}
			if s.log != nil {
				outcome := "retry"
				fields = append(fields, config.F("phase", "apply"), config.ErrorField(err))
				if errors.Is(err, context.Canceled) {
					outcome = "canceled"
				}
				s.log.Server("indexruntime").Info("index.outbox.attempt.complete", "index outbox attempt persisted", append(fields, config.F("job_state", "retry"), config.F("outcome", outcome), config.F("status", "retry"))...)
			}
			continue
		}
		if err := s.store.CompleteDerivedIndexChange(ctx, change); err != nil {
			s.warn("index.outbox.complete_failed", change.EntityKind, err, fields...)
			return
		}
		if s.log != nil {
			s.log.Server("indexruntime").Info("index.outbox.attempt.complete", "index outbox attempt committed", append(fields, config.F("job_state", "succeeded"), config.F("outcome", "completed"), config.F("status", "ok"))...)
		}
	}
}

func (s *Service) applyChange(ctx context.Context, change memory.DerivedIndexChange) error {
	revisions, err := s.store.WritableIndexRevisions(ctx, change.EntityKind)
	if err != nil {
		return err
	}
	if change.EntityKind == "memory" {
		for _, revision := range revisions {
			if revision.Kind == memory.IndexKindMemoryVector && s.embedder == nil {
				continue
			}
			record, recordErr := s.store.MemoryIndexRecordByID(ctx, change.EntityID, change.UserID)
			if errors.Is(recordErr, sql.ErrNoRows) {
				if err := s.store.DeleteIndexRecord(ctx, revision, change.EntityID, change.UserID); err != nil {
					return err
				}
				continue
			}
			if recordErr != nil {
				return recordErr
			}
			if err := s.writeCurrentMemory(ctx, revision, record); err != nil {
				return err
			}
		}
		return nil
	}
	if change.EntityKind == "global_memory" {
		for _, revision := range revisions {
			if revision.Kind == memory.IndexKindGlobalMemoryVector && s.embedder == nil {
				continue
			}
			record, recordErr := s.store.GlobalMemoryIndexRecordByID(ctx, change.EntityID)
			if errors.Is(recordErr, sql.ErrNoRows) {
				if err := s.store.DeleteGlobalMemoryIndexRecord(ctx, revision, change.EntityID); err != nil {
					return err
				}
				continue
			}
			if recordErr != nil {
				return recordErr
			}
			if err := s.writeCurrentGlobalMemory(ctx, revision, record); err != nil {
				return err
			}
		}
		return nil
	}
	for _, revision := range revisions {
		record, recordErr := s.store.TranscriptIndexRecordByID(ctx, change.EntityID, change.UserID)
		if errors.Is(recordErr, sql.ErrNoRows) {
			if err := s.store.DeleteIndexRecord(ctx, revision, change.EntityID, change.UserID); err != nil {
				return err
			}
			continue
		}
		if recordErr != nil {
			return recordErr
		}
		if err := s.writeCurrentTranscript(ctx, revision, record); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) writeCurrentMemory(ctx context.Context, revision memory.DerivedIndexRevision, record memory.MemoryIndexRecord) error {
	for attempt := 0; attempt < 3; attempt++ {
		var vector []float64
		if revision.Kind == memory.IndexKindMemoryVector {
			var err error
			vector, err = s.embed(ctx, revision.Model, embeddingText(record))
			if err != nil {
				return err
			}
			if len(vector) != revision.Dimension {
				return fmt.Errorf("embedding dimension changed during build")
			}
		}
		err := s.store.WriteMemoryIndexRecord(ctx, revision, record, vector)
		if !errors.Is(err, memory.ErrStaleIndexRecord) {
			return err
		}
		record, err = s.store.MemoryIndexRecordByID(ctx, record.ID, record.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return memory.ErrStaleIndexRecord
}

func (s *Service) writeCurrentTranscript(ctx context.Context, revision memory.DerivedIndexRevision, record memory.TranscriptIndexRecord) error {
	for attempt := 0; attempt < 3; attempt++ {
		err := s.store.WriteTranscriptIndexRecord(ctx, revision, record)
		if !errors.Is(err, memory.ErrStaleIndexRecord) {
			return err
		}
		record, err = s.store.TranscriptIndexRecordByID(ctx, record.ID, record.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return memory.ErrStaleIndexRecord
}

func (s *Service) writeCurrentGlobalMemory(ctx context.Context, revision memory.DerivedIndexRevision, record memory.GlobalMemoryIndexRecord) error {
	for attempt := 0; attempt < 3; attempt++ {
		var vector []float64
		if revision.Kind == memory.IndexKindGlobalMemoryVector {
			var err error
			vector, err = s.embed(ctx, revision.Model, record.Memory)
			if err != nil {
				return err
			}
			if len(vector) != revision.Dimension {
				return fmt.Errorf("embedding dimension changed during build")
			}
		}
		err := s.store.WriteGlobalMemoryIndexRecord(ctx, revision, record, vector)
		if !errors.Is(err, memory.ErrStaleIndexRecord) {
			return err
		}
		record, err = s.store.GlobalMemoryIndexRecordByID(ctx, record.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return memory.ErrStaleIndexRecord
}

func (s *Service) embed(ctx context.Context, model, input string) ([]float64, error) {
	response, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: model, Input: input})
	if err != nil {
		return nil, err
	}
	if response == nil || len(response.Embeddings) == 0 || len(response.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embedding provider returned no vector")
	}
	return response.Embeddings[0], nil
}

func (s *Service) vectorDimension(ctx context.Context) (int, error) {
	if s.dimension > 0 {
		return s.dimension, nil
	}
	probe, err := s.embed(ctx, s.model, "derived index dimension probe")
	if err != nil {
		return 0, err
	}
	s.dimension = len(probe)
	return s.dimension, nil
}

func embeddingText(record memory.MemoryIndexRecord) string {
	return record.Scope + "\n" + record.Category + "\n" + record.Statement + "\nEvidence: " + record.Evidence
}

func (s *Service) warn(event, kind string, err error, fields ...config.Field) {
	if s.log != nil {
		if errors.Is(err, context.Canceled) {
			s.log.Server("indexruntime").Info("index.work.canceled", "index work canceled", append(fields, config.F("kind", kind), config.F("workload", "indexing"), config.F("outcome", "canceled"), config.F("status", "ok"))...)
			return
		}
		fields = append(fields, config.F("kind", kind), config.F("workload", "indexing"), config.F("status", "degraded"), config.ErrorField(err))
		s.log.Server("indexruntime").Warn(event, "derived index lifecycle degraded", fields...)
	}
}

func (s *Service) health(event string, revision memory.DerivedIndexRevision, expected, indexed int64, status string, duration time.Duration, err error) {
	if s.log == nil {
		return
	}
	fields := []config.Field{config.F("kind", revision.Kind), config.F("workload", "indexing"), config.F("revision", revision.Revision), config.F("model", revision.Model), config.F("dimension", revision.Dimension), config.F("status", status), config.F("duration_ms", duration.Milliseconds())}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			s.log.Server("indexruntime").Info("index.rebuild.canceled", "index rebuild canceled", append(fields, config.F("outcome", "canceled"), config.F("status", "ok"))...)
			return
		}
		s.log.Server("indexruntime").Warn(event, "derived index rebuild failed", append(fields, config.F("phase", "build_publish"), config.ErrorField(err))...)
		return
	}
	fields = append(fields, config.F("record_kind", "summary"), config.F("expected_count", expected), config.F("indexed_count", indexed), config.F("coverage", coverage(expected, indexed)))
	s.log.Server("indexruntime").Info(event, "derived index health", fields...)
}

func coverage(expected, indexed int64) float64 {
	if expected == 0 {
		return 1
	}
	return float64(indexed) / float64(expected)
}

func (s *Service) snapshot(ctx context.Context) {
	if s.log == nil || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	log := s.log.Server("indexruntime").With(config.F("workload", "indexing"), config.F("record_kind", "snapshot"))
	jobs, err := s.store.JobHealth(ctx)
	if err != nil {
		s.warn("memory.health.failed", "jobs", err, config.F("record_kind", "snapshot"))
	} else {
		for _, job := range jobs {
			log.Info("memory.jobs.health", "durable job backlog snapshot", config.F("job_kind", job.Kind), config.F("queued_count", job.Queued), config.F("active_count", job.Running), config.F("retry_count", job.Retry), config.F("dead_count", job.Dead), config.F("succeeded_count", job.Succeeded), config.F("skipped_count", job.Skipped), config.F("expired_lease_count", job.ExpiredLeaseCount), config.F("oldest_ready_age_ms", job.OldestReadyAgeMS), config.F("status", "ok"))
		}
	}
	for _, kind := range []string{memory.IndexKindMemoryFTS, memory.IndexKindTranscriptFTS, memory.IndexKindGlobalMemoryFTS, memory.IndexKindMemoryVector, memory.IndexKindGlobalMemoryVector} {
		if (kind == memory.IndexKindMemoryVector || kind == memory.IndexKindGlobalMemoryVector) && s.model == "" {
			continue
		}
		needs, err := s.store.IndexRevisionNeedsRebuild(ctx, kind)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.warn("index.health.failed", kind, err, config.F("record_kind", "snapshot"))
			continue
		}
		fields := []config.Field{config.F("index_kind", kind), config.F("is_available", err == nil && !needs)}
		if err != nil || needs {
			log.Warn("index.availability", "derived index unavailable or degraded", append(fields, config.F("status", "degraded"))...)
		} else {
			log.Info("index.availability", "derived index available", append(fields, config.F("status", "ok"))...)
		}
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	last := s.store.LastMaintenance()
	fields := []config.Field{config.F("goroutine_count", runtime.NumGoroutine()), config.F("heap_alloc_bytes", stats.HeapAlloc), config.F("heap_inuse_bytes", stats.HeapInuse), config.F("gc_count", stats.NumGC), config.F("is_last_maintenance_known", !last.IsZero()), config.F("status", "ok")}
	if !last.IsZero() {
		fields = append(fields, config.F("last_maintenance_age_ms", max(time.Since(last).Milliseconds(), 0)))
	}
	log.Info("app.health", "process and maintenance snapshot", fields...)
}
