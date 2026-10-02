## [CIC-106] Backpressure slot held until the run is terminal (2026-10-02)

The controller took a per-repo backpressure slot in each webhook handler and
released it with `defer` when the handler returned, which is milliseconds after
the run was created. The cap of 4 per repo therefore limited concurrent webhook
handling, not concurrent runs.

### Decision

A run holds its slot from the webhook until it is terminal. The slot is
returned exactly once, on whichever path ends it:

| Path | Who releases |
|---|---|
| `CreateRun` fails | the handler, `Release(repo, org)`: no run exists to hold it |
| The run got no jobs (no matching workflow, load error) | the handler, `finishIfNoJobs` (shared with CIC-81, which also completes the run's check there): such a run never reaches a terminal job, so the queue never fires the run-terminal hook |
| The run finishes (runner report, sweeper dead letter, cancel scan) | `runnersvc.Config.OnRunTerminal` → `Controller.ReleaseRun` (the single hook from CIC-103) |

- `backpressure.Tracker` gains `Register(runID, repo, org)` and
  `ReleaseByRunID(runID)`. Both hold the tracker's mutex for the whole call;
  `ReleaseByRunID` deletes the registration before releasing, so a second call
  is a no-op and no path can double-release.
- The run is registered right after `CreateRun` and **before** its jobs are
  enqueued. A run that finished before it was registered would keep its slot.
- If the job count cannot be read, the slot is released: briefly exceeding the
  cap is recoverable, a leaked slot is not.
- The dispatcher gets **no** tracker. It would `Acquire` per job keyed by run
  ID and never release on completion, stalling job 5+ of every run.

### Consequences a reviewer might weigh

- A push or PR that arrives while its repo already has 4 runs in flight is now
  rejected with the existing "backpressure" error, as the cap intends. GitHub
  does not redeliver automatically, so that event creates no run. Queueing
  instead of rejecting would be a separate ticket.
- Slots are in-memory. A server restart forgets them, so runs in flight at the
  restart no longer count against the cap. That errs on the permissive side.
