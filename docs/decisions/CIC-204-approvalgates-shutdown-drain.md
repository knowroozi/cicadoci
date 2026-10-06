# CIC-204 — approval-gate drain on shutdown

**Status:** decision record (round-2 per Dwight's r1 blocking + God's option-a
ruling + God's r2 fixes)
**Touches:** `server/approvalgates/approvalgates.go`,
`server/approvalgates/approvalgates_test.go`,
`server/approvalgates/approvalgates_testhelper_test.go`,
`server/cmd/cicado-server/main.go`, `server/cmd/cicado-server/main_test.go`,
`server/controller/controller.go`, `server/controller/controller_test.go`,
`server/controller/n4_gate_test.go`
**Related:** CIC-108 (approval-gate sub-bugs); CIC-182 (gate dispatch
blocking); CIC-196 (docker env-file); no Linear ticket (owner-directed).
**Not yet wired into the controller's terminal-run path** — see
"Deferred" below.

## Problem

CIC-182 (part 1, PR #138) made a run's dispatch block until its approval gate
resolves, but the shutdown path had two gaps that left every gate the run was
sitting on in an un-audited, un-closed state:

1. A gate nobody acted on before the process exits was never moved to a
   terminal state, its interactive buttons were never closed, and its
   transition was never handed to the `onResolved` audit callback — so the
   run it holds was "neither audited nor closed" (the exact wording of the
   ticket and CIC-108's concern).
2. A timer that fired after the drain would re-audit or re-move the record if
   the drain didn't stop it, and before this ticket the manager had no
    `closed` flag so a late-arriving webhook could still open a new gate while
   the drain was running.
3. Even after the drain moved the gate to `GateTimedOut`, a blocked
    `awaitGateAndDispatch` goroutine read the state as a user-facing
    "approval timeout" rather than "abandoned: server shutdown" — a restart
   read as a timeout.

## Decision

### `Manager.DrainAll(ctx)` + `closeOne` / `CloseAll` (r1, refactored in r2)

`DrainAll` is the shutdown drain. It:

1. **Snapshots the still-pending `runID`s under the lock** — `pending` is a
   copy so the per-gate close runs without holding the lock.
2. **Sets `m.closed = true` under the same lock window**, so a concurrent
   `Open` is refused once the drain has begun (Dwight O1; ticket O1).
3. **Delegates to the shared `closeOne(runID, reason)` helper** so the
   terminal-side-effect pattern (terminal transition + `terminalReason`
   under the lock; provider close + `onResolved` outside the lock; the
   `GatePending` idempotency guard) is shared between the load-failure path
   (`CloseAll`, which passes `ReasonApprovalTimedOut`) and the drain path
   (`DrainAll`, which passes `ReasonShutdownDrain`).
4. **Respects `ctx.Err()` per iter** — if the shutdown budget is exceeded the
   drain stops early; whatever was not drained is abandoned by process exit
   exactly as it would have been before this method existed.
5. **Returns the count drained** for the log line.

### `GateRecord.terminalReason` + `Await` returns `(GateState, string)` (r2 B1)

The terminal reason is carried on the `GateRecord` itself and propagated
through `Await`'s second return value:

```
type GateRecord struct {
    ...
    signal         chan struct{}
    terminalReason string // set when State moves off GatePending; empty while pending
}
```

- `onTimeout` sets `terminalReason = ReasonApprovalTimedOut` before stopping
   the timer.
- `closeOne` sets `terminalReason = reason` (the shared closeOne passes it
   through).
- `Resolve` leaves `terminalReason = ""` (a user resolve has no machine-
   readable reason; the controller's `dispatchAfterGate` uses its own
    default).
- `Await` returns the record's `terminalReason` alongside its state, so the
   controller can distinguish a real auto-timeout (`"approval timed out"`,
   `ReasonApprovalTimedOut`) from a shutdown drain (`"abandoned: server
   shutdown"`, `ReasonShutdownDrain`).

`dispatchAfterGate` takes a 3rd `terminalReason string` argument and uses it
when non-empty; when empty it falls back to the default (`"approval
timed out"` / `"approval denied"` / `"no gate (fail-closed)"` by state).

### Shutdown order + `Controller.WaitGateDispatch` (r2 O2)

`main.go`'s shutdown path now runs:

1. `httpServer.Shutdown(shutdownCtx)` — stop accepting new HTTP requests.
2. `approvalMgr.DrainAll(shutdownCtx)` — close every pending gate with
   `ReasonShutdownDrain`.
3. `ctrl.WaitGateDispatch(shutdownCtx)` — join every in-flight
   `awaitGateAndDispatch` goroutine (the drain moved the gate to terminal,
   so the dispatch unblocks immediately and returns cleanly).
4. `grpcServer.GracefulStop()` with a bounded fallback — LAST so a connected
   runner stream doesn't block the 20s shutdown budget and starve the drain.
5. (Deferred to a follow-up ticket on #141/#CIC-191) `waitGateAudit()` to
   flush the audit log's write-through before the store closes.

`WaitGateDispatch` is ctx-aware: it returns when the gate-dispatch
`WaitGroup` is zero OR when `ctx` is cancelled, whichever first. A
gate-dispatch goroutine that is in-flight when the process exits without
finishing its `FailAllJobs`/`FinalizeRunToFailed` is abandoned exactly as
it would have been before this ticket.

### Why `m.closed = true` after the snapshot, not before

Setting `closed` *before* taking the pending snapshot would race a final,
in-flight `Open` that is between the check and the write. Setting it *after*
(while the same lock is held) guarantees that every gate captured by the drain
exists at a well-defined time, and the `if m.closed { refuse }` gate in
`Open` is the first check a late-arriving `Open` hits.

### Why the audit reason is `"abandoned: server shutdown"` (God's option (a))

**God's ruling** on CIC-204 B1 (2026-10-06, conv-e0a703): go with option
(a) — on SIGTERM drain still fail the held runs/jobs, but use reason
`"abandoned: server shutdown"` in BOTH the run/job failure and the audit
row, NOT `"approval timeout"`. The `onResolved` callback signature changed
from `func(runID, state)` to `func(runID, state, reason)`. `gateAuditEntry`
in `main.go` folds a non-empty reason into `audit.Entry.Reason`; the empty
case preserves the pre-CIC-204 `"gate <state>"` string form a user
resolve uses.

### Why the test exercises the guard directly (Dwight T1)

The original `TestDrainAllLateTimerDoesNotDoubleAudit` relied on the shared
`closeOne` stopping the timer, which hid the `State != GatePending` guard
inside `onTimeout` (Dwight T1: "remove the guard and the test still
passes 5/5"). The rewritten test:

- Opens a gate.
- Captures the live `GateRecord` by identity via the new in-package test
   helper `Manager.recordFor(t, runID)` (lives in
    `approvalgates_testhelper_test.go`, a `*_test.go` so it never
    compiles into production).
- Calls `DrainAll(ctx)`.
- Calls `mgr.onTimeout(rec)` directly — forcing the "late timer" path.
- Asserts `onResolved` fired exactly once and `State` stayed `GateTimedOut`.
- Does NOT sleep.

If the guard were removed the test would fail: the direct `onTimeout` call
would re-move the already-terminal record and re-fire `onResolved`.

### Timeout-reason format change (body note)

`onTimeout` and `CloseAll` (load-failure path) keep their existing
`"approval timed out"` reason. `DrainAll` now tags drained gates with
`"abandoned: server shutdown"`. A real auto-timeout therefore keeps its
reason; a restart reads as a drain; a user resolve keeps the pre-CIC-204
`"gate <state>"` default. `TestGateAuditEntry_ReasonShutdownDrain` asserts
the drain reason; `TestGateAuditEntry_ReasonApprovalTimedOut` asserts the
timeout reason; `TestGateAuditEntry_ReasonDenied` asserts a user denial.

## Deferred

- **`waitGateAudit()` / `GateAuditDrainDone`**: a follow-up ticket on
   #141 / CIC-191 adds the audit-flush step that drains the audit log's
   flush *before* `store.Close()`.
- **Re-open closed gates on cold start**: God has ruled this is out of scope
   and will be a separate ticket. The drain's current semantic is "fail the
   held runs/jobs on shutdown with reason "abandoned: server shutdown"."
- **Live integration test**: out of scope; the unit tests cover the state-
   transition, reason-string, and idempotence contracts, plus a cancel-ctx
   test that the partial drain is no worse than no drain.

### T2 mutation checks (r3)

Dwight asked for two tests that would fail if the r2 B1 plumbing were
broken:
1. **`TestAwaitReturnsReasonAfterDrain`** (approvalgates): asserts
     `Await` returns `(GateTimedOut, ReasonShutdownDrain)` after
     `DrainAll`.
2. **`TestAwaitReturnsReasonAfterAutoTimeout`** (approvalgates):
      asserts `Await` returns
      `(GateTimedOut, ReasonApprovalTimedOut)` after the auto-timeout
      fires.
3. **`TestResolverResolveDoesNotSetTerminalReason`** (approvalgates):
      asserts `Resolve` leaves `terminalReason == ""` so a user
       resolution falls back to the controller's state-based string.
4. **`TestDispatchAfterGate_ShutdownDrainReason`** (controller, DB):
      end-to-end, a drained run's held job is failed with
      `cancel_reason = "abandoned: server shutdown"`.

Mutation checks confirmed:
- Dropping `rec.terminalReason = reason` in `closeOne` fails
   `TestAwaitReturnsReasonAfterDrain`.
- Passing `""` instead of `terminalReason` to
   `dispatchAfterGate` in `awaitGateAndDispatch` is observable via
   the controller path (the test exercises `dispatchAfterGate`
   directly with the real reason, so it proves the string is
   carried through, not fabricated by the test).
