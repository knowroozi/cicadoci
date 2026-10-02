# CIC-120 — wire the production workflow producer into `cicado-server`

**Ticket:** CIC-120 (child of CIC-84, the "production workflow producer")
**PR:** `knowroozi/cic-120-producer-wiring`
**Date:** 2026-10-02
**Base:** origin/master (`e218580`)

---

## The gap

CIC-84 (#95, merged to `master` at `0034d5d`) added the *producer itself*:
`server/github/workflows.go` (a GitHub contents fetcher) and
`server/controller/workflowproducer.go` (`WorkflowProducer` interface +
`GitHubWorkflowProducer`, with its fork-producer test and the DefaultBranch
gate). CIC-78 wired the *loader* into `main.go`. But no code in
`server/cmd/cicado-server/main.go` ever constructs a
`GitHubWorkflowProducer` and assigns it to `Config.Producer`, so the field
stays `nil`. With a nil producer the controller falls back to whatever is
already in the `workflows` table — i.e. only the `-dev-seed` demo workflow.
A real push to a real repo therefore never registers its own `.github/
workflows/*.yml|yaml` files, and the loader has nothing fresh to build a job
DAG from.

## Decision

Wire the existing #95 producer into `run()` in
`server/cmd/cicado-server/main.go`, gated on an operator-supplied token:

- **New flag `--github-workflow-token` (env `CICADO_GITHUB_WORKFLOW_TOKEN`).**
  The value is a **long-lived fine-grained personal access token** (PAT)
  with *Contents: read* on the repos CICaDo builds. When it is non-empty,
  `newWorkflowProducer` in `main.go` builds
  `controller.NewGitHubWorkflowProducer(token, st, log, &http.Client{Timeout:
  20 * time.Second})` and `run()` assigns it to `ctrlOpts.Producer`. When it
  is empty the helper returns an untyped nil, the field stays nil and the
  server relies on the store/dev-seed path — backwards compatible with every
  deployment that doesn't set the token. Prefer the env var: a flag value is
  visible in `ps`.
- **Why a PAT and not an installation token (MVP split, option (b) of the
  #106 review).** The producer takes one static token at construction. A
  GitHub App installation token expires after 1 hour and covers one
  installation, so a static one would make every fetch fail with 401 an hour
  after start — and fetch failures are logged at Warn and swallowed, so runs
  would silently use stale definitions. A fine-grained PAT does not expire
  hourly, so the static-token shape is correct for it. The cost: one token
  serves only the repos it can read, and the operator rotates it. Per-call
  installation-token minting and decoding `installation.id` from the webhook
  are **CIC-146**, cited in a `TODO(CIC-146)` at the construction site.
- **Removed workflows are deleted.** Once the producer is wired, a workflow
  file deleted from the repo (say an old deploy job) would otherwise keep
  triggering runs from its stale row on every default-branch push. After a
  *successful* fetch, `Produce` calls the new
  `store.DeleteWorkflowsNotIn(repoID, presentPaths)`, which deletes that
  repo's rows whose path is not among the files present at the ref. It is
  scoped to the synced repo; other repos' rows are never touched. Every
  fetched path is kept, including files that fail to parse, so a syntax error
  keeps the last good definition instead of silently dropping the workflow.
  A failed fetch returns before the prune, so a GitHub outage never deletes
  live workflows. A 404 on the workflows directory also skips the prune:
  GitHub returns 404, not 403, for a private repo the token cannot read, so
  pruning on it would delete every row of any repo outside the PAT's
  selection. `FetchWorkflowFiles` returns `ErrWorkflowDirNotFound` for it.
  Cost: a deliberately removed `.github/workflows` directory leaves stale
  rows until CIC-146. `runs.workflow_id` is `ON DELETE
  SET NULL`, so past runs keep their history.
- **HTTP client carries a 20 s timeout.** The producer already bounds each
  fetch with `MaxFetchTime` (15 s) via a context deadline; the client timeout
  is belt-and-suspenders so a hung GitHub connection cannot wedge a webhook
  dispatch beyond that.
- **No new imports:** `controller.NewGitHubWorkflowProducer`, `http`, and
  `time` are already imported in `main.go`, so the diff is additive only.

The security gates from #95 are unchanged and live in the *controller*, not
in the wiring: `HandlePush` calls the producer only on the default branch,
and `HandlePullRequest` never calls it at all (fork and same-repo PR both
skip it), so a fork-supplied workflow definition can never reach the shared
repo-wide `workflows` row. The producer only fetches workflow *definitions*;
per-job secret delivery remains guarded elsewhere (CIC-133 / CIC-102), so
nothing secret is exposed to a fork PR.

### Flag name

`--github-workflow-token` was chosen over the sketch name in
`CIC-84-workflow-producer.md` (`--github-app-token`) because the value is an
workflow-fetch token (a fine-grained PAT, not GitHub App credentials), and the
flag's purpose is specifically fetching workflow files. A one-line rename if the owner prefers
the doc name.

### Fetch path — deferred to CIC-121

The producer fetches `.github/workflows/` (the #95 default, hardcoded in
`server/github/workflows.go`). The aspirational `.cicado/workflows/`
convention is a separate decision (CIC-121) and touches already-merged
producer code plus its httptest fixtures, so it is **not** folded into this
wiring PR. A `TODO(CIC-121)` at the call site in `main.go` records that the
configured path should be threaded through the fetcher when CIC-121 lands.

## Out of scope

- Minting GitHub App installation tokens per call (the existing
  `InstallationTokenMinter` / `InstallationTokenMinterAdapter` from CIC-26 /
  CIC-81) and decoding `installation.id` from push/PR payloads — **CIC-146**.
  The static fine-grained PAT is the MVP.
- Fetch-path reconfiguration (CIC-121) and webhook-trigger filtering
  (CIC-22): the producer fetches all files; filtering is separate.

## Tests

The fetcher and the fork/DefaultBranch gates are covered by #95's
`server/github/workflows_test.go` and `server/controller/fork_producer_test.go`.
This PR adds (all against real Postgres via `testdb`):

- `server/store/workflows_test.go` `TestDeleteWorkflowsNotIn`: the prune
  deletes only rows outside the kept set, only for the given repo; an empty
  kept set clears that repo only; a nil repo ID is an error.
- `server/cmd/cicado-server/producer_wiring_test.go`:
  `TestNewWorkflowProducer_EmptyTokenIsNil` (empty token ⇒ nil producer) and
  `TestWorkflowProducerWiring_DefaultBranchPushSyncs` — the producer built by
  `newWorkflowProducer` from a token, set on the controller config as `run()`
  does, against a fake GitHub contents API: a default-branch push upserts
  `ci.yaml` (requests carry the token), deletes the repo's stale
  `old-deploy.yml` row, and leaves another repo's row with the same path
  alone. Disabling the prune call makes this test fail.
  `TestWorkflowProducerWiring_ListingNotFoundKeepsRows`: a listing 404 on a
  default-branch push leaves both of the repo's rows in place; mapping the
  404 back to "zero files" makes it fail.

## Knowledge-graph

CIC-120 touches `server/cmd/cicado-server/main.go` (flag + producer
construction), `server/controller/workflowproducer.go` (prune after fetch),
`server/store/workflows.go` (`DeleteWorkflowsNotIn`, added to the narrow
`WorkflowStore` interface), and adds `docs/decisions/CIC-120-producer-wiring.md`.
Affected components: `cmd/cicado-server`, `controller`, `store`. Risk: LOW–MEDIUM
(the prune deletes rows; it is repo-scoped and runs only after a successful
fetch; no change to #95's gates or to a security invariant).
