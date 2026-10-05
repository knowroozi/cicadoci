# CIC-180 — Producer Token Source Wiring

**Branch:** `knowroozi/cic-180-producer-minter` (based on `knowroozi/cic-146-part2`)
**PR:** pending

## Context

CIC-120 added the `GitHubWorkflowProducer` with a **static Bearer token**
(a fine-grained PAT passed via `--github-workflow-token` /
`CICADO_GITHUB_WORKFLOW_TOKEN`). A static PAT cannot be rotated or
scoped per-repo. CIC-146 P2 (#131) stamps `repos.installation_id` from
the GitHub App installation-event payloads; CIC-151 already wires
`runnersvc.CloneMinterFunc` to mint per-job installation tokens.

CIC-180 closes the gap: the workflow *producer* now uses the same
`InstallationTokenMinter` path so that, when the GitHub App is configured,
each `Produce` call mints a short-lived token scoped to the pushed-to
repo's installation. The static PAT path is retained as an explicit
fallback for environments that do not use the GitHub App.

## Decision

- `controller.MinterFunc` (new type alias):
  `func(ctx context.Context, repoID string) (string, error)`.
  Returns `("", nil)` for repos with no GitHub App installation.
- `controller.GitHubWorkflowProducer` gains:
  - `MinterFunc MinterFunc` — non-nil enables per-call minting.
  - `httpClient *http.Client` — stored so the minter path can
    construct a new `WorkflowFetcher` for each call with the correct
    HTTP client (tests redirect to `httptest` via this field).
- New constructor `NewGitHubWorkflowProducerWithMinter(minterFunc, st, log, httpClient)`.
- `Produce` logic:
  1. If `MinterFunc != nil`: mint a token per call; empty token
     (no installation) short-circuits with a debug log, no HTTP call.
  2. Else if `fetcher != nil`: use the static-PAT fetcher (CIC-120).
  3. Else no-op.
- `main.go`: the minter path (when `--github-app-id` +
  `--github-app-key-file` set) takes precedence; the static PAT
  (`--github-workflow-token`) is a fallback.
- `TODO(CIC-146)` comment removed; replaced with a CIC-180 comment.
- `newForge` comment updated (clone-credential minting is now in
  `runnersvc`, not the forge).
- `newWorkflowProducer` kept as a convenience wrapper around
  `NewGitHubWorkflowProducer` for static-PAT callers / tests.

## Tests

- `TestWorkflowProducerWiring_MinterPathSyncs` — minter returns a
  valid token; GitHub API called with it; workflow upserted.
- `TestWorkflowProducerWiring_MinterEmptyTokenSkips` — minter
  returns `""`; no HTTP call, existing rows untouched.
- `TestNewWorkflowProducer_EmptyTokenIsNil` — untyped nil for empty
  PAT (controller's `!= nil` gate stays off).
- `TestWorkflowProducerWiring_DefaultBranchPushSyncs` —
  `TestWorkflowProducerWiring_ListingNotFoundKeepsRows` — existing
  static-PAT tests still pass.

## Security / invariants

- The minter registers the token value with the
  `redact.RegisteringMasker` before it reaches any log (CIC-74
  invariant, inherited from `InstallationTokenMinter`).
- No changes to the per-job clone token path
  (`runnersvc.CloneMinterFunc` — CIC-151 / CIC-181, separate PR).
