# CIC‑84: Production workflow producer

## Problem

CIC‑78 (PR #89) wired `StoreLoader` to read workflows from the `workflows`
table. CIC‑84 adds the other half: a producer that fetches workflow YAMLs
from a code host at a given ref and upserts them into the table, so the
loader has something to read.

Without CIC‑84, the only path for workflows to reach the table is the
dev‑seed (hardcoded demo workflow). Production has no way to register
workflows from the repository's actual workflow files.

> **Path update (CIC-121, 2026-10-02):** the fetcher originally read
> `.github/workflows/`. That folder belongs to GitHub Actions, which runs every
> YAML file in it, so the path is now `.cicado/workflows/*.yml|*.yaml`
> (`github.WorkflowDir`). See `CIC-121-cicado-workflow-path.md`. Paths below
> are updated to match.

## Design

Two components:

### 1. `server/github/workflows.go` — GitHub contents fetcher

A small leaf package that fetches `.cicado/workflows/*.y{a}ml` from GitHub's
REST API:

* `GET /repos/{owner}/{repo}/contents/.cicado/workflows?ref={sha}` — list the
  directory
* For each file: read the `content` base64 field from the listing entry
* Parse YAML files; skip non‑YAML and sub‑directories

404 at the workflow directory is not an error: the repo simply has no
workflows yet. 5xx → error.

### 2. `server/controller/workflowproducer.go`

* `WorkflowProducer` interface: `Produce(ctx, owner, repo, ref, repoID)`
* `GitHubWorkflowProducer` — implements it using `github.WorkflowFetcher`
  + `store.UpsertWorkflow`
* Each matched file is parsed with `workflow/parse`, JSON‑marshalled, and
  upserted as a `workflows` row.  Parse errors are logged as warnings;
  other valid files are still upserted.
* The producer has a `MaxFetchTime` (default 15 s) to prevent one large or
  unreachable repo from blocking the webhook handler.

### 3. `server/store/workflows_upsert.go`

Adds `UpsertWorkflow(ctx, repoID, path, name, rawYAML, definition)` to the
store.  `source` holds the raw YAML; `definition` holds the JSON‑marshalled
parse output.  Upsert semantics: `ON CONFLICT (repo_id, path) DO UPDATE`.

### 4. Controller integration

`Config.Producer` is a new field; when non‑nil it is called in
`HandlePush` and `HandlePullRequest` **before** `LoadAndCreateJobs`, so
the workflow table holds the freshest definitions at the triggering SHA
before the loader queries it.  A `nil` producer is a no-op: it skips
the GitHub fetch and relies on whatever is already in the store (the
dev‑seed path).

## Why the producer runs *before* the loader

The `LoadAndCreateJobs` path queries the table via `StoreLoader.LoadAll`.
If the producer runs after the loader, a fresh workflow added to a repo
is ignored until the next push. Running the producer first means each
push immediately makes its workflows available.

## Scope

* Fetches `.cicado/workflows/` at the triggering SHA (not at branch HEAD,
  so the run's workflows match what was pushed).
* Uses the GitHub Installation access token via `Bearer` header.
* When no `--github-app-token` is configured, the producer is not created
  and `c.cfg.Producer` stays nil — no fetch attempt, the dev‑seed path
  works as before.
* Fork‑PR security: the same `policy.ForkPolicy` that already restricts
  secrets/runner tags on fork PRs applies to fork PR runs.

## Out of scope

* Webhook event triggers filtering (`on.push.branches`, `on.push.paths`):
  the producer fetches all workflow files; filter evaluation (CIC‑22,
  M2) is a separate concern.
* Workflow file change detection / "run only on changed files": a follow‑up
  ticket.
* Multiple content hosts (GitLab, Bitbucket) behind the `WorkflowProducer`
  interface: the interface allows it; only the GitHub impl is in this PR.

## Tests

* `server/github/workflows_test.go` — httptest‑backed:
  * `TestFetchWorkflowFiles_FindsYAML` — finds `.yaml` files, skips `.md`
    and sub‑dirs, checks auth header and ref query param
  * `TestFetchWorkflowFiles_NoWorkflows` — 404 → nil, no error
  * `TestFetchWorkflowFiles_BadRef500` — 500 → error
* `server/controller/recordingproducer_test.go` — `recordingProducer`
  stub for future integration tests in the controller
* `server/store/workflows_upsert.go` — PG‑gated test to be added

## Knowledge‑graph

CIC‑84 adds:
* New package file `server/github/workflows.go`
* New file `server/store/workflows_upsert.go`
* New file `server/controller/workflowproducer.go`
* New field `Config.Producer` in `controller.go`
* `HandlePush` / `HandlePullRequest` call `Producer.Produce`

Affected components: `controller`, `github`, `store`.
Risk: MEDIUM (new fetch path; no change to existing security invariants).
## Status (as of `d689e40`, 2026-10-02; QC docs-sync)
- **Wired into `cicado-server`** — CIC-120 (PR #106) sets `ctrlOpts.Producer` to `NewGitHubWorkflowProducer(...)` when `CICADO_GITHUB_WORKFLOW_TOKEN` is set; otherwise the producer is `nil` and the dev-seed path is used.
- **Workflow path**: `.cicado/workflows/*.y*ml` (CIC-121, PR #107) — deliberately *not* the GitHub Actions directory, which would itself run every YAML placed there. No fallback.
- **Per-call installation tokens** deferred to CIC-146: until then the producer (and the GitHub Checks forge) ride the single operator-supplied `CICADO_GITHUB_*_TOKEN` PAT, not a per-job installation access token.
