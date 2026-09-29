## CIC-72 — Migration 0003: `artifacts` table

**Date:** 2026-09-28

**Problem:** `server/artifacts/postgres.go` documents its dependency on
"migration 0003" for the `artifacts` table, but no such migration exists in
`server/store/migrations/`. Any call to a `Postgres` method would fail with
`relation "artifacts" does not exist` at runtime.

**Decision:** Add `0003_artifacts.sql` matching the columns the
`Postgres` struct's SQL queries reference: id (uuid PK), org_id (FK orgs),
run_id (FK runs), job_id (FK jobs, nullable), name, blob_key, size_bytes,
content_type, uploaded_at (default now()), expires_at (nullable),
created_at (default now()). Three indexes: (run_id, created_at DESC) for
run-level listing, (run_id, name) partial-on-job_id-null for
`GetByRunAndName`, and (expires_at) partial-on-not-null for
`FindExpired`. Down migration drops the table.

A `server/artifacts/postgres_test.go` verifies the table exists and is
usable via `testdb.New(t)` against `CICADO_TEST_DATABASE_URL` in CI.

The `Postgres` struct's `orgResolver` parameter (maps `org_id` → slug for
blob keying) is unchanged; it is wired in when the run-creation path calls
`Create` (deferred to CIC-71 main.go wiring).
