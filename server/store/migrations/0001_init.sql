-- +goose Up
-- +goose StatementBegin

-- CICaDo schema v1.
--
-- Shape follows project-docs/02-architecture.md §2. Two things are worth
-- knowing before reading:
--
--   1. The `jobs` table IS the queue (ADR-002). There is no separate queue
--      store, and enqueue happens in the same transaction as run/job creation,
--      so there is no dual-write to get out of sync. The claim path is
--      FOR UPDATE SKIP LOCKED; the doorbell is LISTEN/NOTIFY; orphan recovery
--      is a sweep over expired leases.
--
--   2. Leases are the only execution authority (the runner protocol's core
--      invariant). A job carries at most one live lease_id; a requeue mints a
--      new one, which is how a legitimate re-run is told apart from a zombie
--      runner that woke up holding a stale lease.
--
-- Requires PostgreSQL 13+ for gen_random_uuid() in core.

-- ─────────────────────────────────────────────────────────────────────────────
-- Tenancy
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE orgs (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text        NOT NULL UNIQUE,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE repos (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    -- Forge discriminator. Only 'github' is implemented in MVP; the column
    -- exists so the Forge adapter interface has somewhere to land.
    forge          text        NOT NULL DEFAULT 'github',
    forge_repo_id  text,
    owner          text        NOT NULL,
    name           text        NOT NULL,
    default_branch text        NOT NULL DEFAULT 'main',
    -- GitHub App installation this repo is reachable through. Installation
    -- ACCESS tokens are minted per job and never persisted; this is just the
    -- installation's identifier.
    installation_id text,
    -- Per-repo run numbering. Bumped under the row lock taken when creating a
    -- run, so run numbers are gapless and monotonic per repo.
    run_counter    bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, forge, owner, name)
);

CREATE TABLE workflows (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id     uuid        NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name        text        NOT NULL,
    path        text        NOT NULL,
    -- Raw YAML as fetched, plus the parsed form. Keeping the source lets the
    -- UI show the exact bytes a run was produced from, and lets the parser's
    -- positional errors point at something that still exists.
    source      text,
    definition  jsonb,
    content_sha text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_id, path)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Runners
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE runners (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name            text        NOT NULL,
    -- Authoritative labels, copied from the registration token at enrollment.
    -- Never from the RegisterRequest: a stolen token must not be able to
    -- self-assign privileged labels beyond what it was minted with.
    labels          text[]      NOT NULL DEFAULT '{}',
    executors       text[]      NOT NULL DEFAULT '{}',
    os              text,
    arch            text,
    runner_version  text,
    features        text[]      NOT NULL DEFAULT '{}',
    capacity        integer     NOT NULL DEFAULT 1 CHECK (capacity > 0),
    status          text        NOT NULL DEFAULT 'offline'
                                CHECK (status IN ('online', 'draining', 'offline')),
    last_heartbeat_at timestamptz,
    last_session_id text,
    registered_at   timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX runners_org_status_idx ON runners (org_id, status);
CREATE INDEX runners_heartbeat_idx  ON runners (last_heartbeat_at)
    WHERE status IN ('online', 'draining');

-- ─────────────────────────────────────────────────────────────────────────────
-- Runs, jobs, steps
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE runs (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id        uuid        NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    workflow_id    uuid        REFERENCES workflows (id) ON DELETE SET NULL,
    number         bigint      NOT NULL,
    status         text        NOT NULL DEFAULT 'pending'
                               CHECK (status IN ('pending', 'running', 'succeeded',
                                                 'failed', 'cancelled')),
    event          text        NOT NULL,
    ref            text,
    sha            text,
    actor          text,
    -- Which surface asked for this run. Mirrors audit_events.surface so a run
    -- can be traced back to the click, slash command, or webhook that caused it.
    trigger_source text        NOT NULL DEFAULT 'api'
                               CHECK (trigger_source IN ('web', 'chat', 'mcp', 'api',
                                                         'forge', 'system')),
    -- Fork-PR policy input. A fork run gets no secrets, no deploy identity, and
    -- only runners carrying a tag the operator marked fork-safe.
    is_fork        boolean     NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    started_at     timestamptz,
    finished_at    timestamptz,
    UNIQUE (repo_id, number)
);

CREATE INDEX runs_repo_created_idx ON runs (repo_id, created_at DESC);
CREATE INDEX runs_status_idx       ON runs (status) WHERE status IN ('pending', 'running');

CREATE TABLE jobs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    name            text        NOT NULL,

    -- 'pending' means blocked on a `needs` edge; 'queued' means eligible for
    -- claim. Only 'queued' rows are visible to the claim query, which is why
    -- the claim index is partial on that value.
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'queued', 'claimed', 'running',
                                                  'succeeded', 'failed', 'cancelled',
                                                  'dead_letter')),
    executor        text        NOT NULL DEFAULT 'docker'
                                CHECK (executor IN ('docker', 'shell')),
    -- Tags a runner must carry to be allowed this job: required_tags <@ runner.labels.
    required_tags   text[]      NOT NULL DEFAULT '{}',
    -- Lower claims first (see jobs_claim_idx's ORDER BY priority, created_at):
    -- this is a queue position, not a point score. Set below 100 to move a job
    -- ahead of the default queue, above 100 to push it back.
    priority        integer     NOT NULL DEFAULT 100,
    timeout_seconds integer     NOT NULL DEFAULT 3600 CHECK (timeout_seconds > 0),
    env             jsonb       NOT NULL DEFAULT '{}',
    -- Secret NAMES only. Values are never stored on a job: they are fetched by
    -- the runner through GetJobSecrets, authorized against the live lease.
    secret_refs     text[]      NOT NULL DEFAULT '{}',
    git_clone_url   text,
    git_ref         text,
    git_sha         text,

    -- ── Queue and lease state ────────────────────────────────────────────────
    -- lease_id is the execution authority. It is regenerated on every claim, so
    -- (job_id, lease_id) distinguishes a legitimate re-run from a zombie runner
    -- reporting against a lease that has since been revoked.
    lease_id          uuid,
    claimed_by        uuid        REFERENCES runners (id) ON DELETE SET NULL,
    claimed_at        timestamptz,
    lease_expires_at  timestamptz,
    attempts          integer     NOT NULL DEFAULT 0,
    max_attempts      integer     NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    -- Retry backoff: a requeued job is invisible to the claim query until now()
    -- reaches available_at.
    available_at      timestamptz NOT NULL DEFAULT now(),

    -- ── Result and stream watermarks ─────────────────────────────────────────
    exit_code           integer,
    error_class         text,
    error_detail        text,
    -- Highest contiguous sequence numbers durably accepted, per stream. These
    -- are what ServerHello.resume_verdicts replays from after a reconnect.
    status_watermark    bigint      NOT NULL DEFAULT 0,
    log_watermark       bigint      NOT NULL DEFAULT 0,

    -- The lease that last held this job, retained after completion. The bulk
    -- log lane is a separate gRPC stream from the control lane, and the two
    -- have no ordering guarantee relative to each other: a job's final log
    -- chunks routinely arrive after its terminal status. Without a record of
    -- the lease they were produced under, those trailing chunks fail
    -- authorization and are dropped -- losing precisely the last lines of
    -- output, which are the ones worth reading. See log_entries ingest.
    last_lease_id       uuid,

    cancel_requested_at timestamptz,
    cancel_reason       text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    started_at          timestamptz,
    finished_at         timestamptz,

    -- A job is claimed if and only if it holds a lease. Catches any code path
    -- that sets one half of the pair and forgets the other.
    CONSTRAINT jobs_lease_consistency CHECK (
        (status IN ('claimed', 'running')) = (lease_id IS NOT NULL)
    )
);

-- The claim index. Partial on 'queued' so it stays proportional to the queue
-- depth rather than to the history of every job ever run, and ordered to match
-- the claim query's ORDER BY exactly so the scan can stop at LIMIT.
CREATE INDEX jobs_claim_idx ON jobs (priority, created_at)
    WHERE status = 'queued';

-- Sweeper index: finds leases that expired without a terminal status.
CREATE INDEX jobs_lease_expiry_idx ON jobs (lease_expires_at)
    WHERE status IN ('claimed', 'running');

CREATE INDEX jobs_run_idx        ON jobs (run_id);
CREATE INDEX jobs_claimed_by_idx ON jobs (claimed_by) WHERE claimed_by IS NOT NULL;

-- `needs` edges. The DAG lives here rather than in a jobs column so that
-- "which jobs just became eligible" is an index lookup, not a scan.
CREATE TABLE job_dependencies (
    job_id            uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    depends_on_job_id uuid NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    PRIMARY KEY (job_id, depends_on_job_id),
    CONSTRAINT job_dependencies_no_self_edge CHECK (job_id <> depends_on_job_id)
);

CREATE INDEX job_dependencies_depends_on_idx ON job_dependencies (depends_on_job_id);

CREATE TABLE steps (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id          uuid        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    step_index      integer     NOT NULL CHECK (step_index >= 0),
    name            text        NOT NULL,
    -- Digest-pinned where the workflow provided a digest ("img@sha256:...").
    image           text,
    command         text[]      NOT NULL DEFAULT '{}',
    env             jsonb       NOT NULL DEFAULT '{}',
    timeout_seconds integer     NOT NULL DEFAULT 0 CHECK (timeout_seconds >= 0),
    -- Default-off network posture is set by the server, not requested by the job.
    network_enabled boolean     NOT NULL DEFAULT false,
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'running', 'succeeded',
                                                  'failed', 'skipped', 'cancelled')),
    exit_code       integer,
    started_at      timestamptz,
    finished_at     timestamptz,
    UNIQUE (job_id, step_index)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Streams: status updates, logs, structured events
-- ─────────────────────────────────────────────────────────────────────────────

-- Idempotent status upserts keyed (job_id, seq), exactly as the protocol
-- specifies. A redelivered StatusUpdate after a reconnect hits the primary key
-- and is a no-op rather than a duplicate state transition.
CREATE TABLE job_status_updates (
    job_id      uuid        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq         bigint      NOT NULL CHECK (seq > 0),
    lease_id    uuid        NOT NULL,
    state       text        NOT NULL
                            CHECK (state IN ('preparing', 'running_step', 'uploading',
                                             'succeeded', 'failed', 'cancelled')),
    step_index  integer,
    exit_code   integer,
    error_class text,
    detail      text,
    -- Advisory. The server clock decides ordering; this is kept for diagnosing
    -- clock skew on runners.
    runner_ts   timestamptz,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, seq)
);

-- Log chunks and structured events share ONE sequence space per job, so that a
-- single watermark covers both and replay ordering after a reconnect is total.
-- That is a protocol invariant, so it is expressed as a primary key here rather
-- than left to application code across two tables.
CREATE TABLE log_entries (
    job_id      uuid        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq         bigint      NOT NULL CHECK (seq > 0),
    kind        text        NOT NULL CHECK (kind IN ('chunk', 'event')),
    step_index  integer     NOT NULL,
    -- chunk rows
    io          text        CHECK (io IN ('stdout', 'stderr')),
    -- Redacted at ingest, before this insert. Nothing unredacted is ever
    -- written here.
    data        bytea,
    -- event rows
    event_kind  text,
    event_json  jsonb,
    runner_ts   timestamptz,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, seq),
    CONSTRAINT log_entries_shape CHECK (
        (kind = 'chunk' AND data IS NOT NULL AND io IS NOT NULL
            AND event_kind IS NULL AND event_json IS NULL)
        OR
        (kind = 'event' AND event_kind IS NOT NULL AND event_json IS NOT NULL
            AND data IS NULL AND io IS NULL)
    )
);

CREATE INDEX log_entries_step_idx ON log_entries (job_id, step_index, seq);

-- Where a job's logs went after the hot rows were compacted and pruned. Reads
-- fall through to S3 transparently when the rows are gone.
CREATE TABLE log_archives (
    job_id       uuid        PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
    object_key   text        NOT NULL,
    byte_size    bigint      NOT NULL,
    entry_count  bigint      NOT NULL,
    compacted_at timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Secrets
-- ─────────────────────────────────────────────────────────────────────────────

-- Write-only from the API's point of view: there is no endpoint that returns
-- `ciphertext`, and the only read path is the job dispatch path behind a live
-- lease. Envelope encryption: the KMS-wrapped data key is stored beside the
-- payload it encrypts, so key rotation is a per-row rewrap.
CREATE TABLE secrets (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id             uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    repo_id            uuid        REFERENCES repos (id) ON DELETE CASCADE,
    environment        text,
    name               text        NOT NULL,
    scope              text        NOT NULL CHECK (scope IN ('org', 'repo', 'environment')),
    ciphertext         bytea       NOT NULL,
    encrypted_data_key bytea       NOT NULL,
    kms_key_id         text        NOT NULL,
    version            integer     NOT NULL DEFAULT 1,
    created_by         text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT secrets_scope_shape CHECK (
        (scope = 'org'         AND repo_id IS NULL     AND environment IS NULL)
        OR (scope = 'repo'     AND repo_id IS NOT NULL AND environment IS NULL)
        OR (scope = 'environment' AND repo_id IS NOT NULL AND environment IS NOT NULL)
    )
);

-- Uniqueness across a nullable scope.
--
-- Postgres treats NULL as distinct from every other NULL for uniqueness
-- purposes, so a plain UNIQUE(org_id, repo_id, environment, name) would let
-- two org-scoped secrets (both with repo_id/environment NULL) silently share
-- a name — the exact duplicate this index exists to prevent. The nullable
-- columns are folded to fixed sentinels here so NULL compares equal to NULL
-- for this purpose, rather than relying on NULLS NOT DISTINCT (PG15+), so
-- this works on any supported version.
CREATE UNIQUE INDEX secrets_scope_name_idx ON secrets (
    org_id,
    coalesce(repo_id, '00000000-0000-0000-0000-000000000000'::uuid),
    coalesce(environment, ''),
    name
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Tokens
-- ─────────────────────────────────────────────────────────────────────────────

-- Every bearer secret in the system, hashed, in one place: runner registration
-- tokens, runner credentials, per-job tokens, MCP tokens, API tokens. One table
-- means one revocation path, one expiry check, and one place to audit a use.
--
-- `token_hash` is SHA-256 of the presented value. The value itself is shown to
-- the operator once, at mint time, and is not recoverable from here.
CREATE TABLE tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL
                             CHECK (kind IN ('registration', 'runner', 'job', 'mcp', 'api')),
    token_hash   bytea       NOT NULL UNIQUE,
    org_id       uuid        REFERENCES orgs (id) ON DELETE CASCADE,
    -- Subject: at most one of these is set, depending on kind.
    runner_id    uuid        REFERENCES runners (id) ON DELETE CASCADE,
    repo_id      uuid        REFERENCES repos (id) ON DELETE CASCADE,
    run_id       uuid        REFERENCES runs (id) ON DELETE CASCADE,
    job_id       uuid        REFERENCES jobs (id) ON DELETE CASCADE,
    -- Scoped claims. For a registration token this carries the authoritative
    -- labels the enrolled runner will receive; for a job token, the run/repo/
    -- branch/env claims that bound what it can do.
    claims       jsonb       NOT NULL DEFAULT '{}',
    -- Registration tokens may be minted for a limited number of enrollments.
    max_uses     integer     NOT NULL DEFAULT 1 CHECK (max_uses > 0),
    uses         integer     NOT NULL DEFAULT 0 CHECK (uses >= 0),
    expires_at   timestamptz,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    created_by   text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tokens_kind_org_idx  ON tokens (kind, org_id);
CREATE INDEX tokens_runner_idx    ON tokens (runner_id) WHERE runner_id IS NOT NULL;
CREATE INDEX tokens_job_idx       ON tokens (job_id)    WHERE job_id IS NOT NULL;
CREATE INDEX tokens_expiry_idx    ON tokens (expires_at) WHERE revoked_at IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- Audit
-- ─────────────────────────────────────────────────────────────────────────────

-- Every state-changing action from every surface lands here. `payload_hash`
-- rather than the payload: the audit trail must be safe to read widely, and
-- payloads can contain secret material.
CREATE TABLE audit_events (
    id             bigserial   PRIMARY KEY,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    org_id         uuid        REFERENCES orgs (id) ON DELETE SET NULL,
    principal_type text        NOT NULL
                               CHECK (principal_type IN ('user', 'runner', 'agent',
                                                         'token', 'system')),
    principal_id   text        NOT NULL,
    surface        text        NOT NULL
                               CHECK (surface IN ('web', 'chat', 'mcp', 'api',
                                                  'forge', 'system')),
    action         text        NOT NULL,
    subject_type   text,
    subject_id     text,
    payload_hash   bytea,
    metadata       jsonb       NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_events_org_time_idx   ON audit_events (org_id, occurred_at DESC);
CREATE INDEX audit_events_subject_idx    ON audit_events (subject_type, subject_id, occurred_at DESC);
CREATE INDEX audit_events_principal_idx  ON audit_events (principal_type, principal_id, occurred_at DESC);

-- +goose StatementEnd

-- ─────────────────────────────────────────────────────────────────────────────
-- Queue doorbell
-- ─────────────────────────────────────────────────────────────────────────────

-- +goose StatementBegin
-- NOTIFY is delivered at COMMIT, not at statement time. That is exactly the
-- behavior the queue wants: the doorbell rings only once the job is actually
-- visible to the claim query, so a dispatcher woken by it can never observe an
-- empty queue for a job it was just told about.
CREATE FUNCTION cicado_notify_job_queued() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(
        'cicado_job_queued',
        json_build_object('job_id', NEW.id, 'tags', NEW.required_tags)::text
    );
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- available_at is checked here too, not just by the claim query: a backoff
-- Requeue sets status = 'queued' immediately but available_at in the future,
-- and without this the doorbell would ring for a job no claim can pick up
-- yet — harmless (NOTIFY is fire-and-forget and the tick still covers
-- correctness), but a wasted dispatcher pass for every backoff-delayed retry.
CREATE TRIGGER jobs_queued_notify
    AFTER INSERT OR UPDATE OF status ON jobs
    FOR EACH ROW
    WHEN (NEW.status = 'queued' AND NEW.available_at <= now())
    EXECUTE FUNCTION cicado_notify_job_queued();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS jobs_queued_notify ON jobs;
DROP FUNCTION IF EXISTS cicado_notify_job_queued();
DROP TABLE IF EXISTS audit_events, tokens, secrets, log_archives, log_entries,
                     job_status_updates, steps, job_dependencies, jobs, runs,
                     runners, workflows, repos, orgs CASCADE;
-- +goose StatementEnd
