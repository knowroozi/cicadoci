# CIC-146 P2: push/PR payloads stamp installation_id for the clone minter

## Context

CIC-146 P1 added `HandleInstallation` and `HandleInstallationRepos` to
`controller.go` and the `InstallationPayload` / `InstallationRepositoriesPayload`
webhook structs, plus the allow-list routing.  Those handlers stamp
`repos.installation_id` from an explicit `installation` or
`installation_repositories` webhook.

However, GitHub's `push` and `pull_request` events also carry an
`installation` sub-object — every App-delivered webhook includes it — but
those payloads didn't decode it, so a repo's first push could arrive
*before* any installation event, leaving `installation_id = NULL` when the
clone minter needs it.

## What this adds

1. **`webhook.Installation` type** — extracted the inline `Installation`
   struct (previously in `InstallationPayload`) into a reusable global
   type `Installation{ID int64; Account{Login string; Type string}}`.
   Used by `PushPayload`, `PullRequestPayload`, `InstallationPayload`,
   and `InstallationRepositoriesPayload`, so all four payloads share
   one struct and one JSON tag.
2. **`PushPayload.Installation` / `PullRequestPayload.Installation`** —
   two new fields with `json:"installation"` that decode the GitHub
   installation object on every push and PR event.
3. **`controller.resolveRepo(ctx, owner, repoName, installationID)`** —
   the helper now passes the installation ID directly into
   `store.ResolveRepo`, which upserts `repos.installation_id` when
   non-empty. `HandlePush` and `HandlePullRequest` call
   `resolveRepo(ctx, owner, repoName, fmt.Sprintf("%d", payload.Installation.ID))`.
   A zero ID (unset installation) is passed-through as `""`, and
   `ResolveRepo` no-ops an empty string, so an existing `installation_id`
   is preserved. This replaces the earlier separate
   `SetRepoInstallationIDByName` call with a single write path.

## Design decisions

| Decision | Rationale |
|---|---|
| Pass `installationID` into `resolveRepo`/`ResolveRepo` | `ResolveRepo` already supports an `installationID` param and no-ops for empty; one write per event instead of two. |
| Extract `Installation` to a global type | Reduces duplication across four payload structs and makes the type reusable. JSON tags are identical to previous inline versions. |
| `fmt.Sprintf("%d", payload.Installation.ID)` | `repos.installation_id` is `TEXT` in the schema. |
| Zero-ID passes `""` through to `ResolveRepo` | `ResolveRepo` skips the UPDATE when `installationID == ""`, so an existing `installation_id` is preserved. |

## Out of scope (→ CIC-180)

| Item | Why deferred |
|---|---|
| `main.go` TODO(CIC-146) removal and producer token source wiring | The producer token source must be wired in addition to this stamp; it is a separate concern tracked by CIC-180. |

## Tests added

| Test | Verifies |
|---|---|
| `TestHandlePush_InstallsInstallationID` | Push with `Installation.ID=12345` → `repos.installation_id = "12345"` |
| `TestHandlePullRequest_InstallsInstallationID` | PR with `Installation.ID=54321` → `repos.installation_id = "54321"` |
| `TestHandlePush_ZeroInstallationIDIsNoOp` | Push with `Installation.ID=0` preserves a pre-existing `installation_id` |

## Files touched

| File | Change |
|---|---|
| `server/webhook/events.go` | Add global `Installation` type; add `Installation` field to `PushPayload` + `PullRequestPayload`; fix `InstallationRepositoriesPayload` action list |
| `server/controller/controller.go` | `resolveRepo` now takes `installationID string`; `HandlePush`/`HandlePullRequest` pass `fmt.Sprintf("%d", …)`; stale CIC-81 comment removed |
| `server/controller/p2_install_test.go` | Three DB-backed tests; zero-ID test asserts existing ID is preserved |
| `docs/decisions/CIC-146-part2-installation-stamp.md` | This doc |

## Unverified

- No `make check` run included in this commit (Postgres required);
  the three new DB tests are skipped without `CICADO_TEST_DATABASE_URL`.
  CI or `scripts/with-postgres.sh` should run the full suite.
- The `main.go` wiring TODO and producer token source are tracked in CIC-180,
  not this PR.
