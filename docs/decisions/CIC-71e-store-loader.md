# CIC-71e — `StoreLoader`: read workflow definitions from the DB

**Ticket:** CIC-71 (sub-blocker 5)
**PR:** `knowroozi/cic-71e-store-workflow-loader`
**Date:** 2026-09-28
**Branch base:** `knowroozi/cic-71d-server-wiring` (stacked)

---

## Decision

Add `store.WorkflowRow` and `store.ListWorkflowsForRepo` (in
`server/store/reads.go`), and complete the
`StoreLoader.LoadAll` implementation (in `server/controller/jobs.go`)
to call it and decode the `Definition` jsonb column into
`*workflow.Workflow`.

## Why `WorkflowLister` interface

`StoreLoader` previously held a concrete `*store.Store`.  Adding a
`WorkflowLister` interface (just `ListWorkflowsForRepo`) decouples
the controller package from the concrete store so `StoreLoader`
can be tested with a stub that returns pre-built `[]WorkflowRow`
— no Postgres needed.

`*store.Store` is verified to satisfy `WorkflowLister` via the
`var _ WorkflowLister = (*store.Store)(nil)` guard at the bottom
of `jobs.go`.

## `Definition` jsonb — why JSON, not re-parse YAML

Workflow rows store their parsed form as `definition` (jsonb),
not as raw `source` YAML.  This is the design intent in
`0001_init.sql`: the raw YAML is also stored for display and
positional error pointing, but the `definition` jsonb is the
machine-consumed form.  Decoding it via `json.Unmarshal` is fast,
deterministic, and handles all the types Go's json package
supports.  Re-parsing YAML from `Source` would be correct but
wasteful and would require a full parse on every run trigger.

## Malformed rows

`StoreLoader` logs a `Warn` and skips any row whose `Definition`
is not valid JSON.  This is the correct behaviour: a single
malformed workflow row should not block all other workflows in
the same repo from running.  The operator can fix the bad row
in the DB.

## Scope

### Done
- `store.WorkflowRow` struct in `reads.go`
- `store.ListWorkflowsForRepo(ctx, repoID)` in `reads.go`
- `WorkflowLister` interface in `jobs.go`
- `StoreLoader.store` typed as `WorkflowLister` (not concrete
   `*store.Store`)
- `StoreLoader.LoadAll` implemented: query → decode → skip
	malformed → return valid workflows
- 4 new tests: empty, valid decode, skip-malformed, lister-error

### Deferred
- `main.go` wiring of `StoreLoader` into `DefaultConfig()` —
    left to CIC-71e (operator wiring) or a future sub-blocker
- Revision-aware `LoadAll` (SHA-pinned workflow versioning) —
    future ticket
- `Source` YAML → `Definition` jsonb writing — part of the
    workflow registration flow, not this ticket

### Not part of this ticket
- Audit event in controller (CIC-71f)
- Backpressure tracker in dispatch loop (CIC-71g)
- Chat/approvalgates integration (CIC-71h)
