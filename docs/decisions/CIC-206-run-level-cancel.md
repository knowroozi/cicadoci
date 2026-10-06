# CIC-206 — run-level cancel

## Decision

A top-level operator cancel of a single run, with per-job cancellation
fanned out to every active and pending job.

**Status (r3, after Dwight's 3 blockers + small):**
- **B2: audit-first on partial failure.** `handleCancelRun` writes the
  audit row when `touched=true` *before* returning a 500 on a partial
  failure. The operator's cancel action is a fact and must be recorded
  even when the system partially fails.
- **F1: finalize-once, fire-once.** `CancelRunJobs` only finalizes the
  run when **no active (queued/claimed/running) jobs remain**. For a
  gated/pending-only run it finalizes the run to `'cancelled'` and
  fires `fireTerminal` after the tx commit. For a run with active jobs,
  `CancelRunJobs` cancels the pending ones and rings the doorbell on the
  active ones, but does **not** finalize: the cancel scanner + `advanceRun`
  finalize the run and fire the terminal hook exactly once, when the last
  active job's terminal state commits.
  - The gated/pending case fires `fireTerminal` directly (the cancel
    scanner won't handle it — pending jobs aren't `queued`).
  - The active case does not fire `fireTerminal` from `CancelRunJobs`;
    `advanceRun` in the queue does.
- **C1: operator cancel = 'cancelled', not 'failed'.** Pending jobs are
  set to `status='cancelled'` with `finished_at=now()`, and the run is
  finalized to `'cancelled'` (when no active jobs remain).
- **T2: checked fixtures; mixed-state test.** `cancelrun_test.go` now
  checks every `pool.Exec` error and includes `TestCancelRunJobs_MixedState`
  (running/queued/succeeded/pending-gated/pending-dependent) plus
  `TestCancelRunJobs_FireCalledOnGatedNoOp` (the F1 regression).
- **Doc cite to CIC-208.** The open gate record close is filed as CIC-208.

The two surface points: API `POST /api/v1/runs/{id}/cancel` and
CLI `cicado run cancel [run <runID> | <runID>]`.

## B2 audit-first

In `writes.go`, `handleCancelRun`:
```
touched, err := h.writes.CancelRun(ctx, runID, reason)
if touched {
    if auErr := h.audit.Record(...); auErr != nil {
        writeJSONError(500, ...)
        return
    }
}
if err != nil {
    // B2: audit row already written above (touched=true).
    writeJSONError(500, ...)
    return
}
if !touched {
    writeJSONError(404, ...)
    return
}
writeJSON(200, ...)
```

## F1 finalize-once, fire-once

`CancelRunJobs` now takes a `fireTerminal func(ctx, runID)` parameter:
- **Gated case (no active jobs):** cancel pending jobs + finalize run in
  one tx; commit; fire `fireTerminal(ctx, runID)` once.
- **Active case:** cancel pending jobs in a tx (no finalize); ring
  `cancelJob` for each active job; `cancel scanner + advanceRun` finalize
  the run and fire the terminal hook. No double-finalize, no leak.

The main.go closure provides `fireTerminal`:
```
ctrl.ReleaseRun(rid)   // release the backpressure slot
ctrl.RunCompleted(ctx, rid, "cancelled")  // complete the GitHub check
```

## C1 cancel status

`CancelRunJobs` (gated case):
- Top-level pending → `'cancelled'` with `cancel_reason=reason, finished_at=now()`.
- Dependent pending → `'cancelled'` with `cancel_reason=reason, finished_at=now()`.
- Run → `'cancelled'` with `finished_at=now()` (only if no active jobs remain).

The `'skipped: '` prefix is removed. Operator-cancelled dependents use
the operator's reason directly — `'skipped: '` was FailAllJobs' semantic
for a parent failed, which is distinct from an operator cancel.

## CIC-208 follow-up

The open approval gate record (timer + map entry) is **not** closed by
`CancelRunJobs`. File CIC-208 to close it when a gated run is
cancelled, so the gate's timer does not leak.

Note: when a gate times out after a cancel, the gate's timeout callback
(advanceRun via the controller) may flip the check to `failed` even
though the run was just cancelled. CIC-208 must handle this case.
## R1 fix

`CancelRunJobs` now takes a `SELECT ... FROM runs WHERE id=$1
FOR UPDATE` lock inside the tx, then reads the active job IDs inside
the tx. This prevents a race where a job finishes in the gap between
the old out-of-tx `ActiveJobIDs` read and the tx commit, promoting a
dependent that escaped the cancel or missing the run-finalize.

## CLI naming

`cicado run cancel-run <runID>` (hyphen, consistent with other
`cicado run` subcommands).

## Open work

- CIC-208: close gate record on run cancel.
- End-to-end test (Dwight's T2 ask): a hook installed, `RequestCancel`,
  the dispatcher's cancel terminalization; assert the hook fires once
  with `"cancelled"`; plus a gated case.
