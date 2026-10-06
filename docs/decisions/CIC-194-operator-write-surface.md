# CIC-194 — Operator write surface: secret PUT/DELETE + job cancel, audited

**Status:** In Review (PR pending).
**Ticket:** CIC-194 (High, in-progress, God-filed 2026-10-05T22:10Z).
**Branch:** `knowroozi/cic-194-secrets-cancel-endpoints-clic`, based on `origin/master` (`6debce1`).

## The problem

`secrets.Postgres.Put`/`Delete` and `queue.Postgres.RequestCancel` had **no
production caller**. The platform therefore had **no way to set or delete a
secret** and **no way for an operator to take an in-flight job off the runner**
from outside the gRPC control plane. This was a High-severity operational gap.

## Decisions

### D1 — One write surface, through the existing per-surface audit seam
The new routes register on the same `api.Handler` that every other `/api/v1`
route uses (`handler.go` `buildMux`), so they share `buildChain`'s composed
authenticate + per-request-timeout layers. "Every state-changing action writes
an audit event" (CLAUDE.md) therefore holds *without a new audit path*: each
write invokes a separate handler-level seam, `Handler.auditWrite`, that logs but
does not fail the request on an append error. `buildChain` is strictly
authenticate + per-request-timeout — **audit is not a chain layer** — it is the
`auditWrite` seam each write handler calls, which is what makes the audit row
land even on a 400 (C1). This mirrors CIC-192's "never drop an audit on a failed
append" posture.

### D2 — Fail-closed by absence
`WriteDeps` gates each capability on its own nil check: a server with no
`CICADO_SECRET_KEY` leaves `SecretStore` nil and registers **no** secrets route
at all (not a broken route that 500s on every request); a `nil` `Cancel`
registers **no** job-cancel route. The `apiToken` gate is the same idea one level
up: in `main.go`, `WithWriteDeps` is armed *inside* the `if *apiToken != ""`
block, so under `--insecure-api` the write routes are absent — read-only mode is
read-only. This is stronger than a runtime 500: the route that cannot exist
cannot exist. On a dependency that fails *after* wiring (a store call erroring
at runtime), the handler returns `500` with a generic message, not `509`.
A storage-pressure → 509 mapping (`queue.EnrichCancelErr`, the backpressure
layer) is a shared `errors.Is` helper, out of scope here.

### D3 — The secret value is consumed and never redeliverable
`handlePutSecret` decrypts nothing and returns no secret value; the value is
written to the store (which encrypts at rest, CIC-37) and is absent from the
audit entry by construction — the audit `Target` names only
`org/repo/name`, and a test asserts the plaintext never reaches the audit log.

### D4 — `operatorActor` is the audit actor for the CLI/HTTP write surface
M1 has no per-principal model (that is CIC-44), so the operator is recorded as a
named `api-operator` actor rather than `system`. CIC-192 made the audit
principal-faithful; a write that attributed to `system` would be indistinguishable
from a background write, defeating the very fidelity CIC-192 established.

### D5 — Scope: job-level cancel, not run-level
A run cancel would require a jobs-by-run listing, which does not yet exist in the
queue store (`RequestCancel` takes a single `jobID`). This PR delivers the
fully-supported job-level cancel + the complete secrets surface; a run-level
aggregate cancel + rerun is the follow-up **CIC-206** (Dwight confirmed
run-cancel is CIC-206, 2026-10-06). It is noted on the ticket and not claimed
here.

### D6 — CLI: `flag`-package ordering
`flag` stops parsing at the first non-flag argument, so `cicado run cancel`
requires flags to precede the job id (`cicado run cancel --reason R <jobID>`),
documented in the usage string rather than re-implementing an arg parser.

## Files
- `server/api/writes.go` — four write handlers (`handlePutSecret`, `handleListSecrets`,
  `handleDeleteSecret`, `handleCancelJob`) + `WriteDeps` + `auditWrite`; E1 `uuid.Parse`
  client-fault 400 + generic store-error 500; C1 false-cancel 404 with no audit row.
- `server/api/handler.go` — `writes` field on `Handler`/`options`; route registration incl.
  the new `GET /api/v1/secrets` list route; comment fix (auth+timeout chain, not audit).
- `cmd/cicado/ops.go` — `secrets set|delete` + `run cancel` thin HTTP clients; J1 `json.Marshal`
  bodies, `--server` required, `--value-stdin` error surfaced + trailing-`\n` trim.
- `server/cmd/cicado-server/main.go` — S1: `WithWriteDeps` + `WithAPIToken` armed *inside*
  `if *apiToken != ""`, so the write surface is absent under `--insecure-api`.
- `server/audit/postgres.go` — CIC-192 r3: `PrincipalType` + `Surface` column constants.
- Tests: `server/api/writes_test.go` (handler surface incl. E1/C1/T2),
  `cmd/cicado/ops_test.go` (CLI routing incl. J1), `test/e2e/skeleton_test.go`
  (`TestOperatorWriteSurfaceE2E`, T1; `TestSecretDelivery` now PUTs via the API route).

## Out of scope
- Run-level aggregate cancel (no jobs-by-run listing yet).
- Real per-principal audit actor resolution (CIC-44).
- A secret **read/list** operator route — by design; the write surface is
  write-only, per the secret redeliverability invariant.
