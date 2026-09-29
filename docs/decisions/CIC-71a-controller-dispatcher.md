# CIC-71a — Controller: real webhook → run-creation dispatcher

**Ticket:** CIC-71 (sub-blocker 1)
**PR:** knowroozi/cic-71a-controller-dispatcher
**Date:** 2026-09-28

## Decision

Introduce `server/controller` as the first real `webhook.Dispatcher`
implementation, replacing `webhook.NoOpDispatcher`. The controller
creates a `runs` row on push/PR events and reports it to the configured
`forge.Forge`, gated by `backpressure.Tracker`.

## Rationale

The `NoOpDispatcher` means no runs are ever created from webhook
events; the whole M3 feature (webhook → checks reporting) was dormant.
The controller is the minimum viable integration point that proves
the dispatch pipeline works end-to-end without requiring a real
GitHub App.

## Alternatives considered

1. **Wipe and rebuild from scratch:** rejected — the subsystem packages
    (`forge`, `backpressure`, `store`) are already well-tested and
    stable; they just needed a coordinator.
2. **Wire into `dispatch.Dispatcher` directly:** rejected — the
    dispatcher is for the runner↔server session path; run *creation*
    from external triggers is a separate concern that should be its
    own package to avoid import cycles.

## Non-goals

- Real org/repo resolution (CIC-71b — `FindOrgBySlug` + repo lookup
    by org ID + installation ID)
- Workflow loading and job creation (CIC-71c — load YAML file from
    repo, create `JobSpec` DAG, call `CreateJobSpecs`)
- Check run `rerequested` handling (CIC-71c — resolve check run ID
    back to a run and re-enqueue its jobs)
- Main.go wiring (CIC-71d — replace `webhook.NoOpDispatcher{}` with
    `controller.New(...)` in `cmd/cicado-server/main.go`)

## Consequences

- `server/store/runs.go` adds `CreateRun` and `CreateJobSpecs` (the
    job DAG writer). These are atomic and safe to use from any
    goroutine.
- The controller stub is *not wired into main.go* yet (CIC-71d).
- `resolveRepo` uses a deterministic pseudo-UUID (SHA-1 of the org
    name) as a stand-in for a real org lookup; this is clearly marked
    as a TODO and will not survive a production install without real
    org registration.

## Open questions

1. Should `CreateRun` return the full `store.Run` type (via
    `RETURNING *`) or just the ID + number?
2. What happens to a run that fails `forge.Prepare`? Currently the
    store row is created but the check is "orphaned" (no check exists).
    Should we delete the run on forge failure, or keep it as a
    "run started but not reported" state?

---

## Review fixes applied (2026-09-28)

Four blockers from CLAUDE.md review were fixed in commit `9949e54`:

1. **`FindOrCreateRepo` `ON CONFLICT` target** — Changed
   `ON CONFLICT (org_id, owner, name)` to
   `ON CONFLICT (org_id, forge, owner, name)` to match the real
   4-column composite constraint from migration `0001_init.sql`.
   `FindOrCreateRepo` now accepts a `forge` parameter.

2. **`resolveRepo` returned org ID as repo ID** — Changed `resolveRepo`
   to return both `orgID` and `repoID`. Callers now use `repoID` in
   `CreateRunOptions.RepoID`. This was a copy-paste bug: the org
   UUID was passed where the repo UUID was expected, causing
   `CreateRun`'s `UPDATE repos WHERE id = $1` to match zero rows.

3. **Backpressure args swapped** — `Acquire(ctx, orgID, owner)`
   changed to `Acquire(ctx, repoID, orgID)` to match the real
   `backpressure.Tracker.Acquire` signature. Per-repo isolation now
   actually works.

4. **Fork resolution** — `HandlePullRequest` now resolves the repo
   from `payload.Repository.FullName` (base) instead of
   `payload.Head.Repo.FullName` (fork). This prevents any GitHub
   user from auto-provisioning an org row by opening a fork PR.

All four are now verified against the real schema and real API
signatures.
