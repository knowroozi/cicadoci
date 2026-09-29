-- +goose Up
-- +goose StatementBegin

-- Artifacts table (CIC-34). Created by `server/artifacts.Postgres`.
--
-- Holds artifact *metadata* (index); actual bytes live in the BlobStore.
-- One row per artifact. Run-level and job-level artifacts share the table
-- (job_id NULL for run-level).
CREATE TABLE artifacts (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    run_id       uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    job_id       uuid        REFERENCES jobs (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    blob_key     text        NOT NULL,
    size_bytes   bigint      NOT NULL,
    content_type text        DEFAULT 'application/octet-stream',
    uploaded_at  timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Index for the common lookups: by run, by (run, name), and by expiry.
CREATE INDEX artifacts_run_idx        ON artifacts (run_id, created_at DESC);
CREATE INDEX artifacts_run_name_idx   ON artifacts (run_id, name) WHERE job_id IS NULL;
CREATE INDEX artifacts_expiry_idx     ON artifacts (expires_at)
    WHERE expires_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS artifacts;

-- +goose StatementEnd
