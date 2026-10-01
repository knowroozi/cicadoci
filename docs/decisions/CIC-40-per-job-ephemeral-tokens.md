## CIC-40 — Per-job ephemeral platform tokens: run-scoped bearer, hashed at rest

**Date:** 2026-09-30

**Decision:** New `server/jobtoken/` package. Per-job ephemeral platform tokens are
short-lived bearer credentials, scoped to a (org, run, job, branch) tuple plus a
set of API scopes (`read`/`write`). The subsystem is a `Store` (a pluggable
backend for token records; `Memory` for tests and single-process deploys,
`Postgres` reusing the existing `tokens` table for production) wrapped by a
`Manager` that does minting, verification, and revocation. An optional `Auditor`
sink is fired on real state changes (Issue, Revoke) and on each verified Use;
a nil Auditor is rejected by NewManager (fail-closed); use NewMemoryAuditor in tests and NewAuditLog (audit.go) in production.

### Why a bearer, not a JWT

OIDC tokens (CIC-39) cross a trust boundary — the cloud platform verifies a
signed assertion from us and grants cloud access. A job token never crosses a
trust boundary; it stays inside the CICaDo HTTP layer. A 256-bit random with
a fixed prefix (`cicado_job_…`) is unguessable, requires no key material, and
has no signing algorithm to review. We keep one place to think about
authentication — a SHA-256-by-hash lookup — and one place to enforce the
expiry and revocation. The OIDC token handles cross-broker trust; the job
token handles within-broker auth.

### Why hashed-at-rest, show once

The raw bearer is generated in the Manager on `Issue`, returned to the caller
exactly once, and discarded by the caller. The store only keeps the raw's
SHA-256. The invariant — "a job token is not recoverable from the
database" — is enforced by the absence of a `GetRaw` path and by having
adversarial tests verify the headline invariant (`TestStoredRawIsNotRecoverable`).

### Scopes and fork restriction

Two scopes in MVP:

- `read` — the baseline; a fork-PR job gets this only.
- `write` — a non-fork job gets this and must push status or list artifacts;
  it is never granted to a fork-PR job.

The fork-policy decision is enforced at the **mint site**, not inside the
package. `policy/fork.go` (M4) is the caller that decides which scopes get
passed to `Manager.Issue`. The package is agnostic to the policy: it reasons
only about the passed scopes, which is a clean separation of concerns and
keeps the security boundary in one place (wherever a token is minted, that
is the site of the fork check).

### Why TTL is enforced at the Manager, not at the Store

"ephemeral" is the load-bearing property of a job token. A TTL ≤ 0 is a
caller bug — it means "no expiry", which defeats the invariant — so the
Manager rejects it with `ErrNonPositiveTTL`. This is the one invariant
that cannot live at the store: the store has no notion of "this token is
illegal because its TTL was non-positive". The store is a dumb bucket; the
Manager is the policy layer.

### Why `MarkRevoked returns (changed bool, err error)`

A no-op re-revoke is not a state change, so the `Auditor` must NOT fire for
it — otherwise we get noise in the audit trail (every cancel request that
finds the run already canceled would record a revocation). The Store's
MarkRevoked distinguishes "transition happened" from "already revoked"
so the Manager can fire the auditor only on a real transition. The
store's implementation is a conditional UPDATE (`WHERE id = $1 AND
revoked_at IS NULL`) in Postgres and a guarded map update in Memory;
the `RowsAffected() > 0` / "map entry changed" signal is the
"changed=true" signal.

### Why no `max_uses` on a job token

The `tokens` table has a `max_uses` column with a `CHECK (max_uses > 0)`
and a default of 1. Job tokens are not consumed on use (Verify does not
increment `uses`): the same token is used multiple times as a job makes
multiple API calls. Writing 0 would violate the CHECK; writing 1 would
mislead a future reviewer into thinking the token is single-use. The
column is left at its default; it is irrelevant for job tokens and is
used only for registration tokens (which may be consumed on enrollment).

### Why no `last_used_at` on the Memory store

`last_used_at` is a Postgres column nicety (it tells the admin which
tokens are still live). It is not security load-bearing — a leak of
the audit trail already shows recent uses. `Memory.SetLastUse` is a
no-op; `Postgres.SetLastUse` does a lightweight `UPDATE`. The API
boundary never looks at `last_used_at`, so the abstraction is fine.

### Why a `Bearer ` prefix is stripped in `Verify`

Real callers (a shell in a job that does `curl -H "Authorization: Bearer $TOKEN"`)
pass the full header value. Stripping is a convenience so the
boundary layer doesn't have to. The strip is a no-op on any input
shorter than the prefix or with a different case (`bearer ` is also
stripped; anything else is not).

### Invariants verified by test

- `TestIssueThenVerifyRoundTrips` — mint and verify round-trip on all
  claims.
- `TestVerifyRejectsJunkAsMalformed` — non-job tokens and malformed raw
  are rejected at the auth layer via `auth.Validate`.
- `TestVerifyUnknownTokenIsNotFound` — a well-formed but unissued token
  is rejected as `ErrNotFound` (a hash in no store).
- `TestVerifyExpiredToken` — an advanced clock past the token's expiry
  yields `ErrExpired`.
- `TestVerifyAfterRevoke` — a revoked token yields `ErrRevoked`; a
  second revoke is a no-op (`MarkRevoked` returns `(false, nil)`)
  so the auditor gets exactly one event.
- `TestVerifyEnforcesScope` — a `read`-only token is refused `write`.
- `TestIssueRejectsNonPositiveTTL` — a non-positive TTL is rejected at
  the Manager.
- `TestIssueRejectsIncompleteClaims` — a claim record missing run/job
  ids or with zero scopes is rejected.
- `TestStoredRawIsNotRecoverable` — the store holds only one record
  (its hash), not the raw.
- `TestBearerPrefixIsStripped` — `Bearer ` and `bearer ` are
  transparently stripped.
- `TestPostgres_IssueVerifyRevoke` — the same on a real Postgres run
  with seeded org/repo/run/job rows, plus a per-record revocation
  (revoking one token on a job does not revoke a second one minted
  after on the same job).
- `TestPostgres_ClaimsRoundTrip` — the `branch` and `issuer` claims
  survive the round trip through Postgres.

### Why `jobtoken` is a leaf

`jobtoken` depends on `auth` (for the bearer generate/validate and the
hash), `uuid`, and `pgx` (only in `postgres.go`). No controller, no
workflow, no policy. That's the correct separation: a job token is an
authentication object, orthogonal to the scheduling decision, the
runner protocol, and the policy layer.

## Review hardening (2026-09-30, Dwight's PR #87 review)

Three blockers and one non-blocking issue were folded into the package. All
are fail-closed fixes:

- **Auditor is now required.** `NewManager` / `NewManagerWithClock` return an
  error for a nil `Auditor`. CLAUDE.md names "every state-changing action
  writes an audit event" as a security invariant; minting and revoking a
  credential are both state changes, and a silently-nil auditor was the
  worst-case violation. Test-only MemoryAuditor is passed for the no-sink
  case.
- **Audit failures are now logged at Warn, not discarded.** All three audit
  calls (Issue/Use/Revoke) capture the error and call
  `log.Warn(..., "token_id", id, "err", err)`. The Manager carries a
  `log *slog.Logger` defaulting to `slog.Default()`.
- **Empty scope fails closed.** `Verify(ctx, raw, "")` now returns
  `ErrForbidden` instead of silently passing authorization — a caller that
  omits the scope argument or passes a zero-value `Scope` can no longer
  skip the check.
- **RepoID is required in `Claims.Validate()`.** An unbound repo (the
  all-zero UUID) would be inserted into Postgres as
  `00000000-0000-0000-0000-000000000000`, which the
  `repo_id REFERENCES repos(id)` foreign key rejects. Now rejected at the
  validation boundary before Mint.
- **Non-blocking: store errors wrap rather than collapse.** `errStore`
  now wraps non-`ErrStoreNotFound` errors with `fmt.Errorf("jobtoken:
  store: %w", err)` so logs and metrics can distinguish a DB outage from
  a bad token.
