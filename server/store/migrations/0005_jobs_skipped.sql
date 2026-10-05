-- +goose Up
-- +goose StatementBegin

-- Add 'skipped' to the jobs status CHECK constraint.
--
-- A job that is a dependent of a failed/skipped top-level job is skipped
-- (its prerequisite did not succeed), not cancelled by a system event.
-- The cancel_reason column carries the upstream job's cancel reason,
-- prefixed 'skipped: ' so operators can distinguish skipped dependents
-- from system-caused cancellations.
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('pending', 'queued', 'claimed', 'running',
                      'succeeded', 'failed', 'skipped',
                      'cancelled', 'dead_letter'));

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin

ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_status_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_status_check
    CHECK (status IN ('pending', 'queued', 'claimed', 'running',
                      'succeeded', 'failed',
                      'cancelled', 'dead_letter'));

-- +goose StatementEnd
