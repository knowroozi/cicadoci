# CIC-39 — OIDC Token Issuer

**Status:** Implemented, in PR.
**Date:** 2026-09-30.
**Decision-maker:** owner-approved scope; engineering by agent jim-muo6c940.

## Context

The v1 workflow schema carries an optional per-job `identity` field (CIC-19,
`server/workflow/types.go`). `identity: aws` means "this job should assume a
short-lived AWS role via `sts:AssumeRoleWithWebIdentity`". CIC-38 already shipped
the secret-delivery half (`GetJobSecrets` over the lease, `Server.GetJobSecrets` on
M3's `CIC-38` branch). CIC-39 ships the cloud-identity half: a server that mints
short-lived, self-contained job credentials that a runner presents to a cloud
provider, which exchanges it for a scoped, time-limited role.

This PR is the package only: no server wiring, no runner change, no schema change,
no HTTP endpoint. It is a leaf the runner-wiring ticket builds on.

That is worth repeating, because it is the load-bearing part. The package is **not
plugged into anything yet**. A token it mints is unverified, undelivered, and
unexercised on a real job. The integration test is a self-contained mint-then-
verify round-trip within `package oidc_test`, not a runner→cloud path. That
asymmetry is intentional and it is why the PR body and the CI gate (CIC-88) both
state it plainly: this PR earns its keep by being *correct and reviewable*, not
by proving the end-to-end story.

## Decision

Introduce `server/oidc` as a self-contained, dependency-free (stdlib plus the
existing `github.com/google/uuid`) package.

**Mints a JWT**, not a bare token. A JWT is the right shape because:

1. **It is self-contained.** The runner needs a single compact string that
   encodes "who am I and what is this for". No secondary lookup, no DB call, no
   round-trip to the server.
2. **It is short-lived and verifiable.** It carries an `exp` and a `jti`;
   the verifier needs only the public key, published once via `KeySet.Marshal`
   as a JWKS document.
3. **It matches the cloud federation contract.** AWS, GCP, and Azure all
   speak the same dialect for this purpose. Choosing a dialect we already
   know lets the runner's federation code stay small.

**Algorithm is ES256, NIST P-256.** RS256 is fine too, but a 256-bit EC key is
a smaller artifact to ship in a PEM and a smaller signature to sign. The
`Signer` interface abstracts the choice, so adding RS256 or EdDSA later does
not change the wire format.

**Subject is a tuple, not a free-form string.** `job:<run>:<job>` is
enforced by `SubjectFor`, not by convention. This matters because a
mis-formatted subject would let one job's token impersonate another's; the
grammar is validated at construction, not at use.

## Security rationale

Every decision here was made with a specific attack in mind.

- **`alg: none` is closed out by construction.** The token header is encoded
  by the package and the algorithm is hard-wired to ES256. There is no code
  path that produces `alg:none`. This is the most-cited JWT attack in the
  wild, and refusing to mint it is the only correct fix.

- **`kid` mismatch is a hard error.** The verifier rejects a token whose
  `kid` is not in the published key set. A key rotated off the set fails to
  verify, which is the intended behaviour when a key is decommissioned.

- **Issuer and audience are both checked.** Issuer binds the token to this
  server; if an attacker mints a token on a different issuer, we do not
  accept it. Audience binds the token to the cloud it was meant for; a token
  minted for AWS is not accepted by a GCP verifier, even if they shared the
  same key.

- **Expiry is checked on the verifier side.** A token that has expired fails
  to verify. This is the property that makes "short-lived" actually mean
  anything: a leaked token stops working the moment its `exp` passes without
  any revocation step.

- **The JWK is the on-curve gate.** A JWK that names a curve other than
  P-256 is rejected at parse time. An on-curve check runs on every
  reconstruct to close forgery via a half-formed point. `ecdsa`'s
  `IsOnCurve` is deprecated in Go 1.26 in favour of `crypto/ecdh`, so the
  reconstruct path uses `ecdh.P256().NewPublicKey`, which validates on-curve
  and canonical in one call. That is the only `//nolint:staticcheck` in the
  package: the field read to hand a coordinate-borne key to
  `ecdsa.Verify` is the one place a coordinate write is unavoidable, and a
  dependency-free leaf does not pull a third-party curve library for a
  single call. Documented inline and in `issuer.go` / `keyset.go` with a
  cross-reference to this decision doc.

- **jti is UUIDv4.** A fresh random jti per token means two tokens for the
  same subject can never collide. This matters when a job is re-issued and
  an old token has not yet expired: a revocation list keyed on jti can
  identify the specific token to skip without revoking every subsequent run.
  (The revocation list itself is out of scope for MVP; the data structure
  is the seam.)

## What is out of scope

These are explicit so the scope boundary is a decision and not an oversight.

- **Runner-side federation.** A job's runner receives this token, sends it
  to `sts:AssumeRoleWithWebIdentity`, and receives a scoped role. That
  code lives in a runner PR, built on this package.

- **Server wiring.** A `Server.JobIdentity` method is the natural
  caller-site (mirroring `Server.GetJobSecrets`, also M5). Not in this PR
  because it would touch the M3 `cic-38` branch and the `Job` struct.

- **Schema wiring.** The schema already has a `job.identity` field. This PR
  does not add schema validation around it, because that would touch
  `server/vschema`, and the schema's existing validation is the boundary.

- **HTTP endpoint.** `GET /jwks.json` would be a small addition; not in
  this PR because it would touch `server/httpapi`, and the JWKS marshaling
  seam is available to it (`KeySet.Marshal`) when the endpoint is built.

- **Per-job token rotation / revocation list.** The jti space is
  UUIDv4, and the verifier accepts a clock seam for revocation. The
  revocation policy itself is a CIC-40 question.

- **GCP / Azure federation.** AWS is the MVP. The `Signer` interface
  exists so a future GCP/Azure federation can plug in without a token-
  format migration.

- **AWS Terraform provider.** AWS IAM OIDC provider and role trust
  condition configuration. The CICaDo server mints the JWT; the
  Terraform lives in `infra/aws/` and is a separate ticket.

- **`/.well-known/openid-configuration` discovery document.** AWS's
  IAM OIDC provider expects this endpoint alongside `/jwks.json`.
  Not in this PR because it would touch `server/httpapi`; the
  discovery document is a trivial addition when the HTTP endpoint
  is built.

## Consequences

### Positive

- A clean seam between "who is this job?" and "what can it do?" that a
  future runner PR can build on. The token is the unit of cross-boundary
  trust, and it is small and self-contained.
- A `KeySet` that a future `GET /jwks.json` endpoint can serve verbatim.
  No translation layer, no marshaling.
- The test suite exercises the full round-trip including tamper detection,
  kid mismatch, issuer/audience mismatch, expiry, and JWKS round-trip. This
  is a security primitive, and the test suite is the spec for it.

### Negative / costs

- **The `signer.Sign` API is a seam, not a convenience.** Adding a
  second algorithm is a one-line addition to `Signer`, plus a verify
  branch. This is the cost of future-proofing, and it is worth paying
  because the alternative (a hardcoded algorithm in a security primitive)
  is a worse sin.

## Alternatives considered

- **Mint a bearer token for a cloud federation call** — rejected: a
  JWT's self-contained, signed structure is the property a federation
  provider requires of its token. A bare token would need a secondary
  lookup at the cloud side, which is exactly what the federation contract
  is designed to avoid.
- **Use GCP's or Azure's specific token format** — rejected: we only
  integrate AWS in MVP. The token format chosen is AWS-compatible, which
  is a superset of what GCP and Azure accept for a JWT-shaped input.
- **Skip jti** — rejected: a fresh jti per mint is a small cost that
  buys a revocation seam that is cheap to fill in later and expensive to
  retrofit.
- **Hardcode the algorithm** — rejected: the `Signer` interface is a
  trivial cost that buys a clean path to adding RS256 or EdDSA without a
  token-format migration. Worth it for a security primitive.

## Verification

Full verification is in the PR body's Verified / Unverified checklist.
The key items this decision doc pins:

- `server/oidc` tests pass `-race`.
- `golangci-lint` on `server/oidc/...` is zero.
- No new dependency is added to `go.mod`.
- The JWKS shape matches RFC 7517, and the keyset round-trips through
  marshal/unmarshal and an off-curve key is rejected.
- `docs/decisions/CIC-39-oidc-issuer.md` (this document) is in the PR.

## Review hardening (2026-09-30, Dwight's PR #86 review)

Five blockers and two nits from the security review were folded into the
package. None change the token format or the leaf-package surface; each
closes a fail-open path that a future caller could hit:

- **Per-token `iss` override removed.** `Claims.Issuer` is gone; `Issue`
  always stamps the issuer's configured `is.issuer`. A bug or a
  compromised caller can no longer mint a token attributed to a different
  issuer that shares the key, which is the exact knob that defeats the
  "issuer binds the token to this server" rationale.
- **Empty audience is fail-closed at both ends.** `Issue` returns
  `ErrEmptyAudience` for a blank `aud`; `Verify` rejects a token whose
  `aud` is empty. A verifier that forgets `ExpectedAud` can no longer
  accept an audience-less token.
- **Subjects cannot collide.** `SubjectFor` rejects a `:` in either the
  run or job id (both are UUIDs, colon-free), so
  `SubjectFor("a:b","c")` and `SubjectFor("a","b:c")` can no longer both
  yield `job:a:b:c`. `parseSubject` enforces the same invariant on the
  verify side and its doc comment now states it correctly.
- **`KeyFromPEM` enforces P-256 at the load boundary.** A P-384/P-521
  PEM, which would make `ecdsaSigner.Sign`'s fixed-width `FillBytes`
  panic or truncate, is now rejected at load rather than left to a
  downstream mint-time failure.
- **Empty kid is enforced at mint.** `NewIssuer` refuses a key with an
  empty id; a kid-less key mints tokens our own `decode` rejects as
  malformed.
- **Nits:** a token with no `exp` claim is now a hard `ErrExpiredToken`
  (AWS rejects such tokens); the unreachable `ErrNoKID` branch and
  sentinel were deleted (`decode` already rejects an empty kid as
  `ErrMalformedToken`); and `NewVerifier` mirrors `NewIssuer` by refusing
  an empty issuer so it cannot disable the issuer check.

- **`NewVerifier` returns `(*Verifier, error)`** (round-3, 2026-10-01).
    `NewVerifier("")` returns an error rather than a nil pointer that
    would panic on the next `Verify` call. Callers must update to the
    new two-value signature; `newVerifier` in `oidc_test.go` does so.
