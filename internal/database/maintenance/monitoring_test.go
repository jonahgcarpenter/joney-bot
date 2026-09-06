package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

type monitoringSweeper struct {
	counts memory.MaintenanceCounts
	err    error
	cancel context.CancelFunc
}

func TestSweepSummariesFlattenCommittedExpiryCounts(t *testing.T) {
	counts := memory.MaintenanceCounts{CandidatesDeleted: 8, SessionCleanup: memory.SessionCleanupCounts{SessionTurnsDeleted: 1, SessionsDeactivated: 2, MemoryEntriesExpired: 3, CandidatesDeleted: 4, FormationJobsDeleted: 5, SessionSummariesDeleted: 6, CompactionJobsRetired: 7}}
	for _, failure := range []bool{false, true} {
		var output bytes.Buffer
		log := config.NewLogger(config.LevelInfo)
		log.SetOutput(&output)
		sweeper := monitoringSweeper{counts: counts}
		if failure {
			sweeper.err = errors.New("storage failure")
		}
		NewService(sweeper, config.DefaultRetentionPolicy(), log).sweep(context.Background())
		var record map[string]any
		if err := json.Unmarshal(output.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]any{"record_kind": "summary", "session_turn_deleted_count": float64(1), "session_deactivated_count": float64(2), "memory_expired_count": float64(3), "expiry_candidate_deleted_count": float64(4), "expiry_formation_job_deleted_count": float64(5), "session_summary_deleted_count": float64(6), "compaction_job_retired_count": float64(7), "candidate_deleted_count": float64(8), "rows_changed": float64(36)} {
			if record[key] != want {
				t.Fatalf("failure=%v %s=%v want=%v", failure, key, record[key], want)
			}
		}
		if failure && record["partial_committed_count"] != float64(36) {
			t.Fatalf("partial count=%v", record)
		}
	}
}

func (s monitoringSweeper) MaintenanceSweep(context.Context, time.Time, config.RetentionPolicy) (memory.MaintenanceCounts, error) {
	if s.cancel != nil {
		s.cancel()
	}
	return s.counts, s.err
}

func TestSweepLevelsAndIndependentFailureDuringCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, level string
		counts      memory.MaintenanceCounts
		err         error
		cancel      bool
	}{
		{name: "unchanged", level: "info"},
		{name: "changed", level: "info", counts: memory.MaintenanceCounts{CandidatesDeleted: 1}},
		{name: "hygiene", level: "info", counts: memory.MaintenanceCounts{OptimizeRun: true}},
		{name: "degraded", level: "warn", counts: memory.MaintenanceCounts{IndexRevisionsDegraded: 1}},
		{name: "canceled", level: "info", err: context.Canceled, cancel: true},
		{name: "independent_failure", level: "warn", err: errors.New("private sentinel"), cancel: true, counts: memory.MaintenanceCounts{CandidatesDeleted: 2, Phase: "indexes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sweeper := monitoringSweeper{counts: tc.counts, err: tc.err}
			if tc.cancel {
				sweeper.cancel = cancel
			}
			var output bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&output)
			NewService(sweeper, config.DefaultRetentionPolicy(), log).sweep(ctx)
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["level"] != tc.level {
				t.Fatalf("record=%s", output.String())
			}
			if tc.name == "independent_failure" && (record["partial_committed_count"] != float64(2) || record["phase"] != "indexes") {
				t.Fatalf("partial counts=%s", output.String())
			}
			if tc.name == "canceled" && record["error_code"] != nil {
				t.Fatalf("cancellation error noise=%s", output.String())
			}
		})
	}
}
