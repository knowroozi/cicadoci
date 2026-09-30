# CIC-71d — wire the controller into `cicado-server`

**Ticket:** CIC-71 (sub-blocker 4)
**PR:** `knowroozi/cic-71d-server-wiring`
**Date:** 2026-09-28
**Branch base:** `knowroozi/cic-71c-workflow-dag` (stacked)

---

## Decision

Replace `webhook.NoOpDispatcher{}` in `cmd/cicado-server/main.go`
with `controller.New(st, NoOpForge{}, controller.DefaultConfig(), log)`.

## Why `NoOpForge`

The controller requires a `forge.Forge` (used by `report()` to create
a check in the code host).  No real GitHub forge exists yet:
`CIC-38/39/40` will add `forge.NewGitHubForge()` with a
`ChecksReporterAdapter` and `InstallationTokenMinterAdapter`, both
of which require a live GitHub App configuration (webhook secret,
installation access).  For now the NoOp forge returns zero/nil for
all methods and produces no side effects — the controller logs a
debug message when the run is created, but no external HTTP call
is made.

## What is NOT wired yet

The following are intentionally left un-wired in CIC-71d:

- **`WorkflowLoader`** — `DefaultConfig().WorkflowLoader` is `nil`,
  so `LoadAndCreateJobs` is a silent no-op.  Runs are created but
  no jobs are enqueued.  A `StoreLoader` that reads the
  `workflows` table (added in CIC-71e) or a `GitHubLoader` that
  fetches the workflow YAML at a specific SHA via installation
  tokens (CIC-38/39/40) will be wired in a later sub-blocker.

- **Audit event** — the audit package is wired into the store's
  chokepoint but not yet into the controller.  CIC-71f adds the
  audit event to the `HandlePush`/`HandlePullRequest` handlers.

- **`dispatch.New` / queue** — already wired in CIC-11; no change.

## Scope

### Done
- `controller.NoOpForge` — zero-side-effect `forge.Forge`
- `main.go` replaces `webhook.NoOpDispatcher{}` with
  `controller.New(st, NoOpForge{}, controller.DefaultConfig(), log)`
- `webhook.NewReceiver` dispatches to the controller
- `golangci-lint` clean on all touched packages

### Deferred
- `StoreLoader` (CIC-71e) — reads workflows from the DB
- `githubForge` with `ChecksReporterAdapter`
   (CIC-38/39/40) — real checks API + token minter
- Audit event in controller (CIC-71e or CIC-71f)

### Open questions
1. Should `NoOpForge` be logged once at startup so the operator
   knows no external checks are being created?
   → Yes; CIC-38/39/40 adds a startup log line.
2. Is `DefaultConfig()`' `WorkflowLoader: nil` acceptable as a
   default, or should it default to `StoreLoader{}`?
   → `nil` is correct: a nil loader means "no jobs for this run",
   which is the safe default (no spurious job runs).  The operator
   wires a loader when they configure workflows.
