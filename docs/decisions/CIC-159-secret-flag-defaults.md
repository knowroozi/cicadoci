# CIC-159 — Secret/flag env-defaults leak: single shared `envSecret` resolver

## Context

`server/cmd/cicado-server/main.go` registered its flags with Go's `flag` package.
Several used `os.Getenv(...)` as the flag _default_. Go's `flag` prints `(default
"VALUE")` after every flag's usage string; when a program is invoked with `-h`/
`--help` — which `flag` intercepts before `flag.Parse()` — the value shows in the
usage text even when the flag was not passed. This leaks secrets to any reader who
runs `./cicado-server -h`, reads its `--help` output, captures stdout via CI logs,
or inspects `ps` for the flag string.

The flag is not the *source* of the secret — `CICADO_*` is — but having the flag
default to that value (with a fallback to empty) leaks it through the usage
channel, which is not a place anyone expects a secret to live. `--help` is
typically run by a first-time operator; a leak there is the highest-signal
disclosure path.

## Decision

### The 4 original CIC-159 flags + `--database-url` (folded in)

Four flags carried the leak, and `--database-url` shares the class (a connection
DSN carries a DB password). All five are fixed identically:

- `--webhook-secret`        `CICADO_WEBHOOK_SECRET`
- `--secret-key`          `CICADO_SECRET_KEY`
- `--github-workflow-token` `CICADO_GITHUB_WORKFLOW_TOKEN`
- `--github-app-token`      `CICADO_GITHUB_APP_TOKEN`
- `--database-url`          `CICADO_DATABASE_URL`

For each:
1. Flag **default is `""`** (empty, not `os.Getenv("CICADO_…")`).
2. **After `flag.Parse()`** in `run()`, each flag's value is resolved through the
   shared `envSecret` helper (step "Resolution after flag"), so the effective
   value equals what the operator supplied via `CICADO_…` _or_ an empty string,
   and a whitespace-only env value fails closed.
3. Flag usage string notes "(read from CICADO_… after flag.Parse())" so a
   human/developer knows what the env var is.

Behaviour is otherwise unchanged: the effective value is the _same_ value the
operator would have set via `CICADO_…` before this change. No caller behaviour
differs at runtime — secrets-store / forge-producer / checks-reporter /
webhook-signer / DB-connection all receive the same value as before. The change
is only what the **flag default** carries when the flag is not given.

### Why `--database-url` is in scope

A DSN carries a DB password in the common case (`postgres://user:pw@host:5432/`).
Same leak class; same fix; no behaviour change. It is the one place I would have
a strong "fold" argument because the leak is a password, not an optional secret.

### Why `envSecret`, not per-flag inline

Dwight's B1 (PR #119): the original `TestSecretEnvFallbackAfterParse` re-implemented
`TrimSpace(os.Getenv(key))` in the test. The test was green even when run()'s
CICADO_SECRET_KEY fallback was removed, because the test exercised a copy of run()'s
logic, not run() itself. That is the same anti-pattern B1 on #116 flagged.

Fix: one shared `envSecret(flagVal, key string) string`, called by run() for every
flag AND by the tests directly. The flag wins when non-empty (used verbatim); an
empty/whitespace flag falls back to `strings.TrimSpace(os.Getenv(key))`. The
helper is the single source of truth; the tests exercise it, not a copy.

```go
// envSecret is the single source of truth for every secret / token flag that
// keeps an empty default and falls back to a CICADO_* env var after flag.Parse().
// The flag value wins when non-empty (used exactly as typed); an empty or
// whitespace-only flag falls back to os.Getenv(key) trimmed.
func envSecret(flagVal, key string) string {
	if strings.TrimSpace(flagVal) != "" {
		return flagVal // the flag wins as typed.
	}
	return strings.TrimSpace(os.Getenv(key))
}

// resolveAPIToken is a thin named wrapper over envSecret for the CICADO_API_TOKEN
// case; run() calls it so the CIC-150 B4 path reads literally.
func resolveAPIToken(flagVal string) string { return envSecret(flagVal, "CICADO_API_TOKEN") }
```

### Why not a `--help` intercept

The `flag` package intercepts `-h`/`--help` before `flag.Parse()` and prints the
standard usage message with the flag defaults. There is no user-extensible hook in
`flag` to sanitize the usage text. A custom usage printer would fight the stdlib
and complicate onboarding. Empty defaults + post-parse env fallback is the minimal,
standard pattern.

### Why no per-flag "leak" unit test per flag

Adding a unit test per flag is brittle (a new flag silently becomes a leak if the
default is re-added). The regression test is a binary smoke test
(`TestSecretFlagValuesNotInFlagUsage`) that builds the real `cicado-server`
binary via `go build`, runs it with each `CICADO_*` set to a distinctive sentinel,
captures stdout+stderr under `-h`, `--help`, and an unrecognised-flag invocation,
and asserts the sentinel appears nowhere in the output. This catches both the
original 5 flags and any future flag that reintroduces `os.Getenv(...)` as a
default without a new test being written.

### Test suite (3 tests, each proven to fail when its fix is reverted)

- `TestSecretFlagValuesNotInFlagUsage` — binary leak test, 6 sentinels
    (incl. `CICADO_API_TOKEN` per god's merge-order note). Reverts each flag to
     `os.Getenv(...)` and asserts the leak test catches it.
- `TestSecretEnvFallbackAfterParse` — drives the SAME `envSecret` helper run()
  uses, with empty flag + a `CICADO_*` env var. Verifies flag-wins
   (`envSecret("value-with-spaces", key) == "value-with-spaces"`) and
   env-fallback (`envSecret("", key)` returns the trimmed env value; a
   whitespace-only env returns `""`).
- `TestDBEnvFallbackIsWiredIntoRun` — binary proof that run() actually consumes
  the `CICADO_DATABASE_URL` fallback: with a (falsely) reachable DSN supplied via
       the env var and no `--database-url` flag, the "no database configured"
      error must be ABSENT (run consumed the fallback and instead failed at the DB
      connect). Dropping run()'s `envSecret` call for the DSN makes the error
      reappear and fails the test. The other 4 secret flags are not observable at
      start-up without a running server, so their run()-wiring guard is the shared
      `envSecret` helper exercised by TestSecretEnvFallbackAfterParse.

Regression proofs (all verified, each in both directions):

| Fix reverted | Test that fails | Why |
|---|---|---|
| `--secret-key` → `os.Getenv(...)` | `TestSecretFlagValuesNotInFlagUsage` | leaked sentinel in usage |
| `envSecret` drops `TrimSpace` | `TestSecretEnvFallbackAfterParse` | whitespace env not trimmed |
| run() drops `envSecret(*dbURL, …)` | `TestDBEnvFallbackIsWiredIntoRun` | "no database configured" reappears |

### Merge order

Per god's merge-order instruction (2026-10-03, `conv-502685`): #116 (CIC-150)
merges first. This branch is rebased onto `knowroozi/cic-150-api-auth`
(once #116 lands, onto `master`), because both PRs touch
`server/cmd/cicado-server`'s `var(...)` flags in the same function. This branch
shares `resolveAPIToken` (the #116 B4 entry point) with the `envSecret`-based
resolver: `resolveAPIToken` is a thin wrapper over `envSecret`, so the api-token
path and the CIC-159 B1 path use the same helper. The binary leak test's sentinel
map includes `CICADO_API_TOKEN`, catching a regression on the api-token flag too.

### No change to `--insecure-*` flags

`--insecure-webhook` and `--insecure-api` are already `envBool(..., false)`
default. A secret default is not a security concern for a bool; leave alone.

### Why not `secrets.Store` or `auth.Store`

The leak is at the flag-registration layer, at the CLI boundary of `main`.
`secrets.Store` / `auth.Store` are the downstream consumers; they do not
participate in flag registration. No change to those.

## Round 3 per dwight's review on `1832a4c` (2026-10-03, `conv-...`) -- B1 + N1 + N2

1. **B1 -- run-level guard was missing**: dwight deleted each of the six
   `*field = envSecret(...)` assignments in `run()` one at a time; only `*database-
   url` failed `TestDBEnvFallbackIsWiredIntoRun`. The others left every test green
   because `TestSecretEnvFallbackAfterParse` exercised `envSecret` directly, not
   run()'s call. **Fix**: all six assignments moved into
    `resolveSecretFlags(f *serverFlags)`, the single call `run()` makes after
     `flag.Parse()`. A new unit test `TestResolveSecretFlags` exercises this function
     on a fresh FlagSet with all six CICADO_* vars set to whitespace-padded sentinels,
      and asserts every field of `f` is the expected trimmed value. Proven:
    - deleting any one of the six lines in `resolveSecretFlags` fails
         `TestResolveSecretFlags`;
    - removing the `resolveSecretFlags(f)` call from `run()` fails
         `TestDBEnvFallbackIsWiredIntoRun`.
2. **N1 -- comment in `flag_leak_test.go` said "the four secret flags are not
   observable at start-up"**: wrong -- the webhook start-up guard runs first, then
   the API guard, then the DB check. N1 fix: the comment reworked to note the guards
   run before the DB check, that the individual-assignment catch is `TestResolve-
   SecretFlags` at the unit level, and that `TestDBEnvFallbackIsWiredIntoRun` pins the
   run-level guard on the most observable field (the database URL).
3. **N2 -- `flagwins` subtest value `"value-with-spaces"` contained no spaces**: trivial
     fix -- changed to `"value with interior spaces"` to actually exercise the
    "interior spaces preserved on win" invariant. `TestSecretEnvFallbackAfterParse/
    flagwins/*` and `TestResolveSecretFlags` flag-wins case updated accordingly.

## Proofs (round 3)

| Revert | Fails |
|---|---|
| `--secret-key` default → `os.Getenv(...)` | `TestSecretFlagValuesNotInFlagUsage` (all 6 sentinels leak) |
| `envSecret` drops `TrimSpace(...)` | `TestSecretEnvFallbackAfterParse/*` (all 6 cases see whitespace) |
| `run()` drops `resolveSecretFlags(f)` call | `TestDBEnvFallbackIsWiredIntoRun` (all 6 flags stay "" together; DB error fires) |
| Any one of the 6 lines in `resolveSecretFlags` removed | `TestResolveSecretFlags` (per-field per-assignment) |

## Consequences

- `--help`/`--bogus` no longer carry `CICADO_*`-set secrets in the usage text;
  the 4 flags + `--database-url` carry an empty default so no leak occurs even
  if `CICADO_*` is still set in the operator's environment.
- Effective value is unchanged: the env var still flows through; nothing is
  broken by this PR at runtime.
- A new flag in `registerFlags` that uses `os.Getenv(...)` as the default will be
  caught by `TestSecretFlagValuesNotInFlagUsage` (binary smoke), no new tests are
   needed per new flag.
- A new env-fallback regression (dropped `TrimSpace`, dropped run() call) is
  caught by `TestSecretEnvFallbackAfterParse` + `TestDBEnvFallbackIsWiredIntoRun`.

## Related

- `docs/decisions/CIC-80-webhook-fail-closed.md` (the fail-closed / fail-open
  pattern; this PR is the *leak* class, orthogonal to the fail-closed guards).
- `docs/decisions/CIC-150-api-authentication.md` (B4 established the empty-default
    + post-parse env pattern for `--api-token`; this generalises it to the other
    4 secret flags + `--database-url` via a single shared `envSecret` helper.)
- `docs/decisions/CIC-82-wire-built-subsystems.md` (the `--github-*` producer
    / checks-reporter flags; the leak fix here does not change their effective
    value or the wiring).
- DW-21-2026-10-03: DW #119 review B1 — fallback test must call the shared helper
     `envSecret` (not a re-implemented copy), run() must call it for every
     secret (B4p2 / #116 pattern, generalised).
