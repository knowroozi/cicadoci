# CIC-209 — close approval gate on zero-job run

## Context

#138 opens the approval gate for every PR run before job creation
(CIC-182 B2: avoid the TOCTOU window between create and hold). When a PR
matches zero workflows, `LoadAndCreateJobs` creates no jobs. Before this
fix, the gate was left dangling:
- `awaitGateAndDispatch` blocks forever when `ApprovalTimeout=0`.
- A late deny/timeout calls `dispatchAfterGate`, which flips the run
  and the forge check from `succeeded` to `failed`.

## Decision

Two-part fix (option B in the ticket, as requested):

### 1. Finalise the run to its terminal state BEFORE closing the gate

In `finishIfNoJobs`, when `n==0`:
```go
if loadErr != nil {
    store.FinalizeRunToFailed(ctx, runID) // moves to 'failed'
} else {
    store.FinalizeRunToSucceeded(ctx, runID) // moves to 'succeeded'
}
```
This moves the run to a terminal state so that when the async waiter
calls `dispatchAfterGate(GateTimedOut)` after `CloseAll`, the `F1` fix
(CIC-208 `FinalizeRunToFailed` returning `moved==false` for a non-
pending row) causes `reportFailed` to be skipped.

### 2. Close the gate without starting the waiter for zero-job runs

`finishIfNoJobs` calls `CloseAll(runID)` when `gateOpened && n==0`.
Because the run is already terminal, `dispatchAfterGate` (triggered by
the closed signal) is a no-op for `FinalizeRunToFailed`.

## F1 dependency

CIC-209 depends on CIC-208 F1: `FinalizeRunToFailed` returning
`(moved bool, err error)` where `moved=false` when the row's `status`
is no longer `'pending'`. This is the same F1 fix that prevents a
cancel-side `ReportFailed` from overwriting a cancelled run.

## Call sites

- `HandlePush`: `gateOpened=false` (push runs do not open gates).
- `HandlePullRequest` loadErr path: `gateOpened=false` (gate already
  closed in the pre-check).
- `HandlePullRequest` main path: `gateOpened` (the real case).
- `rerun`: `gateRerun` bool.

## T1 test

`TestCic209_ZeroJobGatedRun_NotFailed` (Postgres-gated):
1. Create a run in `pending` status.
2. Open a gate with `Timeout=50ms`.
3. Call `c.finishIfNoJobs(ctx, runID, nil, true)` — CIC-209 fix applies.
4. Call `c.spawnGateDispatch(runID)` — starts the async waiter.
5. After 300ms: assert `run.Status != 'failed'` and the forge
        `Complete` list has no `ConclusionFailure` for this run.

## Verified
- `go build/vet -tags postgres ./...` clean
- `golangci-lint run ./...` 0 issues
- `TestCic209_ZeroJobGatedRun_NotFailed` requires Postgres
- CI: disabled
