# CIC-208 — close approval-gate record on run cancel

## Context

CIC-206 (PR #145) added `CancelRun`: a top-level operator cancel of a
single run that fans out to every active and pending job. When a run is
cancelled while a gate is still pending, the gate record (timer + map
entry) was left dangling: the timer kept ticking and the map entry
lingered until the process exited.

## Decision

Close the gate when the run is cancelled — and make the terminal
side-effect pattern race-safe.

### F1 guard: `FinalizeRunToFailed` returns `(bool, error)`

`FinalizeRunToFailed` must return `(moved bool, err error)` where `moved`
is true only when the `UPDATE runs SET status='failed' WHERE status='pending'`
actually changed a row. In `dispatchAfterGate`, `ReleaseRun` and `reportFailed`
are guarded on `moved`:

```go
moved, ferr := c.store.FinalizeRunToFailed(ctx, runID)
// ... log ferr ...
if moved {
    c.ReleaseRun(runID)
    c.reportFailed(ctx, runID)
}
```

A cancelled run has `status='cancelled'` (set by `CancelRun`/`CancelRunJobs`).
`FinalizeRunToFailed`'s `WHERE status = 'pending'` guard skips it, so
`moved=false`. The forge check for a cancelled run is therefore
**never** flipped to `failed` — the correct terminal state is `cancelled`.

### F2: `CancelGate` uses `closeOne(runID, ReasonRunCancelled)`

Before CIC-208 F2, `CancelGate` delegated to `CloseAll`, which always
emitted `ReasonApprovalTimedOut` in the audit transition. After F2:

```go
func (m *Manager) CancelGate(runID string) {
    m.closeOne(runID, ReasonRunCancelled)
}
```

`ReasonRunCancelled = "cancelled: run cancelled"` so the audit row and
the `onResolved` callback read as a cancel, not a timeout.

### `closeOne` terminal side-effect pattern

`closeOne` already handles:
- Moves the record to `GateTimedOut` under the lock.
- Stops the auto-timeout.
- Closes the signal channel.
- Calls `provider.CloseApproval` (outside the lock).
- Fires `onResolved(runID, GateTimedOut, reason)` (outside the lock).

### Wire point

`main.go`'s `CancelRun` closure calls `mgr.CancelGate(rid.String())`
via the `fireTerminal` hook after the store transaction commits.

### T1 regression test

`TestCancelGate_ThenResolveRejected` (Postgres-gated; `-count=20`):
1. Create a pending run in the store.
2. Open a gate.
3. Call `CancelRunJobs` (store-level cancel).
4. Call `mgr.CancelGate(runID)`.
5. Call `dispatchAfterGate(GateTimedOut)`.
6. Assert the run's status is `cancelled`, not `failed`.
7. Assert the last `Complete` call for the run has NO
   `ConclusionFailure`.

## Verified
- `go build/vet ./...` clean
- `golangci-lint run ./...` 0 issues
- `TestFinalizeRunToFailed` passes (now checks `moved=true`/`moved=false`)

## Unverified
- CI is disabled.
- `TestCancelGate_ThenResolveRejected` requires a Postgres `DATABASE_URL`.
