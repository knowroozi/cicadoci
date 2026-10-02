# CIC‑85: Zero‑match run finalisation

## Problem

When a webhook event arrives but no workflow in the store matches the
event (wrong branch, wrong path, no workflows registered, etc.), 
`LoadAndCreateJobs` creates no jobs.  The run row in Postgres therefore
stays in `status='pending'` with no jobs that a runner can pick up.
The sweep for stale runs only sweeps *jobs*, not runs — so the run
sits indefinitely.

## Fix

**`store.FinalizeRunToSucceeded(ctx, runID)`** — a single atomic UPDATE
that flips a `pending` run to `succeeded` with `finished_at=now()`.  The
WHERE clause includes `AND status='pending'` so it is idempotent and
never forces a transition from a more advanced state.

**`LoadAndCreateJobs`** calls `FinalizeRunToSucceeded` when `created==0`.
The finalisation is non‑fatal: if the DB update fails, the run continues
to exist but the error is logged.  A subsequent `retry` or manual action
can still fix it.

## Design decision: “succeeded” vs a new “skipped” status

Adding a new `skipped` status to the CHECK constraint requires a schema
migration and touches the UI, API, and all run‑status consumers.  For
MVP, `succeeded` is the cleanest terminal state that doesn’t lie: the
run did successfully complete its job, which was “find matching workflows”.
A future ticket can add a distinct `skipped` status if the UI needs to
distinguish “no workflows matched” from “all workflows ran”.

## Out of scope

* A `skipped` run status
* Finalisation on *partial* match (some workflows create jobs, others
  don’t — those non‑matched workflows don’t affect the run status because
  the matched ones produce jobs)
* A “run was not useful” audit event

## Knowledge‑graph

* New function: `store.FinalizeRunToSucceeded`
* Modified: `controller.LoadAndCreateJobs` — adds the finalize call
