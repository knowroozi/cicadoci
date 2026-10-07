# CIC-107 — Step-level `if: failure|always` handling

## Problem

`workflow.IfConditionValue` is defined (`success|failure|always`) and parsed
correctly from YAML. The runner (cicanscript) is not yet wired to honour
step-level conditions. The validator must reject unsupported conditions
until the runner implementation lands.

1. The cicanscript executor stops at the first failing step.
    Steps with `if: failure` or `if: always` on any step index would
    silently never run, giving false-negative cleanup on job failure.
2. Step-level `if` conditions are not yet part of the runner protocol
   (no proto field for per-step conditions).

## Decision

The **validator rejects `if: failure` and `if: always` on every step**
(step index 0 or greater) with a clear, actionable error:

    job "build" step 0: step-level 'if: failure' is not yet supported
    by the runner (CIC-107); remove it or change to 'if: success'

This makes the limitation fail loudly at validation time rather than
silently misbehaving at execution time.

### Follow-up: runner-side implementation (CIC-118)

The runner-side implementation is tracked as **CIC-118** (a separate
ticket and PR, not blocked by this one):

1. Add a `string step_if` field to the `runner.Step` proto (additive,
    non-breaking under ADR-004; absent fields default to "success").
2. The cicanscript executor evaluates each step's condition using the
    prior step's exit code.
3. The runner reports each step's exit code via the existing StepRun RPC;
    the executor decides which steps to skip.

### DAG.PropagateFailure note (resolved by CIC-118 item 4 — proxy removed)

`DAG.PropagateFailure` (server/workflow/dag.go) used to use `job.Steps[0].If == "always"`
to bypass upstream-failure propagation — a **cross-job proxy** dual-purposing the
intra-job step `if:` field. It was a design placeholder with no production caller.

That proxy is now **removed** (CIC-118 item 4, `server/workflow/dag.go`): a
dependent job is skipped whenever a prerequisite failed, transitively, with no
read of `Step.If`. The field therefore has a single, well-defined meaning
(intra-job step sequencing). A dedicated job-level "run even if an upstream job
failed" mechanism can be introduced when `PropagateFailure` is wired in (CIC-103)
without reusing `Step.If`.

## Status

- Validator rejection: **done in this PR (#103)**
- Runner-side implementation: **CIC-118, separate PR**

## Resolved

CIC-118 (see `CIC-118-runner-if-condition-enforcement.md`) implemented the
runner-side `if:` enforcement, removed this validator rejection, AND decoupled
the job-level `PropagateFailure` proxy (item 4 — see the "Item 4" section there)
so that lifting the rejection did not re-introduce the latent dual-meaning. The
CIC-107 concern is now fully closed.
