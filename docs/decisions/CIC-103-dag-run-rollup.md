## [CIC-103] DAG promotion and run rollup (2026-10-02)

Before this change nothing moved a `needs` job from `pending` to `queued`, nothing
handled the dependents of a failed job, and nothing ever changed `runs.status`.
A workflow with `needs` ran its first wave and hung; every run looked pending
forever.

### Decision

All of it lives in `server/queue`, inside the transaction that writes the job's
terminal state. There is one helper, `advanceRun`, and every terminal job
transition calls it:

| Transition | Where | Terminal state |
|---|---|---|
| Runner reports a result | `Complete` | succeeded / failed / cancelled |
| Lease expired, no attempts left (sweeper), or runner rejected the job on its last attempt | `Requeue` | dead_letter |
| A queued job is cancelled before any runner took it (cancel scan) | `TerminalizeCancel` | cancelled |

`advanceRun` does three things:

1. **Locks the run row** (`SELECT ... FOR UPDATE OF runs`). Two jobs of one run
   finishing at the same moment are serialised here. Without the lock, two
   READ COMMITTED transactions can each see the other's job as still running:
   neither queues a join job that needs both, and neither finalises the run.
2. **Advances the graph** through the `job_dependencies` edges.
   - The job **succeeded**: queue each *direct* dependent that is still
     `pending` and whose every need has `succeeded`. The finished job is never
     touched, and grandchildren wait for their own needs.
   - The job **failed, was cancelled or dead-lettered**: cancel every `pending`
     job downstream of it, children and grandchildren alike (recursive CTE).
     Such a job can never have all its needs met, so leaving it pending would
     hang the run.
3. **Finalises the run** when no job of the run is left non-terminal:
   `UPDATE runs ... WHERE status IN ('pending','running') RETURNING status`.
   Any failed or dead-lettered job makes the run `failed`. Otherwise any
   cancelled job makes it `cancelled`. Otherwise it is `succeeded`.

The status guard on that UPDATE makes it single-winner. Only the transaction
that gets a row back reports the run as finished. **Only that transaction fires
the hook, and only after it commits.**

`Claim` also moves a claimed job's run from `pending` to `running` in the same
transaction as the claim. It locks the run rows in id order, so two concurrent
claims cannot deadlock.

### The run-terminal hook

`runnersvc.Config.OnRunTerminal func(runID uuid.UUID, state string)` is the one
run-level hook. `runnersvc.New` installs it on the queue with
`queue.(*Postgres).SetRunTerminalHook`. Every terminal transition goes through
the queue, including the dispatcher's sweeper and cancel scan, which share the
same `*queue.Postgres`. So the hook fires for all of them, exactly once per run.

`main.go` sets the field **before** `runnersvc.New` copies the config. Forge
check completion and the backpressure release attach to this hook rather than
adding their own.

### Alternatives considered

- **Promote and roll up from `runnersvc` after `Complete` returns.** This was
  the earlier shape of this PR. It misses the sweeper and cancel-scan paths,
  which never pass through the runner session. It also leaves a gap: a crash
  between the job's commit and the follow-up statements stalls the graph for
  good. Doing it inside the same transaction closes both.
- **A database trigger on `jobs`.** This would cover every writer, including
  hand-written SQL. It was rejected because the hook has to fire after commit in
  Go, and a trigger would hide the scheduling logic from the code that owns the
  queue.
- **`skipped` instead of `cancelled` for dependents of a failed job.** The
  `jobs.status` CHECK constraint has no `skipped`. Adding it needs a migration
  for no behavioural gain today.

### Not covered here

- **Job-level `if: always` / `if: failure`** (`workflow/dag.go` `PropagateFailure`)
  is not persisted on the `jobs` row, so the queue cannot honour it. Every
  downstream job of a failed job is cancelled. Honouring it needs a column plus
  a change to `cancelDescendantsSQL`; this needs its own ticket.
- **Runs with zero jobs** never enter the job state machine. They are CIC-85.
- **Late jobs.** `controller.LoadAndCreateJobs` creates each matching workflow's
  jobs in a separate `CreateJobSpecs` transaction. If the first workflow's jobs
  all finish before the second workflow's jobs are inserted, the run is
  finalised early. The status guard keeps the later jobs from re-finalising it
  or firing the hook twice (`TestRunFinalisedOnlyOnce`). The fix is to create
  all of a run's jobs in one transaction.
- **`store.CompleteJob`** had no callers. It wrote a terminal status with no
  lease check and no graph advance, so it was deleted, as the ticket asked.
