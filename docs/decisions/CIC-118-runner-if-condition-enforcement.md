# CIC-118 — Runner `if:` conditional enforcement (resolves CIC-107)

Resolves the runner-side follow-up that **CIC-107** (validator rejection of
`if: failure|always`) deferred: the runner now evaluates each step's `if:`
conditional at execution time, and the validator stops rejecting
`if: failure|always` on steps.

## Problem

`workflow.IfConditionValue` (`success|failure|always`) was parsed and
validated but never enforced at execution. CIC-107 closed this by *rejecting*
`if: failure|always` at validation so the limitation failed loudly rather than
silently. CIC-118 removes that wall and implements the behaviour.

## Decision

Per-step `if:` is enforced by the **runner's step-execution loop**, using the
outcomes of the steps that ran before it in the *same job* (a job is already the
run-unit; steps run sequentially).

1. **One source of truth.** `internal/stepif.Evaluate(if, priorSucceeded,
   priorFailed)` is the single evaluation function, imported by the server's
   `workflow.IfCondition.Evaluate` (now a thin delegate) and by the runner.
   The runner cannot `import "server/workflow"` without a server←runner cycle,
   so the canonical logic lives in a dependency-free `internal` package.

   - `"success"` (or empty) → run iff the prior step **succeeded**.
   - `"failure"` → run iff the prior step **failed**.
   - `"always"` → run regardless.

2. **Downstream state.** A step that is **skipped or fails** sets
   `priorSucceeded=false` and `priorFailed=true`; a step that runs and
   succeeds sets `priorSucceeded=true` and `priorFailed=false`. This makes
   "failure" run when *any* prior step failed **or was skipped**, matching the
   documented "if failure" semantics.

3. **Single terminal per job.** A user-script or timeout failure no longer
   returns a terminal `StatusUpdate` mid-run (the server's `Complete` treats the
   first terminal as final and it is `ON CONFLICT (job_id,seq) DO NOTHING`
   idempotent). Instead the failure is recorded and the loop `continue`s so that
   later `if: failure`/`if: always` steps still run; exactly **one** terminal
   (`SUCCEEDED` or `FAILED` with the last failure's exit code / error class) is
   emitted after the loop. Infrastructure and cancel remain fatal terminals.

4. **Wire path.** The `if:` value flows
   `step.if (workflow) → store.StepSpec.If (col if_condition, migration 0007) →
   queue.Step.If → runnersvc.AssignmentFor → runnerv1.StepSpec.if_condition` and
   back into the job via `AssignmentForSpec`. A new `step_skipped` event
   (registered in `server/events.ValidKinds`) records skips; no
   `RUNNING_STEP`/`step_started` update is emitted for a skipped step.

## Follow-on: lift CIC-107

The validator no longer rejects `if: failure|always` on steps (validation now
accepts any step-level `if`); see the diff in `server/workflow/*`.

## Item 4: decouple the job-level behaviour (review comment on PR #161)

The PR that was merged pre-review-loop and then reverted (#161 → revert #162)
implemented ticket items 1–3 and 5 but **not item 4**: `DAG.PropagateFailure`
(`server/workflow/dag.go`) still reused `job.Steps[0].If == "always"` on a job's
first step as a *dual-purpose* proxy — the same field CIC-118 wires for intra-job
step sequencing ALSO meant "run this job even if an upstream job failed" at the
cross-job level. Lifting the CIC-107 validator rejection (item 5) without item 4
would make a workflow author's `if: always` on a job's first step silently carry
that latent cross-job meaning the moment `PropagateFailure` gets a caller (CIC-
103), with no documentation of where the coupling lives. This was the reason
`docs/decisions/CIC-107-step-if-conditions.md` falsely read "fully resolved."

**Decision (reviewer's option (b)):** since `PropagateFailure` has no production
caller yet, *strip the first-step `if: always` proxy out of `PropagateFailure`
and its test*. `Step.If` now governs only intra-job step sequencing; a dependent
job is skipped whenever any prerequisite failed, transitively, with no job-level
short-circuit read of `Step.If`.

Rationale for option (b) over (a) "add a real job-level `if:` field": there is no
caller and no consumer of a cross-job "run-anyway" field, so inventing a schema
field now would be speculative surface area. The decouple is the smaller, safer
change and it *removes a footgun* rather than deferring it. A dedicated job-level
"run even if an upstream job failed" mechanism can be introduced when
`PropagateFailure` is wired in (CIC-103 / #99), without reusing `Step.If`.

Effect on behaviour today: `PropagateFailure` has no production caller, so
nothing changes at runtime; the change is purely to close the dual-purpose
coupling and make the field single-meaning. Test
`TestDAG_PropagateFailure_IgnoreAlways` is renamed
`TestDAG_PropagateFailure_FirstStepIfAlwaysNoLongerBypasses` and now asserts that
a `if: always` first step is still skipped (the proxy is gone).

## Status

- Proto field + data-path wiring: **done in this PR**
- Runner `if:` enforcement + single-terminal restructure: **done in this PR**
- New test `TestStepIfConditionsEnforcedAtRunner`: **done**
- Lift CIC-107 validator rejection: **done in this PR**
- **Item 4 — decouple `PropagateFailure` (strip the `if: always` first-step
  proxy): done in this PR** (`server/workflow/dag.go` + `dag_test.go`), addressing
  the review comment on the reverted PR #161.
- This redo re-applies the reverted PR #161 net diff on top of current master
  (`c558f8e`), plus the item-4 decoupling as a second commit.
