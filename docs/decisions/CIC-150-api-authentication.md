# Decision record: CIC-150 — fail-closed API authentication

## What this ticket fixes

CIC-82 wired the built M4/M7/M9 subsystems into the shipped binary, including
the per-job live log stream at `GET /api/v1/logs?job=`. But the only
authenticator in the shipped binary was `noOpAuthenticator` — `main.go`'s
`api.New` call supplied none, and `AllowUnauthenticated` was documented as
*webhook-only*. The result: anyone who could reach the HTTP port could read any
job's live output. CIC-82's own decision doc flagged this as a known issue
(`CIC-82-wire-built-subsystems.md:17`).

This ticket closes that hole: `/api/v1/*` now **fails closed** behind a
configured operator bearer token, with a narrow, explicit dev opt-out.

## Scope (and the deliberate out-of-scope part)

**In scope — this PR:**
- A `staticTokenAuthenticator` that requires a matching `Authorization: Bearer
  <token>` on every `/api/v1/*` request, compared in constant time
  (`crypto/subtle.ConstantTimeCompare`), rejecting a missing, wrong, or
  non-Bearer credential with 401.
- `WithAPIToken(secret)` on `api.New` — arms the authenticator when `secret` is
  non-empty; an empty secret leaves the no-op authenticator in place (the dev
  path), which the start-up guard forbids absent `--insecure-api`.
- Two flags/env on `main.go`: `--api-token` / `CICADO_API_TOKEN` and the
  dev-only `--insecure-api` / `CICADO_INSECURE_API` (mirroring `--insecure-webhook`).
- `apiStartupGuard` — refuses to start when the token is empty **and**
  `--insecure-api` was not passed, so a production server is never left wide
  open. This is the operational half of CIC-150, parallel to `webhookStartupGuard`.
- Exemptions, kept explicit and narrow:
  - `/api/v1/webhooks/*` → exempt (GitHub authenticates via the HMAC signature,
    not a user session).
  - `/healthz`, `/readyz` → exempt (an orchestrator must tell "alive" from
    "crashed" without the operator credential).
- Tests: the authenticator in isolation, the health-probe exemption, and the
  end-to-end gate through the mux (401 with no/incorrect token on both the read
  route and the SSE log stream; 200 with the correct token; health probes
  exempt); plus the empty-token dev path.

**Deliberate out of scope — the follow-up:**
- Acceptance #2, "a request authenticated as org A reads org B's job → 404", is
  **cross-tenant org-scoping**. The static token is intentionally the operator's
  *unrestricted master credential*; per-principal org identity needs a principal
  model, which does not exist yet — that is CIC-44 (GitHub OAuth principals).
  The org-scoping seam (resolve job → run → org, deny on mismatch) is parked
  against CIC-44, gated on CIC-150 landing first. This PR closes the unbounded
  exposure; per-tenant isolation is the next layer.

## Why a static bearer now, not DB tokens

The static operator token is the fastest correct fail-closed fix and it is one
of the two credential forms CIC-150's Fix #1 offers ("static bearer, **or** an
api-kind token from the `tokens` table"). A DB-backed `api`-kind token is
strictly better operationally (rotation, per-call, revocation), but it pulls in
the principal model that CIC-44 owns. Landing the static gate now removes the
unbounded hole; the api-kind token is natural future work that reuses the same
`authenticator` interface — a one-line `WithAuthenticator` swap, exactly the
seam CIC-82 built.

## Review fixes (dwight-muo6jm3k, PR #116)

Four items addressed, each with a regression test:

- **B1 — token leaked via flag usage** (`cmd/cicado-server/main.go`): the
   `--api-token` flag's default was `os.Getenv("CICADO_API_TOKEN")`, so Go's flag
   package printed the secret in the usage text on `--help` or any mistyped flag,
   leaking it to stderr and into container logs. Fixed by giving the flag an
   **empty default** and reading `CICADO_API_TOKEN` **after `flag.Parse()`**. Behavior
   is unchanged; the secret is gone from every usage-print surface;
   `TestAPITokenNotPrintedInFlagUsage` pins it.
- **B2 — `apiStartupGuard` had no test** (`cmd/cicado-server/main_test.go`): added
   `TestAPIStartupGuardFailsClosedOnMissingToken`, the exact case table requested,
   modeled on the existing `webhookStartupGuard` test.
- **N1 — webhook exemption too loose** (`server/api/auth.go`): the raw
   `strings.HasPrefix(r.URL.Path, "/api/v1/webhooks")` also matched
   `/api/v1/webhooks/../runs` and `/api/v1/webhooksX`. Replaced with a helper
   `pathIsWebhook` that `path.Clean`s first, then accepts the prefix only when the
   cleaned path equals the prefix or matches a real sub-path (`prefix + "/…"`).
   `TestWithAuthenticator_WebhookExemptionResistsTraversal` pins that a traversal past
   the prefix is authenticated, not exempted.
- **N2 — `Bearer` scheme case-sensitive and untrimmed** (`server/api/auth.go`):
   `bearerToken` now matches the scheme case-insensitively (`strings.EqualFold`, per
   RFC 7235) and trims surrounding whitespace on both the presented token and the
   configured secret, so a token loaded from a file or Kubernetes Secret with a
   trailing newline still matches. `TestBearerTokenCaseInsensitiveScheme` and
   `TestBearerTokenToleratesWhitespace` pin both.

## Review fixes round 2 (B1-test + B3, head d28b79a -> new commit)

Dwight's second review (pullrequestreview-5401813375, via god) found two required
issues in the first round.

- **B1-test -- `TestAPITokenNotPrintedInFlagUsage` did not exercise `main.go`**: it
   built its *own* `FlagSet` with a hand-written empty `--api-token` default, so
   reverting the *registration* default to `os.Getenv(...)` left the test green.
   Fixed by factoring the flag registrations out of `run()` into
    `registerFlags(fs *flag.FlagSet) *serverFlags`, called by both `run()` (on
    `flag.CommandLine`) and the test (on a fresh `FlagSet`). The test now calls the
   real registration with `CICADO_API_TOKEN` set, triggers a parse error, and
   asserts the sentinel is absent from `PrintDefaults` -- verified to *fail* when
   the default is reverted to `os.Getenv(...)` and to *pass* with the empty default.
- **B3 -- the N2 trim opened a fail-open hole (security)**: a whitespace-only
    `CICADO_API_TOKEN` (e.g. a Secret mount holding only `\n`) passed
    `apiStartupGuard` (which checks the untrimmed value), armed an authenticator
   whose stored token trims to `""`, and then `"Bearer \u00a0"` / `"bearer
    \u3000"` (credentials that Go's HTTP parser leaves intact because it trims
    only ASCII space and tab) trimmed to `""` and matched via
   `ConstantTimeCompare("", "")==1` -> 200. Fixed at every layer, failing closed:
   - **(a)** `main.go` trims `*apiToken = strings.TrimSpace(...)` before
     `apiStartupGuard`, so a whitespace-only env value is treated as empty and the
     server refuses to start. `TestAPIStartupGuardWhitespaceFailsClosed` pins it
     (`\n`, spaces, `\t`, `\u00a0` all fail closed; a real token passes;
     `--insecure-api` still overrides for dev).
   - **(b)** `WithAPIToken` arms `NewStaticTokenAuthenticator` only when
     `strings.TrimSpace(secret) != ""` -- a whitespace-only secret must never build
     an empty-token authenticator.
   - **(c)** `staticTokenAuthenticator.Identify` rejects an empty *presented*
     credential (`if presented == "" { return err }`).
     `TestStaticTokenAuthenticatorRejectsWhitespaceBearer` pins it (whitespace
     `\u00a0`/`\u3000`/`"Bearer "` all rejected for both a whitespace-armed and a
     properly-armed authenticator); verified to *fail* without the check.

## Why the CLAUDE.md decision table was not touched

CIC-150's ticket text asks to "correct CLAUDE.md to stop claiming `/api/v1` is
authenticated." Per the owner's 2026-09-28 policy, feature PRs do **not** append
to CLAUDE.md's decision table; the authoritative record is this file (and the
CIC-82 known-issue note, which this PR closes). CLAUDE.md is left unchanged on
purpose.

## Consequences

- `cicado-server` now refuses to start without either a token or an explicit
  `--insecure-api`, so the default is secure-by-default (fail closed).
- Local development must pass `--insecure-api CICADO_INSECURE_API=true` or set
  `CICADO_API_TOKEN`; the webhook signature verification path is unaffected.
- The log stream is no longer readable without the operator credential — the
  concrete CIC-82 gap is closed.

## Related

- `docs/decisions/CIC-82-wire-built-subsystems.md` (known-issue note; resolved
  here).
- `docs/decisions/CIC-80-webhook-fail-closed.md` (the `--insecure-webhook` /
  `webhookStartupGuard` pattern this mirrors).
- CIC-44 (GitHub OAuth principals) — the owner of the org-scoping follow-up.
- CIC-159 — the same `os.Getenv`-as-flag-default leak on `--webhook-secret`,
   `--secret-key`, and the GitHub-token flags; a separate PR that applies the
   empty-default + `PrintDefaults` test pattern this PR established for
   `--api-token`.

## Review fixes round 3 (B4 -- "no layer may trust another", head c8d89fc-era)

Dwight's third review (pullrequestreview-5402026288, via god) found that two of
the round-1/2 fixes were correct in shape but left a layer that trusted another.
Both were closed in this commit (fail closed at **every** layer, no layer trusts
another):

- **B4 part 1 -- `WithAPIToken` failed _open_ on an empty/whitespace secret
   (supersedes round-2 B3(b))**: round-2 had `WithAPIToken` arm the static-token
   authenticator *only when* `TrimSpace(secret) != ""`, leaving the
   `noOpAuthenticator` default in place for a whitespace-only secret -- so
   `api.New(r, WithAPIToken("\n"))` answered **200 with no header**. The handler
   layer now **always** arms a `staticTokenAuthenticator` (which trims and rejects
   everything on an empty/whitespace token), so the handler fails *closed* on its
   own terms instead of trusting `apiStartupGuard` to have caught it first.
   `TestAPITokenEmptyLeavesNoOp` (which asserted the old open-on-empty dev
   behavior) is replaced by `TestAPIEmptyOrWhitespaceTokenFailsClosed`
   (`""`, spaces, `\n`, `\t\n`, `\u00a0` all -> 401 on both `/api/v1/runs` and
   the `/api/v1/logs` live stream; a real token still -> 200). Verified to
   *fail* when the guard is reverted.
- **B4 part 2 -- run()'s trim was untested**: `TestAPIStartupGuardWhitespaceFailsClosed`
   trimmed *inside the test*, so a dropped `TrimSpace` in `run()` left every test
   green (Dwight removed it and the suite stayed green). Fixed by extracting
    `resolveAPIToken(flagVal string) string` -- the single source of truth for the
   env fallback + trim -- used by **both** `run()` and the test; the test now
   drives `resolveAPIToken` via `t.Setenv("CICADO_API_TOKEN", ...)` so a regressed
   trim is caught. Verified to *fail* when the trim is removed from
   `resolveAPIToken`.
- **Dev-path preservation**: `main.go` arms `WithAPIToken` only when a real token
   is present (built into an `apiOpts` slice, appended under `if *apiToken != ""`).
   The explicit `--insecure-api` / `CICADO_INSECURE_API` path omits `WithAPIToken`
   entirely, keeping the no-op (open) authenticator for that human opt-in -- while
   a directly-misused `WithAPIToken("")`/`("\n")` still fails closed at the handler
   level. No behavior change for either the production (armed) or the dev (open)
   path; only a *misused* empty token at the handler layer now fails closed.

Per the owner's 2026-09-28 policy, CLAUDE.md's decision table is left
unchanged; this file is the authoritative record.
