# CIC-146 — GitHub App installation event handler (Part 1)

> Refs CIC-146 (part 1 of 2). Part 2 (producer token source, main.go
> wiring, decode `installation.id` from push/PR payloads) is a separate PR.

## Background

CIC-146 was flagged in project-docs/01-project-plan.md §7 as a missing
"GitHub App install event handler" — the first item in the M3 runtime
gaps list.  Before this change:

- `server/webhook/events.go` acknowledged `installation` and
  `installation_repositories` events but did nothing with them
  (case "installation": return nil — "out of scope for this ticket").
- `repos.installation_id` existed in the schema (migration 0001) but was
  never populated — always NULL or empty string.
- The clone minter (CIC-151) resolves `installation_id` from
  `repos.installation_id` via `repoInstallationID(pool, reqCtx,
  repoOwner)`.  With this column always empty, B5 soft-skip returned
  `("", nil)` — every job ran with an unauthenticated `git clone`.
  On a private repo, the clone fails.

## Design

The handler is split across three layers.

### 1. Webhook protocol layer (`server/webhook/events.go`)

- `EventInstallation` and `EventInstallationRepos` constants.
- `InstallationPayload` and `InstallationRepositoriesPayload` structs: the
  minimal subset of GitHub's payload we consume.
- `Dispatcher` interface extended with `HandleInstallation` and
  `HandleInstallationRepos`.
- `NoOpDispatcher`, `fakeDispatcher`, `blockingDispatcher`, `capturePullRequest`
  (tests) implement both methods (no-op, returns nil).
- `Dispatch` routes `"installation"` and `"installation_repositories"`
  to the new handles instead of returning nil.
- `Repositories []GitRepo` lives at `InstallationPayload` (top-level),
  per GitHub's actual JSON shape.

### 2. Store layer (`server/store/installations.go`)

`SetRepoInstallationIDByName(ctx, orgOwner, repoName, installationID)`
upserts org+repo and sets `installation_id`.  An org that
doesn't exist yet is created (installation events often
arrive before the first push — expected and correct).
`SetRepoInstallationID` (batch: all repos under an org) was removed in a
review round: it was dead code in production (never called) and the
top-level `Repositories` list from GitHub gives us the exact repo
names, so per-repo upsert is both tighter and more explicit.

### 3. Controller layer (`server/controller/controller.go`)

- `HandleInstallation(payload InstallationPayload)`:
  **Allow-list** (S1 fix): only `created` and `new_permissions_accepted`
  actions call `SetRepoInstallationIDByName` for each repo in
  `payload.Repositories`.  All other actions (`deleted`, `suspend`,
  `unsuspend`) are no-ops — clearing `installation_id` is CIC-81 scope.
- `HandleInstallationRepos(payload InstallationRepositoriesPayload)`:
  On `added` action, calls `SetRepoInstallationIDByName` for each repo in
  `payload.RepositoriesAdded`.  `removed` is a no-op (clearing is CIC-81
  scope).  `added_target`/`removed_target` are not valid GitHub actions
  and are not handled.

## Security

- `installation_id` is a numeric GitHub App installation identifier —
  it is NOT a secret.
- Tokens are minted per-job (CIC-151) and are never persisted.
- On `deleted`: the `installation_id` is not cleared — clearing is
  CIC-81 scope.  A deleted installation's App JWT → installation token
  exchange fails on GitHub's side, so the minter returns an error and
  jobs on those repos fail with `secret_unavailable` (fail-closed).

## Tests

- `server/store/installations_test.go`:
  - `TestSetRepoInstallationIDByName`: creates org+repo, verifies the
    stored value.
- `server/controller/controller_test.go`:
  - `TestHandleInstallation_RoutesToSetByName`: JSON decode test
    (B3: `payload.Repositories` top-level).
  - `TestHandleInstallationRepos_AddRoutesToByRepo`: action routing.
  - `TestHandleInstallation_DeletedNoOp`: `deleted`, `suspend`,
    `unsuspend` are no-ops.
- `server/webhook/receiver_test.go`:
  - `FakeDispatcher` / `blockingDispatcher` / `capturePullRequest` all
    implement the extended `Dispatcher` interface.
