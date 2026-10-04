# CIC-148: Per-Job GitHub Clone Token

Decision: for a non-fork PR or push run where the repo's `git_clone_url` is
non-empty, the controller injects `github_clone_token` into both the
job's `secret_refs` and the checkout step's env, so the runner can
authenticate a private-repo clone via `GetJobSecrets`.

Fork PR runs get **no** token (defense-in-depth: `secretRefsFor` returns
`nil` unconditionally for fork runs).

This decision is recorded per the 2026-09-28 owner policy; CLAUDE.md's
decision table is left untouched.

---

## 1. What landed (B1-B4 + N1 + N2 fixes)

### `secretRefsFor(j, isFork, gitCloneURL)`

```go
func secretRefsFor(j *workflow.Job, isFork bool, gitCloneURL string) []string {
    if isFork {
        return nil    // fork PRs get NO secrets, ever
    }
    // ...collect user-declared refs...
    if gitCloneURL != "" {
        seen[githubCloneTokenSecretName] = struct{}{}
    }
    // ...
}
```

- `githubCloneURL` is the new third parameter.
- When `gitCloneURL != ""` and `isFork == false`, the string
    `"github_clone_token"` is added to the `seen` set alongside
    user-declared secrets.
- Fork PRs return `nil` unconditionally; `github_clone_token` never
    appears in a fork PR's `secret_refs` regardless of `gitCloneURL`.

### `cloneStepSpec(url, ref, sha, isFork)`

Non-fork runs with a `gitCloneURL` get a step `Env`:
`{"CICADO_GITHUB_TOKEN": "${{ secrets.github_clone_token }}"}`.
The `$` prefix is required by `secretexpr.secretTemplateRe`.
Fork PR runs get `Env = nil` (no token, no credential).

### B1 — Fix secretexpr syntax

The original draft used `{{ secrets.github_clone_token }}` (without the
`$` prefix). `secretexpr.Resolve` uses regex
`secretTemplateRe = \`\\$\\{\\{\\s*secrets\\.(NAME)\\s*\\}\\}\\``,
which requires the `$` prefix. Without it, the raw template was passed
through literally and private clones failed. Fixed in `cloneStepSpec`.
Proven by `TestCloneStepSpec_NonForkEnvResolvesViaSecretExpr` in
`server/controller/secretrefs_test.go`, which calls
`secretexpr.Resolve(raw, secrets)` with a sentinel token and checks
the exact output.

### `gitCredentialHelper` constant & `buildCloneScript`

```go
// gitCredentialHelper is a package-level constant so B3 tests can
// reference it directly without parsing the script.
const gitCredentialHelper = `!f() { echo username=x-access-token; ` +
    `echo password="$CICADO_GITHUB_TOKEN"; }; f`
```

N1: changed from `git config credential.helper='...'` to
`git -c credential.helper='...' fetch`, so the helper is scoped to
the single fetch invocation and not persisted to `.git/config` for
later repo-controlled steps.

B2: wrapped in `if [ -n "$CICADO_GITHUB_TOKEN" ]; then git -c ... fetch;
else git fetch; fi`. B2 normalises `CICADO_GITHUB_TOKEN` to `""` when
the token is absent or whitespace-only, so the guard selects the
anonymous-fetch path.

### `unresolvedSecrets` — B2 skip

`github_clone_token` is a reserved optional secret. `unresolvedSecrets`
skips it: when it is absent from `secrets`, the job does NOT fail
as `secret_unavailable`.

### `resolveOptionalSecrets` — B2 + B4

```go
func resolveOptionalSecrets(stepEnv, stepRaw map[string]string,
                            secrets map[string]string) map[string]string
```

Three-arg signature (added `stepRaw` for B4):
1. If `stepEnv` has no `CICADO_GITHUB_TOKEN` key, return as-is.
2. Look up `stepRaw["CICADO_GITHUB_TOKEN"]`; if the raw value does not
    reference `github_clone_token` (e.g. it references `my_pat` or a
    literal), return as-is. This is the B4 fix.
3. If the raw value DOES reference `github_clone_token` AND the token
    is absent or whitespace-only in secrets, blank `CICADO_GITHUB_TOKEN`
    to `""`.
4. If the token is present and non-whitespace, pass through unchanged.
Proven by `TestResolveOptionalSecrets_UserMyPatPreserved` and
`TestResolveOptionalSecrets_UserMyPatAndCloneTokenAbsent` in
`runner/secrets_test.go`.

### B3 — Runner-level executeJob tests

In `runner/job_test.go`:
- `TestGithubCloneTokenNoSecretJobSucceeds`: step env
    `CICADO_GITHUB_TOKEN=${{ secrets.github_clone_token }}`, `SecretRefs`
    contains `github_clone_token`, but no secret is delivered
    (newTestAgent uses nil client). The step runs
    `test -z "$CICADO_GITHUB_TOKEN"` and succeeds.
    Mutation proof (M1): remove `resolveOptionalSecrets` call in
    `runner/job.go:203` → test fails.
- `TestGithubCloneTokenWhitespaceJobSucceeds`: same as above with
    `secrets = {"github_clone_token": "    "}`. B2 normalises to
    `""`; test passes.

In `server/controller/clone_e2e_test.go`:
- `TestBuildCloneScript_E2E_BareRepo`: real `file://` bare git repo,
    three subtests (no_token / sentinel_token / whitespace_token),
    passes the REAL commit SHA to `buildCloneScript`, asserts
    `git rev-parse HEAD` matches the expected SHA after the script runs.
- `TestGitCredentialHelper_Fill`: drives
    `git -c credential.helper=<const> credential fill` with
    `stdin: protocol=https\nhost=example.com\n\n`, verifies the output
    contains `password=<sentinel>` and `username=x-access-token`.
- `TestGitCredentialHelper_EmptyTokenProducesEmpty`: with
    `CICADO_GITHUB_TOKEN` unset, the helper produces an empty password
    (`password=`), verifying it does not leak a stale token value.

All three tests are real shell/git invocations, not pure string checks.
Mutation proofs:
- M1 (delete `resolveOptionalSecrets` call): B3c tests fail.
- M3 (`if true` in `resolveOptionalSecrets` — always blank): B3c
    `TokenDelivered` unit test fails (token present but forced to "").
- M4 (helper prints wrong password, e.g. `password=$FOO`):
    `TestGitCredentialHelper_Fill` fails (output does not match
    sentinel).

### N2 — optional exemption scoping

N2 asks whether the `github_clone_token` optional-secret exemption
should apply to every step or only the checkout step.
**Decision**: the exemption is scoped globally but the controller
only injects `github_clone_token` into the CHECKOUT step's env.
User steps that reference `github_clone_token` in their own env will
have it treated as optional (no error if absent), which is acceptable
behaviour: a user who writes `${{ secrets.github_clone_token }}` in a
step but has not provisioned it gets an empty string, which is safe
(worse than a hard failure is an auth error somewhere downstream,
but the clone step has a guard). Documented here as the chosen
behaviour; a scoped-to-checkout-only exemption would require
passing `stepIndex` to `resolveOptionalSecrets`, which is not worth
the complexity.

---

## 2. Why this design

### `github_clone_token` is the canonical secret name
The secret is stored by the operator under the key `github_clone_token`
in the org+repo namespace. The controller references it by name; the
runner resolves it by name via `GetJobSecrets`. This keeps the secret
value out of the persisted `steps` table (readable by all runners)
and out of the job row's `env` column.

### Token is never in `Command` or the job row
The `steps` table is readable by all runners on the server, so
embedding a token in `Command` would leak it on any `GetJob`. The
credential is injected as a step env var whose value is resolved
only by the live lease-scoped `GetJobSecrets` RPC.

### Fork PR defense-in-depth
`secretRefsFor` returns `nil` for fork PRs unconditionally, and
`cloneStepSpec` sets `Env` to `nil` for fork PRs. These are two
independent guards; a mutation to one is caught by the other.

### B4: scoped blanking prevents user PAT clobbering
The original B2 design blanked `CICADO_GITHUB_TOKEN` for every step
whenever `github_clone_token` was absent. A user who sets their own
`CICADO_GITHUB_TOKEN: ${{ secrets.my_pat }}` in a different step would
silently have that token clobbered to `""` on every run. B4 scopes
the blanking to references that actually name `github_clone_token`,
via `secretexpr.NamesIn(raw["CICADO_GITHUB_TOKEN"])`.

### Why `github_clone_token` is optional (B2)
A deployment may run public repos where no token is needed, or private
repos where the operator has not yet provisioned a token. Requiring
the token unconditionally makes every public-repo run fail with
`secret_unavailable` at checkout, which is the wrong outcome. The
B2 approach (blank to empty → anonymous fetch) means public repos
work as expected, and unconfigured private repos fail with a clear
`remote: error: Git HTTP: Authentication required` — the expected
degradation until CIC-146/151 mints tokens.

---

## 3. Mutation proofs

| Remove / break                                   | Test that fails |
| ----------------------------------------------- | ----------------- |
| `secretRefsFor` returns `nil` for `gitCloneURL != ""` | `TestSecretRefsFor_GitCloneToken` |
| Remove `isFork` guard in `secretRefsFor`        | `TestSecretRefsFor_ForkJobNoGitCloneToken` |
| `cloneStepSpec` sets `Env` for `isFork=true`    | `TestCloneStepSpec_ForkHasNoCredentialEnv` |
| Use `{{ secrets.NAME }}` without `$`            | `TestCloneStepSpec_NonForkEnvResolvesViaSecretExpr` |
| `git config` instead of `git -c`                | `TestBuildCloneScriptPrefersSha` |
| `unresolvedSecrets` flags `github_clone_token`  | `TestUnresolvedSecrets_SkipsGithubCloneToken` |
| Blank a user's `CICADO_GITHUB_TOKEN` referencing `my_pat` | `TestResolveOptionalSecrets_UserMyPatPreserved` |
| Remove `resolveOptionalSecrets` call in `job.go` | `TestGithubCloneTokenNoSecretJobSucceeds` |
| Helper prints wrong password                    | `TestGitCredentialHelper_Fill` |
| `fetch --depth 1 origin HEAD` instead of target SHA | `TestBuildCloneScriptPrefersSha` (asserts SHA is in script) |
| `buildCloneScript` with `sha=""`               | `TestBuildCloneScript_E2E_BareRepo` (SHA mismatch on `rev-parse HEAD`) |
| B4: blank all `CICADO_GITHUB_TOKEN` when `github_clone_token` absent | `TestResolveOptionalSecrets_UserMyPatAndCloneTokenAbsent` |

---

## 4. What CIC-146 / CIC-151 must still wire

CIC-148 closes the **delivery** path: the controller adds
`github_clone_token` to `secret_refs` and the runner fetches it.
The following remain:

- **CIC-146 / CIC-151**: mint a per-repo GitHub installation access
    token at run-start (not operator-provisioned), deliver it to the
    runner via `GetJobSecrets`.
    `main.go` has a `TODO(CIC-146)` noting the `TokenMinter` is not
    wired. Until wired, operators must provision a token as
    `github_clone_token` secret via the existing secrets API.
    **Note**: a GitHub App installation access token expires after
    ~1 hour. Long-running jobs (longer than the installation token
    lifetime) will receive a stale token. A refresh mechanism
    (re-mint on expiry) is tracked by CIC-151. Until then,
    operator-provisioned PATs with longer lifetimes should be used.

- **Masking**: the runner registers delivered `github_clone_token`
    values in the log redaction masker via `RegisterWithVariants`
    (CIC-113 established the mechanism; CIC-148 relies on it).

---

## 5. Open questions

1. Should `github_clone_token` be a configurable secret key name
    (e.g. `--github-clone-token-name github_clone_token`)?
2. Should the runner log a warning when a non-fork run with a
    `git_clone_url` has no `github_clone_token` provisioned, so
    operators are alerted rather than discovering the 403 silently?
