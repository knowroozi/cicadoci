# CIC-151 / CIC-146: Per-job GitHub installation token minting

_Stacked on PR #123 (CIC-148) head. God's decision: option A (full minter,
RS256 only, no new dependencies)._

## Decision

When the operator configures `cicado-server` with `--github-app-id` and
`--github-app-key-file`, `GetJobSecrets` dynamically mints a short-lived
GitHub App installation token when a job's `secret_refs` include
`"github_clone_token"`, rather than looking up a static, operator-entered
token from the secrets store.

The token is registered with the server-side log masker before it is
returned to the runner (CIC-74), so that any appearance of the value in
log streams or structured events is scrubbed. It is NEVER persisted to
the database.

## Why

CIC-148 (PR #123) introduced per-repo `github_clone_token` delivery.
However, the token the operator places in the secrets store is a
single, long-lived, static credential — a "CIC-146 static-token"
problem (a leaked static token compromises every clone that uses it for
its full lifetime).

CIC-151 closes this gap by replacing the static lookup with a dynamic
App-JWT mint, matching the intent of CIC-146: per-job, per-run,
short-lived, non-persisted, masked-before-return.

## How

1. **App-JWT minter** (`server/github/jwt.go`): `AppAuthFunc` signs
   a GitHub App JWT with **RS256** (PKCS#1v15 + SHA-256) using stdlib
   `crypto/rsa`, `crypto/x509`, `encoding/pem`. **No external
   dependency.** EdDSA is NOT supported (god conv-e2490b requirement #1).
   Non-RSA keys produce a clear error (fail-closed).
    - `iat = now - 30s` (clock skew tolerance).
    - `exp = now + 10 min` (GitHub's `jwtMaxLifetime`).
   `iss = appId` (as string). `kid = appId`.

2. **CloneMinterFunc** (`server/runnersvc/clone_minter.go`):
    `NewCloneMinterFunc(ctx, pool, appID, keyBytes, masker)` returns
   a `CloneMinterFunc` that:
    - Resolves the installation ID from `repos.installation_id` by
      repo UUID (COALESCE handles NULL).
    - Delegates to `github.InstallationTokenMinter.Mint`, which signs
      the App JWT, exchanges it for an installation token via the
      GitHub API, and registers the token with the masker.
    - **Fork runs** (`isFork=true`): minted with `ClonePermissions="read"`
      (CIC-151 / conv-e2490b #2). All other secret refs remain
      refused for `isFork=true` runs (CIC-27). The minter does NOT
      error on `isFork=true` — it produces a `contents:read`-scoped
      token for the clone step.
    - Returns nil when `appID=0` or `keyBytes` is empty.

3. **Git installation token permissions** (`server/github/installation.go`):
    - New `ClonePermissions` field on `InstallationConfig`:
      `"read"` or `"write"`; sent as `permissions: {contents: <perm>}`
      in the GitHub API request.
    - Default: `"read"` (clone only needs read).

4. **GetJobSecrets** (`server/runnersvc/service.go`): when
    `s.cfg.CloneMinter != nil` and `name == "github_clone_token"`,
   calls the minter instead of the static store lookup. If the minter
   returns an error, the token is omitted (fail closed) and the job
   proceeds without it.

5. **main.go wiring** (`server/cmd/cicado-server/main.go`):
    - `--github-app-id` (int64, default 0) and
      `--github-app-key-file` (string, default empty).
    - When both are set, `NewCloneMinterFunc` is called and assigned to
      `svcCfg.CloneMinter`.
    - Backwards compatibility: nil minter → static store path unchanged.

## Tests

- `jwt_test.go`: 7 tests — zero key, empty key, invalid PEM,
  reproducible claims, RSA PKCS#1, RSA PKCS#8, non-RSA key fails
  closed (ECDSA / ED25519 → error), zero installation ID.
- `clone_minter_test.go`: 6 tests — nil when unconfigured, resolver
  error, invalid installation ID, fork-run calls resolver (no fork
  guard), zero installation ID.
- `installation_test.go`: existing tests for minter permissions (if any).
- All existing runnersvc tests pass.

## Nits / scope boundaries

- **Dwight B2** (duplicate `githubCloneTokenSecretName`): not present
  in this branch (PR #124 v1 introduced a duplicate; this PR is
  stacked on `0441ec3` which has no duplicate).
- **Dwight B3** (overwrites static store): minter NEVER writes to the
  secrets store; the value is returned over gRPC only.
- **Dwight B4** (cross-repo leak): per-job minter called in
  `GetJobSecrets` with the job's `repoID`; each job gets a fresh
  token scoped to its installation (and, optionally, its repo —
  `Mint(ctx, installationID, org, repo)` — currently passed
  `org=""`, `repo=""` so the installation's configured perms apply).
- **CIC-146 static-token design**: rejected per god conv-bb0bc1
  (`f5ed1b`); replaced with this dynamic per-job App-JWT mint.
- **Real GitHub E2E**: tracked as a follow-up; the minter makes a real
  HTTP call in production but the test uses a fake key.
- **`contents:read` permission scope**: sent in the GitHub API request
  body as `permissions: {contents: "read"}` so both fork and non-fork
  clone tokens are read-only. Future work: add a `--github-clone-perms`
  flag to allow `write` for repos that need to push tags as a result of CI.

## Not-included (future work)

- Real GitHub API E2E (requires a GitHub App + org).
- Per-installation write permissions (for repos that need to push).
- `--github-installation-id` static override flag for multi-org.
- Token caching within a run (one mint per run, not per job).

## 2026-10-04: DWIGHT REVIEW B1-B4 + S1-S3 fixes

### B1: Fork policy in `GetJobSecrets` + `controller`
- `runnersvc/service.go`: fork runs get `github_clone_token` via the minter only;
  all other static-secret refs are silently skipped (`s.log.Warn` + `continue`).
- `controller/jobs.go`: `secretRefsFor(job, isFork, gitCtx)` returns
  `["github_clone_token"]` for fork jobs when a git clone URL is present; no
  static refs. `cloneStepSpec` sets `CICADO_GITHUB_TOKEN` for all clone steps
  (removed the `!isFork` guard).

### B2: Per-repo Mint with real repo name
- `clone_minter.go`: `repoInstallationID` now returns
  `(installationID int64, repoName string, err error)`. The minter calls
  `Mint(ctx, installationID, "", repoName)` — `repo=""` (cover-all) is removed;
  the token is scoped to the specific repository.

### B3c: Fail-closed minter failure
- `runnersvc/service.go`: when `CloneMinter` returns an error the call now
  returns `status.Error(codes.Internal, ...)` instead of a `continue`. The
  job fails with `secret_unavailable` and the runner cannot proceed.

### B4: Mutation-proven gRPC tests
- `server/runnersvc/service_minter_test.go` (new): three tests using
  `testdb`, a real Postgres DB, and `metadata.NewIncomingContext` to
  exercise `Service.GetJobSecrets`:
   - `TestCloneMinter_NonForkGetsToken`  — minter called, token in response.
   - `TestCloneMinter_MinterFailureOmitsToken` — minter error → gRPC error.
   - `TestCloneMinter_ForkGetsToken` — fork run gets token; static secret absent.

### S1: `jwtMaxLifetime` = 9 minutes
- `github/jwt.go`: `9 * time.Minute` (well under the 10-minute GitHub TTL).

### S2: Captured request ctx
- `clone_minter.go`: `repoInstallationID` uses `reqCtx` (the gRPC request
  context), not the captured `context.Background()`.

### S3: `s.secrets != nil || s.cfg.CloneMinter != nil`
- `runnersvc/service.go`: the early-skip only fires when BOTH store and
  minter are nil. If either is set, the loop runs.

### `main.go` B3a/B3b
- `main.go`: B3a — `svcCfg.Masker` must implement `redact.RegisteringMasker`;
  a non-`RegisteringMasker` returns an error. B3b — `githubpkg.AppAuthFunc`
  probes the key file's format at startup.

### B5: `installation_id` empty ⇒ soft skip, not hard error
- `clone_minter.go` resolver: when `coalesce(installation_id,'')` is empty OR
  parses to 0, returns `(0, "", nil)` — not an error. Outer closure skips
  minting (`if installationID == 0 { return "", nil }`).
- **Rationale**: `repos.installation_id` is set only by a webhook install
  event (CIC-146 / CIC-81). Before that event, a public repo or a repo
  managed by an admin user (no App) has an empty `installation_id`.  A
  hard error there would fail every job on such repos when
  `--github-app-id` is configured. B5 changes the behavior to
  *anonymous clone* (public/unauthenticated `git clone`).
- **Scope**: the resolver still returns a **hard error** for:
  - a parse error (non-numeric `installation_id` in the DB); or
  - a real HTTP/auth failure from the GitHub API (propagated via B3c
   to a `codes.Internal` failure for the job).
- **Tests**: `TestNewCloneMinterFunc_B5NoInstallationID` and
  `TestNewCloneMinterFunc_B5ZeroInstallationID` assert `("", nil)` is
  returned; `TestNewCloneMinterFunc_InvalidInstallationID` still
  asserts the parse error is propagated.

### B4 (expanded): fork + no-minter test cases
- `TestCloneMinter_ForkGetsToken` now also asserts that a static
  secret ref (`"other_secret"`) is absent from the response and that
  `secrets.Store.Get` was never called (B4a security boundary
  proof; uses `failingStore`).
- `TestCloneMinter_ForkNoMinter` (new, B4b): a fork run with
  `CloneMinter == nil` and `secret_ref = ["github_clone_token",
  "other_secret"]` returns an empty secrets map and never calls
  `secrets.Store.Get`.

### Nits
- `mintWithJitter` renamed to `mintCloneToken`; dead params `_repoID`,
  `isFork` dropped.
- `clone_minter.go` doc comment corrected to match B5 behavior.
- `service.go` B3c error text simplified to `"clone minter failed"`.

### B4a mutation-proof fix
- `NewCloneMinterFunc` now accepts variadic `NewCloneMinterFuncOption`
  (see `WithBaseURL`, `WithHTTPClient`).  Tests can inject an httptest
  server so that `clone_minter_httptest_test.go` proves the HTTP request
  body (repositories + permissions) through the *real* closure, not a
  direct construct of `github.InstallationTokenMinter`.
- `TestB2_ThroughClosure`: asserts the request body contains
  `repositories:["b2repo"]` and `permissions:{contents:"read"}` when
  called through `NewCloneMinterFunc(..., WithBaseURL, WithHTTPClient)`.
- `TestB2_B5NoInstallationID`: asserts B5 soft-skip — zero HTTP requests
  when `installation_id` is not set (the `cloneMinter` returns `("",""`,
  `nil)` without any HTTP traffic).

## Fork PR policy (CIC-148 vs CIC-151 reconciliation)

### Conflict

CIC-148 (PR #123) decided: fork PR runs get `nil` `secret_refs`
unconditionally — the clone step has no credentials and the runner does
an unauthenticated `git clone`.  This is correct for **public** repos
(a fork of a public repo is public and cloneable without auth) but
**broken** for **private** repos: a fork PR on a private repo needs
some credential to clone.

CIC-151 (PR #124) supersedes CIC-148 for fork runs.

### Ruling

**Fork PRs get ONLY `github_clone_token`** — a per-job, per-run,
short-lived, contents:read-scoped GitHub App installation token
*minted* by the clone minter. They get NO static secrets.

Implementation:
- `controller.secretRefsFor` returns `[]string{"github_clone_token"}`
 for `isFork=true` runs (CIC-151 override of CIC-148's unconditional nil).
- `controller.cloneStepSpec` sets `Env` for the git clone step
  (`CICADO_GITHUB_TOKEN`) for ALL runs, including fork runs, when a
  `github_clone_token` entry exists.
- The `github_clone_token` minter (CIC-151) mints a contents:read
  installation token for both fork and non-fork runs.  Fork runs get
  the same scope.
- `service.GetJobSecrets` (CIC-151 B1+B3): for `isFork` runs, the
   minter is called and its secret returned, but NO static secrets are
   delivered — the per-ref `if isFork { continue }` guard enforces this.
  B5 soft-skip (`installation_id` not set yet): the minter returns
   `("", nil)`; `GetJobSecrets` omits `github_clone_token` from the
   response; the runner gets an anonymous clone — correct for public
   repos.

### Rationale — why this is safe

1. The token is **dynamic and short-lived**: the App JWT used to
   obtain it has a 9-minute max lifetime (`jwtMaxLifetime`, S1; GitHub
   policy is 10 min); the returned installation token has GitHub's
   default up-to-1-hour TTL.   Registered with the server-side log
   masker before it leaves the server and NEVER persisted.   A leaked
   value is scrubbed from all log streams (CIC-74) and expires within
   one hour.
2. The scope is **contents:read** — the minimum permission for
  `git clone`.  No `actions`, no `admin:*`, no `pull_requests`, no
  `contents:write`.  The token cannot write to the repo or read other
   GitHub API surfaces.
3. The token is bound to the specific **GitHub App installation**
   (per-installation-scoped installation ID resolved from
   `repos.installation_id`).  It does not leak to other repositories or installations.

     **Per-repo scoping** (CIC-26/CIC-123): the token request embeds
     `repositories: ["repoName"]` in the GitHub API call body, scoping
   the returned token to the single repository named in `repos.name`;
   `repoInstallationID` resolves this via `JOIN repos ON repos.name`.
4. **Fork boundary** (`if isFork { continue }` in `GetJobSecrets`):
   even if a static secret were somehow included in `secret_refs`
  for a fork run, `GetJobSecrets` skips it — fork PRs can NEVER
   receive a static secret.  The B4a test
  (`TestCloneMinter_ForkGetsToken`) proves this via
  `failingStore`: `secrets.Get` is never called for
   fork runs.

5. **Risk acceptance**: fork PR step code *can* read the `contents:read`
   token via `$CICADO_GITHUB_TOKEN`.  The scope is `contents:read`
   only (no write, no non-contents GitHub API surfaces) with an
   up-to-1h TTL; the value is scrubbed from log streams (CIC-74)
   before it leaves the server process.

### Effect on CIC-148's fork test

CIC-148's `secretRefsFor` returned `nil` for `isFork=true` runs;
CIC-151 overrides this to return `["github_clone_token"]`.    When
#124 merges, the CIC-148 controller test `TestSecretRefsFor_Fork`
(in #123) has been updated to assert `["github_clone_token"]` instead
of `nil`.   This is a known, intentional reconciliation — CIC-151
supersedes CIC-148's fork policy.
