# CIC-71f — Audit events for run creation in the controller

**Ticket:** CIC-71 (sub-blocker 6)
**PR:** `knowroozi/cic-71f-audit-wiring`
**Date:** 2026-09-28
**Branch base:** `knowroozi/cic-71e-store-workflow-loader` (stacked)

---

## Decision

Add an `audit.Log` to the controller's `Config`, and emit an
`action=create_run` audit entry whenever a run is successfully
created in `HandlePush` or `HandlePullRequest`.

## `ActionCreateRun` — new audit action

A new action constant `audit.ActionCreateRun = "create_run"` was added.
Existing actions (`approve`, `cancel`, `rerun`, `deploy`, `rollback`,
`secret.put`, `secret.delete`) cover the post-creation lifecycle;
creating the run itself had no corresponding audit entry.  This closes
that gap.

## Non-fatal audit failures

`emitAudit` is a fire-and-forget pattern: if `audit.Append` fails,
it logs a `Warn` but does not block the run.  This is the right
pattern because:

1. The run row was already committed; the run's state is correct.
2. Audit failures are operational concerns, not correctness
   concerns.
3. Adding audit failure to the error return from
   `HandlePush/HandlePullRequest` would propagate through the
   webhook handler and cause GitHub to re-deliver the event.

## `Controller.Config.Audit` field

`Config` now has an `Audit audit.Log` field.    `DefaultConfig()`
leaves it nil; callers must explicitly wire one via the WithAudit()
builder.    `main.go` wires a durable `audit.NewPostgresLog(st.Pool())`
since `PostgresLog` merged to master via CIC-75 / PR #73 — the run-
creation audit trail is hash-chained and survives restarts/deployments.

## Scope

### Done
- New `ActionCreateRun` constant in `server/audit/audit.go`
- `Config.Audit audit.Log` field
- `emitAudit` helper: append + log-on-failure
- Called in both `HandlePush` and `HandlePullRequest`
- `DefaultConfig()` is nil-safe; `main.go` wires `audit.NewPostgresLog(st.Pool())` for the durable path
- 2 new tests: nil-audit safety + MemoryLog append

### Deferred
- Per-state transition audit (job → running → completed) — out of
	scope; CIC-71 covers run-creation only
- Cancel/rerun/deploy audit (those actions are already in
	`audit.go`, just not yet called from the controller)

### Not part of this ticket
- Backpressure tracker in dispatch loop (CIC-71g)
- Chat/approvalgates integration (CIC-71h)
