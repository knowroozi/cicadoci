# CIC-79 Decision: Secrets actually reach a job (server `secret_refs` + runner `GetJobSecrets` fetch + `secrets.X` resolution)

**Status:** In Review — PR #91 (CIC-79)
**Opened:** 2026-09-30
**Closed the gap:** the M1+M5 wiring delivered secrets end-to-end *except* the
two missing links — the server never recorded which secrets a job requested, and
the runner never asked for them (`GetJobSecrets` was implemented server-side but
unused, and `executor.Job.Secrets` was a field that was never populated). So no
user workflow could reference `secrets.FOO` and have it substituted. This PR
closes both links.

## The invariant

A secret **value** is never written to the job row, never sent on the wire in a
job assignment, and only delivered to a runner inside the live-lease window via
the explicit `GetJobSecrets` RPC (CIC-38). What a job *requests* is the set of
**names** (`secret_refs`, a `text[]` of `secrets.<NAME>`), which is safe to
persist because it carries no value. This mirrors GitHub Actions: a workflow
names the secret; the value is injected at run time by the server under
lease-scoped auth.

## Part A — server records the requested names (`secretRefsFor`)

`secretRefsFor(j)` in `server/controller/jobs.go` now extracts the distinct,
sorted list of referenced secret names from:
- the job-level `Env` (`Job.Env`), and
- every step's `Env` (`Step.Env`).

The canonical syntax is owned by the new leaf package `internal/secretexpr`:
- `Refs(env)` — extraction (server side, written to `secret_refs`);
- `Resolve(env, secrets)` — substitution (runner side, at step start).

Two forms are accepted:
- the bare form: `DEPLOY_ROLE: secrets.PROD_DEPLOY_ROLE`
- the template form:  `DEPLOY_ROLE: `${{ secrets.PROD_DEPLOY_ROLE }}``

A `secrets.FOO` *embedded* inside a larger string (`prefix secrets.SUFFIX
suffix`) is **not** treated as a reference, because it cannot be
unambiguously rewritten and would otherwise leak a partial reference into the
child process environment. Fork jobs get `secret_refs = {}` from policy,
regardless of the workflow text — the extraction never runs on fork jobs.

`convertJobSpecs` passes `SecretRefs: secretRefsFor(j)` into the
`store.WorkflowInput`/`JobSpec`, and the queue writes it to the `jobs` row.

## Part B/C — runner fetches and resolves (`fetchJobSecrets`, `secretexpr.Resolve`)

In `executeJob` the runner now:
1. `secrets, err := fetchJobSecrets(ctx, a.secrets, jobID, leaseID)` — a single
   lease-scoped `GetJobSecrets` call per job (auth is against the LIVE lease,
   authorized by the job's `secret_refs`),
2. `job.Secrets = secrets`,
3. for each step, `secretexpr.Resolve(mergeEnv(jobEnv, stepEnv), secrets)` —
   substituting `secrets.<NAME>` values with the fetched values before the
   step's command runs.

A nil client (unit tests / pre-connect agents) is treated as "no secrets" —
an empty set, not an error. Only a live RPC failure (transport,
unauthorized) classifies as an infrastructure failure and fails the job.

### Narrow-client seam

`fetchJobSecrets(ctx, client secretsClient, …)` takes a **narrow** one-method
interface so it is unit-testable offline without a live gRPC server. The
`Agent` exposes a `secrets` field of that type, wired from the real
`RunnerServiceClient` only when a live connection exists
(`maybeSecretsClient(conn)`). This keeps the secret-fetch path testable without
a network and decouples the security-critical fetch from the transport.

### Redaction (server side)

The value is registered in the server's redaction masker the moment
`GetJobSecrets` serves it, so any log chunk the runner streams back is masked
before it touches the log body column. The runner never writes the value to
disk or its own log. (See `service.go:58` comment and `redact.go`.)

## What landed / verified

- `internal/secretexpr` — extraction + resolution, fully unit-tested
   (`TestRefs_*`, `TestResolve_*`, `TestValidName`), leaf package, only
   stdlib.
- `runner/secrets.go` — `fetchJobSecrets` + the `secretsClient` narrow
   interface, with a fake-client test suite.
- `runner/agent.go` — `secrets` field + `maybeSecretsClient(conn)` wiring.
- `runner/job.go` — `executeJob` populates `job.Secrets` and resolves step env.
- `server/controller/jobs.go` — `secretRefsFor` extracts refs from job + step env.
- `gofmt -l` clean; `go vet .` clean; `golangci-lint` clean (offline
   packages: controller, internal/secretexpr, runner — `-race`).

## Explicitly out of scope

- **Per-job ephemeral tokens CIC-40** — that's a distinct delivery path
   (`JobToken`); the runner may *exchange* a job token for a cloud credential
   but the token *is not* a secret reference in this sense.
- **Identity / OIDC exchange CIC-39** — cloud-federation token issuance, not
   `secrets.X` resolution.
- **Runner redaction (local)** — server-side masking is sufficient for the
   log body; a runner-local scrubber is an M9 hardening item.
- **Fork-PR policy wiring** — this PR assumes the policy layer (CIC-27/CIC-42)
   already blanks `secret_refs` for fork jobs; this PR does not implement that
   policy.
- **MCP / web UI surfaces** — the secret value is not exposed through any
   HTTP or MCP endpoint as a consequence of this PR.

## Verified / Unverified checklist

- [x] Server records `secret_refs` from job + step env, deduped + sorted
      (TestSecretRefsFor)
- [x] Runner fetches secret VALUES via `GetJobSecrets` and populates
      `job.Secrets` (TestFetchJobSecrets_*)
- [x] Step env is resolved: `secrets.X` → value; plain values pass through
      unchanged (TestResolve_*)
- [x] Embedded `secrets.FRAGMENT` is not treated as a reference
      (TestRefs_EmbeddedFragmentNotMatched)
- [x] Nil-client (test / pre-connect) is a no-secret empty map, not an error
      (TestFetchJobSecrets_NilClient, TestCancelPropagatesToRunningStep)
- [x] Live RPC failure classifies as an infra error (TestFetchJobSecrets_InfraError)
- [x] Secret VALUES are never written to the job row or sent on the wire
      (invariant unchanged: `secret_refs` is a `text[]` of names)
- [x] `gofmt -l` clean, `go vet .` clean, `golangci-lint` clean
- [ ] PG-gated `test/e2e` integration for CIC-79 (full HTTP → runner
      round-trip with a real `secrets.FOO` reference) — deferred to M5 tail
      (CIC-41/42) as a hardening exercise
- [ ] Server-side redaction masker registration is invoked from
      `GetJobSecrets` (invariants hold from pre-CIC-79 code; this PR does not
      add that path, only consumes the existing one)

## Security fixes (Dwight's #91 re-review)

### Blocker 1a — fork policy single-source-of-truth

`server/policy/fork.go` called `looksLikeSecretRef`, which only stripped
values starting exactly with `secrets.`. The new `secretexpr.IsRef` and
`secretexpr.Extract` functions are exported; `ForkPolicy.scrubSecrets` now uses
`secretexpr.IsRef`, so a fork PR can no longer smuggle a secret through the
template form `${{ secrets.NAME }}` or via leading whitespace. Regression test:
`TestForkPolicy_StripsTemplateFormSecret` in `server/policy/fork_test.go`.

### Blocker 1b — defense-in-depth for fork jobs

`secretRefsFor` now takes an `isFork bool` parameter. When `isFork` is true,
the function returns `nil` unconditionally, so even if `ForkPolicy.Apply`
missed a ref the fork job's `secret_refs` column is empty and GetJobSecrets
authorizes nothing. Regression test: `TestSecretRefsFor_ForkJobReturnsNil` in
`server/controller/secretrefs_test.go`.

### Blocker 2 — duplicate-key env across steps

`secretRefsFor` previously merged job-level and all step envs into one map by
key before calling `secretexpr.Refs`, so a key that appeared in two steps with
different secret refs left only the last step's ref in the list. Fixed: the
job-level and each step's env are now evaluated independently, and all refs
are accumulated in a set. Regression test:
`TestSecretRefsFor_MultipleStepsSharedKey` in
`server/controller/secretrefs_test.go`.

### Should-fix 3 — missing secret fails the job

After `secretexpr.Resolve`, the runner checks `unresolvedSecrets`. If any env
value is still a secret reference after resolution, the job is failed with
`ErrorClass: "secret_unavailable"` and a clear message naming the missing
secret, rather than having the step fail on an opaque downstream auth error.
`runner/secrets.go`: `unresolvedSecrets`, `secretexpr.Extract` exported.
Regression test: `TestUnresolvedSecrets_ReturnsMissingNames`.

### Should-fix 4 — stale TODO in `server/runnersvc/logstream.go`

The `TODO(CIC-37)` comment ("masker scrubs against an empty registered-value
set") was updated to reflect that since CIC-79, the masker's registered-value
set is populated when a runner claims a job with non-empty `secret_refs`;
CIC-83 tracks the lifecycle (unregister on completion / lease expiry).

### Nit 5 — security invariants in PR body

Server-side masking on ingest (`server/runnersvc/logstream.go`) is the
authoritative masking point for this MVP. The runner-local mask in the
journal is a separate concern tracked under CIC-83; not blocking.
