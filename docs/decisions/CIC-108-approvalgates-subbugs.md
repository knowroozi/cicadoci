# Decision record: CIC-108 — approvalgates sub-bugs (concurrency + lifecycle)

**Status:** In Review — PR (CIC-108)
**Opened:** 2026-10-03
**Parent:** runtime queue (CIC-108): DW flagged that a gate may be denied by
policy but still fire the approval callback; audit the `approvalgates/` Manager
and its consumers.

## Sub-bugs (as scoped by the ticket), and where each lives

| # | Sub-bug | Location | Disposition in this PR |
|---|---------|----------|------------------------|
| 1 | Timeout never enforced | `approvalgates.Manager` | **Fixed** in this PR |
| 2 | Defensive by-value copy in `Resolve` | `approvalgates.Manager` | **Fixed** in this PR |
| 3 | Callback fired under the lock | `approvalgates.Manager` | **Fixed** in this PR |
| 4 | Gate recorded on a failed post | `approvalgates.Manager` | **Fixed** in this PR |
| 5 | Channel set to the PR number | `controller.maybeOpenGate` | **Scoped out** — follow-up |

## 1 — Timeout never enforced (fixed)

`Gate.Timeout` was handed to the provider but the manager itself never acted on
it, so a gate nobody resolved sat at `GatePending` forever. `CloseAll` existed
but had to be called explicitly. Now `Open` arms a `time.AfterFunc` when
`g.Timeout > 0` that transitions a still-pending gate to `GateTimedOut`,
closes its buttons, and fires the callback. A zero timeout means "no automatic
timeout" (not "invented one"). Any manual `Resolve`/`CloseAll` stops the timer,
so it can never overwrite a resolution.

## 2 — Defensive by-value copy in `Resolve` (fixed; defense in depth)

`Resolve` read `rec.State` (the live record in the shared map) *after*
releasing `m.mu`, then passed it to the callback. In practice this is not a live
data race: `rec.State` is written only on a `GatePending` → terminal transition,
and every such write is guarded by that same state under `m.mu`, so once a
terminate write has happened nothing ever writes that record's `State` again.
Keeping the by-value copy taken *under the lock* (`gateCopy := *rec`), passed to
the provider close and the callback, is still the correct shape as defense in
depth, and is what the review asks for (not a claim that a reachable race
today is covered by a test). The same pattern was copied into `onTimeout`.

## 3 — Callback fired under the lock (fixed)

`CloseAll` called `m.onResolved` while still holding `m.mu` (via
`defer m.mu.Unlock()`). A callback that re-enters the manager — e.g. a waiter
that calls `State` — deadlocked. Fix: do the provider close and fire the
callback *outside* the lock, using the by-value copies captured under it.
`Resolve` already did this; `CloseAll` now matches it.

## 4 — Gate recorded on a failed post (fixed)

`Open` inserted the gate record into `m.gates` *before* calling
`RequestApproval`, and on error returned without removing it. A post that fails
thus left a dead, unresolvable-but-reachable record and, with sub-bug #1, a
dangling timer. Fix: on `RequestApproval` error, `delete(m.gates, runID)` and
return the error before arming any timer, so a failed post leaves no half-open
gate.

## 6 — Stale timer from an earlier Open (fixed in review)

CIC-108 PR #118 review item B1: the auto-timeout was armed with
time.AfterFunc(… func() { m.onTimeout(runID) }) (by RUN ID, not by record
identity) and `Open` did not stop the previous record's timer. A second `Open`
for the same run (e.g. retry after cancel) overwrote `m.gates[runID]`, and the
first Open's timer, when it fired, looked up the *now-current, not-yet-timed-out
newer record* and moved THAT to GateTimedOut early. Fix:

- `Open` stops the previous record's timer before overwriting it; and
- `onTimeout` now takes the record by identity and acts only when
   `m.gates[runID] == rec` still holds, so a stray callback from a superseded
   timer is a no-op even if it outlives its `Stop()`.

`TestStaleTimerDoesNotTimeOutNewerGate` proves the fix: asserted to *fail
before* the fix (state clobbers to GateTimedOut at 120 ms) and pass after.

## 6b — N-items from the PR #118 review (all applied)

- N1: `TestCallbackRunsOutsideLock` now calls `go mgr.CloseAll(…)` so that, on a
     regression that returns the callback-under-lock deadlock, the deadlock happens
     in that goroutine and the test's 2-second guard fires as a fast failure rather
     than the test goroutine blocking until the `go test` package timeout.
- N2: `TestResolveBeatsTimeout` now asserts exactly one resolution callback and
     exactly one provider close, so a double-transition would be caught.
- N3: `Resolve` moved `rec.ActorID`/`rec.ResumedAt` assignments into the
     approve/deny switch cases, so an unknown-action click returns an error without
     recording that user as the actor of a still-pending gate.

---

## 5 — Channel set to the PR number (scoped out to a follow-up)

`controller.maybeOpenGate` sets
`Channel: fmt.Sprintf("%d", payload.PullRequest.Number)` — the gate message is
posted to the chat channel named `<PR-number>`, not a real channel — and leaves
`Gate.Timeout` at zero (so even a working auto-timeout, sub-bug #1, would never
fire here because the caller supplies no timeout).

This is a *controller*-side bug that needs a channel/timeout **source**, which
does not yet exist: `Config` has no approval-gate channel or timeout fields, and
the `workflow.ApprovalGate.Channel` parsed by the workflow layer is not threaded
through to the controller yet. Wiring a Config field (flag/env) + threading it
through `maybeOpenGate`, and giving the gate a sensible timeout, is a separate
design decision that touches `main.go`, `controller.go`, and the config — a
bigger, riskier change than the self-contained manager correctness fixes above.

**Decision:** land the in-package correctness fixes here — a stale-timer clobber,
a defensive by-value copy, a callback-under-lock deadlock, and a lifetime leak —
and track sub-bug #5 as a follow-up. Rationale: a focused, low-risk, race-tested
change for the concurrency bugs is preferable to bundling a larger
controller+config change that needs its own design; bundling them would also
widen the surface a reviewer has to reason about for fixes whose hard parts are
self-contained in `approvalgates/`.

## Consequences

- `Gate.Timeout > 0` now enforces a real auto-timeout end-to-end in the manager.
   Any gate with a zero timeout must be closed explicitly (`Resolve`/`CloseAll`)
  — the caller-wiring follow-up (#5) is what gives PR-run gates a channel + a
  timeout at all.
- `onResolved` is always invoked outside `m.mu`; callbacks may re-enter the
   manager safely.
- A failed post leaves no half-open gate.
- The package is race-tested: the new regressions (`TestTimeoutFiresGate`,
   `TestFailedPostDropsGate`, `TestResolveBeatsTimeout`,
   `TestCallbackRunsOutsideLock`) plus the existing suite pass under
   `go test -race`.

## Related / follow-up

- **CIC-108 follow-up (sub-bug #5):** add an approval-gate channel + timeout
  source in `Config`/flags, and thread it + a non-zero timeout into
  `controller.maybeOpenGate`. (Track as a new Linear ticket or a named
  follow-up PR; flagged to the owner + dwight in the PR body.)
- The `CloseAll(runID string)` name is retained (it accepts the single runID it
  closes) to avoid changing a signature; its doc comment now states its true
  behavior.
