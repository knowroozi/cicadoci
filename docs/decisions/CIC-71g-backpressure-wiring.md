# CIC-71g — Backpressure tracker wired into dispatch loop

**Ticket:** CIC-71 (sub-blocker 7)
**PR:** `knowroozi/cic-71g-backpressure-wiring`
**Date:** 2026-09-28
**Branch base:** `knowroozi/cic-71f-audit-wiring` (stacked)

---

## Decision

Add `*backpressure.Tracker` to `dispatch.Config` and call `Tracker.Acquire`
in `dispatchOnce` for every claimed job.  When `Acquire` returns `false`
(over the cap), the job is requeued with `UndeliveredBackoff` and the
loop continues to the next job.  A `nil` tracker is a no-op —
backends that don't set it behave identically to pre-CIC-71g.

## `job.RunID.String()` as the cap key

`ClaimedJob` does not yet carry a `RepoID` or `OrgID` — only `RunID`.
The `Acquire(ctx, job.RunID.String(), "")` call therefore tracks a
**per-run concurrency limit**, not a per-repo or per-org limit.  With run-scoped
keys the cap effectively limits concurrent runs across the fleet, not per-repo.

A future ticket (CIC-39 or a new one) should add `RepoID`/`OrgID` to
`ClaimedJob`, update the claim SQL to join `runs` and `orgs`, and switch
`Tracker.Acquire` to the real `repoID`/`orgID` keys.

## Release semantics

`Tracker.Acquire` increments the counter when it returns `true`.  The caller
MUST pair that with `Tracker.Release` when the job finishes or is undelivered.

**Undelivered-job path (runner unreachable between claim and send):**
`Release(job.RunID.String(), "")` is called immediately after `Requeue`, so the
counter slot is freed even though the job never ran.

**Success path:** the job is assigned to a runner and the dispatch loop moves on.
The counter slot is held for the duration of the job.  **Release on job
completion** is deferred to a future "job finished" callback — in the current
MVP, accepted-and-running jobs hold their counter slot until the tracker is
reset on restart or until a future callback arrives.  This makes the cap
conservative: in steady state with many concurrent runs the counter
monotonically increases until the cap is hit, at which point new jobs are
requeued even though some running jobs may have already completed.

## `DefaultConfig`

`DefaultConfig` does **not** set `Backpressure` in `dispatch.Config`.
The CIC-71d server wiring passes it explicitly:

```go
bp := backpressure.NewTracker(backpressure.DefaultLimits())
dispatcher := dispatch.New(q, svc.Sessions(), dispatch.Config{
    Backpressure:       bp,
    UndeliveredBackoff: 15 * time.Second,
}, log)
```

## Scope

### Done
- `*backpressure.Tracker` in `dispatch.Config`
- `dispatchOnce`: calls `Acquire` after `Claim`; requeues on `false`
- `Release` on the undelivered-job requeue path (runner-unreachable)
- Fail-open on `Acquire` errors (logs `Warn`, still assigns job)
- `gofmt` / `golangci-lint` clean; all existing tests pass
- No regression: `nil` tracker is a no-op

### Deferred (separate ticket)
- `RepoID`/`OrgID` in `ClaimedJob` for true per-repo cap
- `Release` on job completion callback — wire when the terminal
  status-update event is available in the dispatch layer
- `PostgresAuditLog` — separate ticket
