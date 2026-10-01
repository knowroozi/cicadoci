# CIC-78 — wire the store-backed workflow loader; fix the two silent job-creation gaps

**Ticket:** CIC-78 (child of CIC-71, the "runs but never jobs" audit finding)
**PR:** `knowroozi/cic-78-workflow-loader-wiring`
**Date:** 2026-09-30
**Base:** master (rebased to origin/master tip)

---

## The finding

Dwight's QC audit (CIC-78) found that the production server accepts webhook
events and creates `runs` rows, but **never creates jobs**. Three
independent seams were all broken at once:

1. **`main.go` left `WorkflowLoader` nil.** `controller.New(st, NoOpForge{},
   controller.DefaultConfig(), log)` used `DefaultConfig()`, which sets
   `WorkflowLoader: nil`. With a nil loader, `LoadAndCreateJobs` is a silent
   no-op by design (CIC-71c) — runs are created, jobs are not.
2. **`NewStoreLoader` was only used in tests.** CIC-71e added it but nothing
   in `main.go` instantiated it, so the store-backed loader was dead code on
   the real server path.
3. **Nothing wrote the `workflows` table.** `Store.ListWorkflowsForRepo`
   existed (a read) but there was no write path, so even a wired loader had an
   empty table to read.

A fourth gap was discovered while proving the fix end-to-end:

4. **`CreateJobSpecs` wrote a NULL `secret_refs`.** `convertJobSpecs` left
   `JobSpec.SecretRefs` nil, and `CreateJobSpecs` passed it straight into the
   `jobs.secret_refs` column, which is `NOT NULL DEFAULT '{}'`. pgx serialised
   the nil slice as SQL NULL, so the `INSERT` violated the NOT NULL
   constraint and job creation failed for **every** job, even when a workflow
   loaded. (The same latent risk applied to `required_tags`.)

## Decision

### 1 + 2 — wire the loader in `main.go`

Build the controller's `Config` via `DefaultConfig().WithAudit(...)` and set
`WorkflowLoader = controller.NewStoreLoader(st, log)` before constructing the
controller. The store loader reads the `workflows` table; an empty table means
"no jobs," which is the safe default CIC-71c chose for a nil loader. Wiring the
loader therefore turns the silent no-op on for any repo that has a workflow
row, without changing behaviour for repos that don't.

### 3 — add `Store.UpsertWorkflow` + a dev-seed producer

`server/store/workflows.go` adds `WorkflowInput` and `Store.UpsertWorkflow`,
an idempotent upsert keyed on `UNIQUE(repo_id, path)`: re-registering a path
overwrites name, source, definition, and content_sha and bumps `updated_at`,
matching the StoreLoader's "latest version of each path" read. `definition` is
the JSON-encoded `workflow.Workflow`; `source` is the raw YAML for the UI.

Producers call it; CIC-78 supplies one in `-dev-seed`: `seedDemoWorkflow`
parses and validates a trivial push-triggered workflow through the *same
parse+marshal seam* a production producer would use, then persists it. This
exercises the write path in the dev stack, so `make dev` now yields a run that
produces a job DAG, not just a bare run.

The **production producer is deferred** — the GitHub-raw fetch that fills the
`workflows` table lands with CIC-84 (the GitHub-raw workflow producer, god-approved). The loader
and the write method are the permanent seams; the dev-seed is a stand-in so the
path is reachable and tested today.

### 4 — guard nil slices in `CreateJobSpecs`

`CreateJobSpecs` now passes `saFor(sp.SecretRefs)` and
`saFor(sp.RequiredTags)` (nil → `[]string{}`), mirroring the existing `envFor`
idiom. A job with no secret refs or no tags is the empty-array case, never
NULL, so job creation no longer fails on the NOT NULL constraints.

## Why these seams, not one big ingest subsystem

The loader (read), the upsert (write), and producer are deliberately separate.
The read and write are the permanent contracts; any producer (dev-seed, a CLI
`ingest` subcommand, or the GitHub-raw fetch) can fill the table and every
server picks it up. Keeping producers out of the loader is what let CIC-71c add
the store loader without a rewire, and it keeps the data layer free of the
GitHub HTTP client.

## Out of scope (follow-ups)

- **CIC-84 (Urgent, god-approved, filed 2026-09-30 by Dwight) — production ingest source:** the GitHub-raw fetch of a workflow file that fills
     the `workflows` table for real production runs. Per god c6e323 this is
     NOT CIC-38/39/40; it must land before CIC-82/83 so production creates
     jobs, not just runs. Until it exists only `-dev-seed` populates the
     table.
- A `cicado ingest-workflow` CLI subcommand for ops: natural next step,
  not blocking.
- **CIC-85** — run finalisation when zero workflows match: if a run is created but no
  workflow matching the trigger exists, the run must be marked `cancelled`/`skipped`
  rather than left `pending` forever. CIC-78 wired the loader but did not implement the
  terminalise-on-zero-match path; tracked as **CIC-85**, not blocking this PR.
- Revision-pinned loading (load the workflow at the run's exact SHA rather than
  "latest in table"): the StoreLoader's `sha` argument is currently ignored; a
  future revision-aware loader uses it.

## Verified

- `gofmt`, `go vet` clean across touched packages.
- `golangci-lint v2.13.2`: 0 issues on `server/store`, `server/controller`,
  `server/cmd/...`.
- Offline `go test ./...`: all pass (PG-gated tests skip).
- PG-gated `go test -race ./server/store/ ./server/controller/`: all pass,
  including the new `TestUpsertWorkflowRoundTrip`,
  `TestUpsertWorkflowRequiresRepoAndPath`, and
  `TestLoadAndCreateJobsViaStoreLoader`.
    - PG-gated `go test -race -run TestDevSeedProducesClaimableJob ./server/cmd/cicado-server/`: all pass, proving the requested "dev-seed signed webhook -> job claimed" acceptance run.

## Unverified

- The `-dev-seed` path exercised by hand (`make dev` against a live throwaway
  stack): the YAML is unit-checked to parse+validate, and the upsert/load path
  is PG-tested, but the full `make dev` loop was not driven end-to-end.

## Knowledge-graph currency

`.ua/` artifacts refreshed in this PR; graph commit tracked in `.ua/meta.json`.
