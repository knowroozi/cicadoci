## CIC-38 — GetJobSecrets RPC and Postgres secrets store

**Date:** 2026-09-29

**Problem:** `runnersvc.Service.GetJobSecrets` returned an empty
`GetJobSecretsResponse` — the lease check was correct, but no secrets
were ever looked up or returned. The `secrets.Store` interface and
`Memory` implementation existed but had no wire-up.

**Decision:**

1. **`secrets.Postgres`** (`server/secrets/postgres.go`):
    - Implements `secrets.Store` against the `secrets` table (schema
     in migration `0001_init.sql`).
    - AES-256-GCM encryption; stored blob is `nonce‖ciphertext‖tag`
     (12-byte random nonce, GCM appends the 16-byte tag).
    - `Get`, `Delete`, and `List` split into two queries each:
      repo-scoped (`repo_id = $2`) first, then org-scoped
      (`repo_id IS NULL`). Postgres does not support a single
     `WHERE repo_id = $1 OR repo_id IS NULL` that also respects
     scope priority, and the `IS NULL::uuid` cast bug in inline
     `CASE` expressions was confirmed in review.
    - `Put` uses a transactional `SELECT id ... FOR UPDATE` then
     `INSERT` or `UPDATE` pattern. Postgres does not support
     `ON CONFLICT` on the expression index `secrets_scope_name_idx`
     (which uses `coalesce(repo_id, …)`).
    - Repo-scoped secrets take precedence over org-scoped.
    - 5-minute lease TTL; secrets expire and must be re-fetched.
    - KMS envelope encryption (`encrypted_data_key`, `kms_key_id`
     columns) deferred to M9; empty strings used for MVP.

2. **`runnersvc.Service.secrets`** field:
    - `New(pool, q, cfg, log, secretsStore)` — 5th param, nil-safe.
    - `GetJobSecrets` now:
      1. Authenticates the runner and verifies the live lease.
      2. Looks up `job.secret_refs` and `run_id` via a single SQL
       query.
      3. Checks `run.is_fork` — fork runs get no secrets.
      4. Resolves `org_id` from `repos`.
      5. For each secret ref: calls `secrets.Get`, registers the
      value with `cfg.Masker.RegisterWithVariants` (CIC-74
      invariant), stores the plaintext in the response.
      6. Missing secret refs produce a `log.Warn` and are skipped,
      not a hard failure: a job can run with whatever secrets *are*
      configured.

3. **`main.go`** startup guard:
    - `--secret-key` / `CICADO_SECRET_KEY`: hex-encoded 32-byte AES
     key.
    - `--allow-ephemeral-key` / `CICADO_ALLOW_EPHEMERAL_KEY`:
     permits a random ephemeral key when `CICADO_SECRET_KEY` is
     empty. Without this flag and without `--dev-seed`, the server
     **refuses to start** — prevents silent loss of encrypted-at-rest
     data in production.
    - Passes `secrets.NewPostgres(st, key)` to `runnersvc.New`.

4. **Tests** (`server/secrets/postgres_test.go`):
    - `TestPostgres_SecretsLifecycle`: Put → Get → Get(missing) →
     Delete → Get(fails) — verified PASS against a real Postgres
     via `scripts/with-postgres.sh`.
    - `TestPostgres_List`: List returns names, not values.
    - Both skip when `CICADO_TEST_DATABASE_URL` is empty.

**Deferred:**
- KMS envelope encryption (M9): the `encrypted_data_key` and
   `kms_key_id` columns in the `secrets` table are reserved but
   unused; a plaintext `CICADO_SECRET_KEY` is acceptable for an MVP.
- OIDC job-identity tokens (CIC-39): `GetJobSecretsResponse.identity_token`
   is returned as empty. A full OIDC signer with JWKS is a separate
   ticket.