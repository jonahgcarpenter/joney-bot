package memory

import (
	"context"
	"time"
)

// JobHealthCounts is a database-wide aggregate, never a list of tenant jobs.
// OldestReadyAgeMS is time past available_at for queued/retry work, clamped at zero.
type JobHealthCounts struct {
	Kind                         string
	Queued, Running, Retry, Dead int64
	Succeeded, Skipped           int64
	OldestReadyAgeMS             int64
	ExpiredLeaseCount            int64
}

// JobHealth reads all supported job kinds in one snapshot. An error returns no
// gauges, rather than presenting unavailable storage as an empty backlog.
func (s *Store) JobHealth(ctx context.Context) ([]JobHealthCounts, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.sql.QueryContext(ctx, `WITH kinds(kind) AS (VALUES ('memory_formation'), ('session_compaction'), ('derived_index'))
SELECT kinds.kind,
 COALESCE(SUM(job.state = 'queued'), 0), COALESCE(SUM(job.state = 'running'), 0),
 COALESCE(SUM(job.state = 'retry'), 0), COALESCE(SUM(job.state = 'dead'), 0),
 COALESCE(SUM(job.state = 'succeeded'), 0), COALESCE(SUM(job.state = 'skipped'), 0),
 COALESCE(SUM(job.state = 'running' AND julianday(job.lease_until) <= julianday('now')), 0),
 COALESCE(MAX(CASE WHEN job.state IN ('queued','retry') THEN MAX(0, CAST((julianday('now') - julianday(job.available_at)) * 86400000 AS INTEGER)) END), 0)
FROM kinds LEFT JOIN durable_jobs job ON job.job_kind = kinds.kind GROUP BY kinds.kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []JobHealthCounts
	for rows.Next() {
		var item JobHealthCounts
		if err := rows.Scan(&item.Kind, &item.Queued, &item.Running, &item.Retry, &item.Dead, &item.Succeeded, &item.Skipped, &item.ExpiredLeaseCount, &item.OldestReadyAgeMS); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// LastMaintenance returns the last successful sweep in this process. Zero means
// unknown, including after restart; it is not a persisted maintenance timestamp.
func (s *Store) LastMaintenance() time.Time {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	return s.lastMaintenanceAt
}
