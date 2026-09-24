-- +goose Up
-- +goose StatementBegin

-- Widen the `jobs.executor` CHECK constraint to permit 'cicanscript'.
--
-- The cicanscript executor (CIC-16) is a thin wrapper around Shell that
-- makes "run a sequence of shell commands" explicit at the JobSpec.Executor
-- level. Without this migration, a job row with executor='cicanscript' is
-- rejected at write time by the CHECK constraint, so NewExecutor("cicanscript")
-- and its runner-side tests are all correct in isolation but unreachable
-- end-to-end.
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_executor_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_executor_check
    CHECK (executor IN ('docker', 'shell', 'cicanscript'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_executor_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_executor_check
    CHECK (executor IN ('docker', 'shell'));

-- +goose StatementEnd
