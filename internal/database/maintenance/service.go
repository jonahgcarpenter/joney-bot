package maintenance

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

// Sweeper performs one complete maintenance pass.
type Sweeper interface {
	MaintenanceSweep(context.Context, time.Time, config.RetentionPolicy) (memory.MaintenanceCounts, error)
}

// Service owns the single periodic maintenance goroutine.
type Service struct {
	sweeper Sweeper
	policy  config.RetentionPolicy
	log     *config.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewService creates a periodic maintenance service.
func NewService(sweeper Sweeper, policy config.RetentionPolicy, log *config.Logger) *Service {
	if policy.MaintenanceInterval <= 0 {
		policy.MaintenanceInterval = time.Hour
	}
	if log != nil {
		log = log.Server("maintenanceruntime")
	}
	return &Service{sweeper: sweeper, policy: policy, log: log}
}

// Start launches an immediate sweep followed by interval-based sweeps.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.sweeper == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

// Stop cancels the worker and waits for the active sweep to return.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()
	if s.log != nil {
		s.log.Info("maintenance.worker.started", "maintenance worker started", config.F("workload", "maintenance"))
		defer s.log.Info("maintenance.worker.stopped", "maintenance worker stopped", config.F("workload", "maintenance"))
	}
	s.sweep(ctx)
	ticker := time.NewTicker(s.policy.MaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sweepAt(ctx, now.UTC())
		}
	}
}

func (s *Service) sweep(ctx context.Context) {
	s.sweepAt(ctx, time.Now().UTC())
}

func (s *Service) sweepAt(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	started := time.Now()
	counts, err := s.sweeper.MaintenanceSweep(ctx, now, s.policy)
	if s.log == nil {
		return
	}
	// rows_changed counts committed operations, not distinct rows: a row can be
	// updated in one phase and deleted in another. Phase counters are disjoint.
	fields := []config.Field{
		config.F("record_kind", "summary"), config.F("workload", "maintenance"), config.F("phase", counts.Phase),
		config.F("rows_changed", counts.Changed()), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("is_optimize_run", counts.OptimizeRun),
		config.F("session_turn_deleted_count", counts.SessionCleanup.SessionTurnsDeleted),
		config.F("session_deactivated_count", counts.SessionCleanup.SessionsDeactivated),
		config.F("memory_expired_count", counts.SessionCleanup.MemoryEntriesExpired),
		config.F("observation_deleted_count", counts.SessionCleanup.ObservationsDeleted),
		config.F("assessment_receipt_deleted_count", counts.AssessmentReceiptsDeleted),
		config.F("observation_receipt_deleted_count", counts.ObservationReceiptsDeleted),
		config.F("expiry_candidate_deleted_count", counts.SessionCleanup.CandidatesDeleted),
		config.F("expiry_formation_job_deleted_count", counts.SessionCleanup.FormationJobsDeleted),
		config.F("session_summary_deleted_count", counts.SessionCleanup.SessionSummariesDeleted),
		config.F("compaction_job_retired_count", counts.SessionCleanup.CompactionJobsRetired),
		config.F("pending_delivery_failed_count", counts.PendingDeliveriesFailed),
		config.F("candidate_deleted_count", counts.CandidatesDeleted),
		config.F("terminal_job_deleted_count", counts.FormationJobsDeleted+counts.CompactionJobsDeleted),
		config.F("derived_index_job_deleted_count", counts.DerivedIndexJobsDeleted),
		config.F("challenge_deleted_count", counts.ChallengesDeleted),
		config.F("index_row_deleted_count", counts.IndexRowsDeleted),
		config.F("index_revision_degraded_count", counts.IndexRevisionsDegraded),
		config.F("index_table_dropped_count", counts.IndexTablesDropped),
	}
	if err != nil {
		if s.log != nil {
			fields = append(fields, config.F("partial_committed_count", counts.Changed()))
			if errors.Is(err, context.Canceled) {
				s.log.Info("maintenance.sweep.canceled", "maintenance sweep canceled", append(fields, config.F("outcome", "canceled"), config.F("status", "ok"))...)
			} else {
				s.log.Warn("maintenance.sweep.failed", "periodic maintenance sweep failed", append(fields, config.F("outcome", "partial"), config.F("status", "degraded"), config.ErrorField(err))...)
			}
		}
		return
	}
	if s.log != nil {
		emit := s.log.Info
		if counts.IndexRevisionsDegraded > 0 {
			emit = s.log.Warn
		}
		status := "ok"
		if counts.IndexRevisionsDegraded > 0 {
			status = "degraded"
		}
		emit("maintenance.sweep.complete", "periodic maintenance sweep completed", append(fields, config.F("outcome", "committed"), config.F("status", status))...)
	}
}
