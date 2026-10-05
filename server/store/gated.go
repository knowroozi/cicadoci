package store

import (
	"context"

	"github.com/google/uuid"
)

// PromoteTopLevelJobs flips all top-level jobs (those with no incoming
// job_dependencies rows for this run) from "pending" to "queued", firing
// the NOTIFY trigger so the dispatch loop picks them up.
//
// CIC-182: call after an approval gate is approved.
func (s *Store) PromoteTopLevelJobs(ctx context.Context, runID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
        UPDATE jobs
          SET status = 'queued'
          WHERE run_id = $1
            AND status = 'pending'
            AND NOT EXISTS (
              SELECT 1 FROM job_dependencies WHERE job_id = jobs.id
             )`, runID)
	return err
}

// FailAllJobs fails every "pending" job for the run and skips all of
// its dependents, so a denied or timed-out gate leaves no orphaned
// queued jobs. CIC-182 B3.
//
// A "dependent" is any job that has an incoming edge in the
// job_dependencies table for this run.
func (s *Store) FailAllJobs(ctx context.Context, runID uuid.UUID, reason string) error {
	// CIC-182 N5: top-level jobs are failed; dependent jobs are skipped.
	// They would never run because their predecessor did not succeed.
	_, err := s.pool.Exec(ctx, `
        UPDATE jobs SET status = 'failed', cancel_reason = $1
          WHERE run_id = $2
            AND status = 'pending'
            AND NOT EXISTS (
              SELECT 1 FROM job_dependencies WHERE job_id = jobs.id
              )`, reason, runID)
	if err != nil {
		return err
	}
	// Skip pending dependent jobs (jobs with incoming edges).
	_, err = s.pool.Exec(ctx, `
        UPDATE jobs SET status = 'skipped', cancel_reason = 'skipped: ' || $1
          WHERE run_id = $2
            AND status = 'pending'`, reason, runID)
	return err
}
