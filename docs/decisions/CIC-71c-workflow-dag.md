# CIC-71c — workflow → job DAG creation

**Ticket:** CIC-71 (sub-blocker 3)
**PR:** `knowroozi/cic-71c-workflow-dag`
**Date:** 2026-09-28
**Branch base:** `knowroozi/cic-71b-real-org-lookup` (stacked)

---

## Decision

Add the `WorkflowLoader` interface to the controller package, implement
`StaticLoader` and a skeleton `StoreLoader`, and wire `LoadAndCreateJobs`
into `HandlePush` and `HandlePullRequest`.  After creating a run row, the
controller loads the repo's workflows, matches each against the triggering
event via `workflow.On.Matches`, and calls `CreateJobSpecs` for
every matching workflow.

## Why this design

### `WorkflowLoader` as an interface seam

The workflow loading source is orthogonal to the controller:

- **Tests** use `StaticLoader` with pre-parsed in-memory workflows.
- **Dev servers** use `StoreLoader` reading YAML from the `workflows`
  table (added by a future `ListWorkflowsForRepo` store method).
- **Production** adds a `GitHubLoader` that fetches the workflow file at
  a specific SHA using the repo's installation token (CIC-38/39/40).

Without an interface, adding the GitHub loader later would require a
new constructor and a rewire in `main.go`.  With an interface, only
one new file is added and the existing controller is unchanged.

### Jobs are created even when the workload is nil

A nil `WorkflowLoader` in `Config` (the `DefaultConfig` default) means
`LoadAndCreateJobs` is a silent no-op.  This preserves the CIC-71a and
CIC-71b behaviour: runs are created, forge status is reported, jobs
are not.  A deployment that has not yet wired a loader will still
accept webhook events without panicking.

### Workflow failures are non-fatal

A single broken workflow (invalid YAML, validation error, DAG cycle)
does not block other workflows in the same repo from running.  The
controller logs a warning and continues to the next workflow.  A run
with zero created jobs is a valid end state (the operator sees a run
with status "pending" in the UI but no jobs).

### Topological ordering from `DAG.Jobs`

`BuildDAG` returns `DAG.Jobs` in topological order with a
deterministic level-based sort.  `convertJobSpecs` iterates this
order directly, so job specs are already in correct DAG order when
passed to `CreateJobSpecs`.  `CreateJobSpecs` then resolves `Needs`
names to UUIDs in a single transaction, guaranteeing that every
dependency edge can be written.

### `envToJSON` uses `encoding/json`

The env JSON field is a small `map[string]string`.  `encoding/json`
is correct, handles all escaping, and the allocation cost is negligible
for a one-time operation per run.  A hand-rolled encoder was
considered and rejected on the grounds that the risk of a subtle
escaping bug far exceeds the cost of the import.

---

## Scope

### Done
- `WorkflowLoader` interface in `jobs.go`
- `StaticLoader` + `NewStaticLoader` + `Register`
- `StoreLoader` skeleton (returns nil; no-op until
  `store.ListWorkflowsForRepo` exists — CIC-71d)
- `convertJobSpecs` — workflow.Job → store.JobSpec
- `envToJSON`, `secretRefsFor`
- `LoadAndCreateJobs` — loads, matches, creates
- `ParseWorkflowBytes` — exported YAML parser helper
- `WorkflowLoader` field in `Config` (nil by default)
- `LoadAndCreateJobs` wired into `HandlePush` and `HandlePullRequest`
- 13 tests covering splitRepo, convertJobSpecs (linear/diamond/env),
  StaticLoader + trigger filtering, nil-loader no-op

### Deferred
- `StoreLoader.LoadAll` implementation — depends on
  `store.ListWorkflowsForRepo` (CIC-71d)
- Per-job `CloneURL`/`GitRef`/`GitSha` in `JobSpec` — deferred to
  CIC-38/39/40 (need clone credentials)
- `SecretRefs` in `JobSpec` — workflow schema has no job-level
  secret directive yet; deferred to a future ticket
- `HandleCheckSuite`/`HandleCheckRun` — require job resolution that
  CIC-71d adds; they remain no-ops with a clear TODO

### Not part of this ticket
- `StoreLoader` full implementation (CIC-71d)
- GitHub installation-token-based workflow loading (CIC-38/39/40)
- Per-repo workflow file management UI (M6)
- Job-level timeout/secret directives in the workflow schema
- check_suite / check_run event routing
