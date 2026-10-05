# CIC-182: Gate dispatch blocking (part 1)

**Decision:** When an approval gate is open for a PR run, its top-level
jobs are held in 'pending' status; only after the gate is resolved do
they get promoted to 'queued' for dispatch.

## Rationale

Without this blocking, a PR run with a mandatory approval gate could
dispatch its jobs immediately even though the deployer's intent was to
hold them until someone explicitly approved.

## Implementation

- `store.CreateJobSpecs` — top-level jobs start 'pending' when created with
  `hold=true`; they stay 'pending' until `PromoteTopLevelJobs` is called
  (B2/S3 fix: no TOCTOU window; the gate check precedes job creation in
  `HandlePullRequest`)
- `store.PromoteTopLevelJobs` — flips pending top-level jobs to 'queued'
  (called after gate is approved)
- `store.FailAllJobs` — fails all 'pending' jobs in the run when the gate
  is denied or times out (B3: both top-level and dependent jobs)
- `approvalgates.Manager.Await(ctx, runID)` — blocks on a per-gate
  channel signalled exactly once on any terminal transition
- `controller.awaitGateAndDispatch` — goroutine that blocks on `Await`,
  then promotes (approval) or fails (deny/timeout) the run
- **B1 fix:** when `ApprovalTimeout=0`, `Await` uses `context.Background()`
  so the gate is not preemptively timed out — the run blocks until manually
  resolved
- **S4 fix:** `GatePending` (unknown runID) in `dispatchAfterGate` is
  treated as fail-closed (fails the run and all held jobs)

## Safety

- Timeout: when `ApprovalTimeout > 0`, the `Await` call uses
  `context.WithTimeout(context.Background(), ApprovalTimeout)`, so a gate
  that never resolves times out and the run fails
- When `ApprovalTimeout=0`, the gate blocks indefinitely until a human
  resolves it (or the server shuts down, triggering `CloseAll`)
- `CloseAll`: also closes the signal channel, ensuring a shutdown never
  leaves a goroutine blocked
- Idempotency: `signal` is closed via the existing terminal-state guard in
  `Resolve`/`onTimeout`/`CloseAll`; concurrent calls race safely on a
  sync.Once-equivalent lock-protected check
- Audit trail: gate resolutions emit an `Audit` entry via the controller's
  `onResolved` callback (shipped in CIC-162, merged in #135)

## Held jobs after server restart

CIC-182 does not persist gate state to Postgres. If the server restarts
while a gate is open and jobs are held in 'pending' status, the
`awaitGateAndDispatch` goroutine terminates with the process. The held
jobs remain in 'pending' state in the database. On a fresh server start,
they will not be resumed automatically — a new PR push (or a re-request
via `POST /api/runs`) would create a new run.

This is a known limitation for part 1. Persistence of gate state across
reboots is tracked in a future ticket.

## Out of scope for this PR

- API endpoint to resolve a gate programmatically (`POST /api/runs/{id}/gates/resolve`)
- Persistence of gate state across server restart
- Workflow file declaration of mandatory gates (e.g., `gates: [deploy]`)

## References

- Linear: CIC-182
- Related: CIC-162 (approvalgates wiring — merged PR #135), CIC-108 (approvalgates subbugs)
