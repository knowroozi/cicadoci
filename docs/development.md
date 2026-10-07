# Development

Requires **Go 1.26+**. Docker is needed for `make dev`, but not for the tests.

```sh
make dev-deps   # install pinned tooling into ./bin
make check      # lint, vet, and the full test suite with -race
```

## The development stack

```sh
make dev        # server + Postgres + one runner
make dev-down   # tear it down, including volumes
```

The runner persists its enrollment credential in the `runner-state` volume so
it survives a restart. `make dev-down` removes that volume along with
Postgres's, so the common cycle is safe. Resetting only the database — a plain
`docker compose down` (no `-v`) followed by dropping and recreating it, or
`docker compose down -v postgres` — is not: the runner comes back up with a
credential the fresh database has never seen, fails to enroll, and the demo
job never runs. `make dev-reset-runner` clears just the runner's state for
exactly that recovery, without also discarding the database.

The server seeds a development org, a well-known registration token, and a demo
job on startup, so the runner enrolls itself and you can watch `echo hello`
travel the whole path with nothing to bootstrap by hand. Within a few seconds of
`make dev` the logs show assignment, execution, log chunks, and a terminal
status.

- HTTP API: <http://localhost:8080> (`/healthz`, `/readyz`)
- gRPC runner service: `localhost:8081`
- Postgres: `localhost:5432`, user/password/database all `cicado`

**None of it is production-shaped.** The database password is in
`docker-compose.yml`, the runner connects without TLS, the runner container runs
as root so the shell executor can write its workspace, and the registration
token is a constant published in this repository. `-dev-seed` exists for this
stack and nothing else.

## Running without Docker

Every test path works against a plain Postgres, and the binaries run fine
outside containers:

```sh
# One-shot: start a throwaway cluster, run the suite against it, tear it down.
./scripts/with-postgres.sh go test ./... -race

# Or point at a database you already have.
export CICADO_TEST_DATABASE_URL='postgres://user:pass@localhost:5432/cicado_test?sslmode=disable'
make test-all
```

To drive the stack by hand:

```sh
make build

./bin/cicado-server \
    -database-url "$CICADO_TEST_DATABASE_URL" \
    -dev-seed -grpc-addr 127.0.0.1:8081 -http-addr 127.0.0.1:8080

./bin/cicado-runner \
    -server 127.0.0.1:8081 -insecure \
    -registration-token cicado_reg_Y2ljYWRvLWRldmVsb3BtZW50LXRva2VuLURPLU5PVCE \
    -state /tmp/cicado/runner.json -workspace /tmp/cicado/workspaces
```

The runner refuses to start without `-insecure` until TLS transport credentials
land (CIC-13). That is deliberate: the protocol is gRPC over TLS with no
plaintext fallback, and a misconfigured production runner should fail loudly
rather than stream job output and credentials in the clear.

## Required server configuration (fail-closed)

As of the fail-closed hardening (re: CIC-150 / PR #116; re: CIC-159 / PR #119), a
production `cicado-server` **refuses to start** unless each secret below is supplied via its
environment variable. The matching flags are **development-only opt-outs** with **empty
defaults**: since re: CIC-159 every secret/token value is read from the environment
(`envSecret`, after `flag.Parse()`) so it never leaks into the flag usage text or the
process listing, so the flags themselves carry no value and you should not set them in
production.

| Concern | Env var (required in production) | Dev-only opt-out | Behavior when unset & opt-out not passed |
|---|---|---|---|
| Webhook signature verification | `CICADO_WEBHOOK_SECRET` | `--insecure-webhook` (env `CICADO_INSECURE_WEBHOOK`) | refuses startup — see [Webhook signing](#webhook-signing) |
| API bearer authentication | `CICADO_API_TOKEN` | `--insecure-api` (env `CICADO_INSECURE_API`) | refuses startup |
| Secrets-store encryption | `CICADO_SECRET_KEY` (64 hex chars = 32-byte AES-256 key) | `--allow-ephemeral-key` (env `CICADO_ALLOW_EPHEMERAL_KEY`) | refuses startup unless you accept a non-persistent random key |

Two consequences for operators:

- **API auth now requires a Bearer token.** Every `/api/v1/*` call — *including the
  live-logs SSE stream* (re: CIC-82) — must carry `Authorization: Bearer <token>`. Only
  `/healthz`, `/readyz`, and the webhook delivery route stay exempt. An empty/absent token
  is fail-open **only** when `--insecure-api` is set explicitly; otherwise the server
  refuses to start.
- **Read the three secrets from the environment in production.** Set `CICADO_WEBHOOK_SECRET`,
  `CICADO_API_TOKEN`, and `CICADO_SECRET_KEY`; pass `--insecure-webhook`, `--insecure-api`,
  or `--allow-ephemeral-key` (or the `CICADO_INSECURE_*` / `CICADO_ALLOW_EPHEMERAL_KEY`
  env vars) **only** in local `make dev`, where the signing secrets are not configured.
  `-dev-seed` additionally permits the ephemeral key, so dev-mode secrets do **not** survive
  a restart; set `CICADO_SECRET_KEY` for a persistent key.

## Webhook signing

A production `cicado-server` always reads `CICADO_WEBHOOK_SECRET` and verifies
`X-Hub-Signature-256` on every delivery. An empty secret refuses startup
(unless `--insecure-webhook` is passed), so a production server can never
silently skip signature checks. The flag is only for local `make dev` or a
dev box where the webhook is reachable from GitHub but the signing secret has
not been set.

```sh
# With a real secret (recommended even for local dev):
export CICADO_WEBHOOK_SECRET=cicado_whsec_$(openssl rand -hex 32)
./bin/cicado-server --webhook-secret "$CICADO_WEBHOOK_SECRET" ...

# Without a secret (DEV ONLY: all deliveries accepted unverified):
./bin/cicado-server --insecure-webhook ...
```


## App config

The GitHub App integration reads its configuration from flags and, for every
secret/token value, from the environment *after* `flag.Parse()` (so a value
never appears in the flag usage text or the process listing). Each secret has a
`CICADO_*` env var and, where it is a development opt-out, an empty default.

### GitHub App, clone, and workflow tokens (CIC-148 / CIC-151 / CIC-161 / CIC-146)

- `--github-app-token` (env `CICADO_GITHUB_APP_TOKEN`) — used **only for
  Checks-API reporting**. With it set, `newForge(...)` (in `cicado-server`'s
  `main.go`) picks the GitHub Checks forge; empty reports nothing (`NoOpForge`
  plus a warning). It is **not** used to mint clone credentials. A GitHub App
  installation token expires after 1 hour, so a static one suits a short-lived
  dev server only; per-call minting is CIC-146.
- `--github-app-id` (env `CICADO_GITHUB_APP_ID`) + `--github-app-key-file` — to
  enable **per-job clone tokens** the two must be set *together*. The App ID
  (`0` disables minting) plus the PEM private key file (never logged or
  persisted) drive an App-JWT + installation-token minter
  (`runnersvc.NewCloneMinterFunc`, wired as `svcCfg.CloneMinter`); the minted
  `github_clone_token` is delivered to a job over `GetJobSecrets` and consumed as
  `git -c credential.helper`. **Without both**, `github_clone_token` is looked up
  as a static secret instead. Fork PR runs get the same read-only clone token and
  no other secrets.
- `--github-workflow-token` (env `CICADO_GITHUB_WORKFLOW_TOKEN`) — a long-lived
  fine-grained PAT (Contents: read) used to fetch the repo's workflow files on a
  default-branch push. Optional for dev: empty keeps the producer off and the
  server falls back to the store / `-dev-seed` path.

None of the three GitHub App/workflow variables refuses startup when empty — an
empty App token falls back to `NoOpForge` (with a warning), an empty workflow
token keeps the producer off, and an App ID of `0` disables per-job minting.
The variables that *do* refuse startup when unset, and their dev opt-outs, are
documented just above under
[Required server configuration (fail-closed)](#required-server-configuration-fail-closed):
a missing `CICADO_API_TOKEN` (opt-out `--insecure-api`), an empty
`CICADO_WEBHOOK_SECRET` (opt-out `--insecure-webhook`), and a missing
`CICADO_SECRET_KEY` (opt-out `--allow-ephemeral-key`).

### Approval gates (CIC-162 / CIC-182 / CIC-160, PRs #135 / #138 / #129)

- `--approvalgates-enabled` (env `CICADO_APPROVALGATES_ENABLED`, default `false`)
  — build the approval-gate manager and open a gate for every PR run
  (CIC-162 part 1). Gates do not yet block dispatch through a *real* chat
  provider, so it **requires** `--insecure-approvalgates-noop`.
- `--approvalgates-allowed` (env `CICADO_APPROVALGATES_ALLOWED`, default empty)
  — the allow-list of user IDs permitted to resolve a gate.
- `--insecure-approvalgates-noop` (env `CICADO_INSECURE_APPROVALGATES_NOOP`,
  default `false`) — permit `--approvalgates-enabled` with the no-op chat
  provider: gates post nothing and can reach a terminal state only by auto-timeout
  or run-cancel (CIC-208), not by a human. **Development only.**
- `--approval-channel` (env `CICADO_APPROVAL_CHANNEL`, default empty) — the
  channel for gate messages; empty means **no gate is opened** (fail-closed).
- `--approval-timeout` (env `CICADO_APPROVAL_TIMEOUT`, default `0` = no
  auto-timeout) — per-gate auto-timeout; `CICADO_APPROVAL_TIMEOUT` overrides the
  flag value.

## Operator API & CLI (CIC-194 / CIC-206, PRs #143 / #145)

Before CIC-194 the platform could **deliver** secrets to a job but an operator
could **not set** them, and a cancel request had **no trigger**. CIC-194
(PR #143) added the operator write surface and CIC-206 (PR #145) added
run-level cancel. An operator today can set/delete secrets and cancel a run or a
single job.

### HTTP routes

Every route is mounted on the **same mux** as the rest of `/api/v1`, so it shares
the same auth + per-request-timeout chain: each write requires the bearer token in
**`CICADO_API_TOKEN`** (compared in constant time; an absent or wrong token is a
`401`). The operator is audited as a single clearly-named principal, **`api-operator`** —
never `system` or a zero UUID, because CIC-192 made the audit record
principal-faithful (a write attributed to `system` would be indistinguishable from a
background one). A real per-principal model is the CIC-44 / CIC-192 follow-up.

| Route | Action | Notes |
|---|---|---|
| `POST /api/v1/secrets` | Set a secret | Body `{org_id, repo_id?, name, value}`. The **value is stored and never echoed or logged** — only org/repo/name are recorded. |
| `DELETE /api/v1/secrets?org_id=&name=&repo_id=` | Delete a secret | Idempotent. |
| `GET /api/v1/secrets?org_id=&repo_id=` | List secret **names** | Values are **never** returned. |
| `POST /api/v1/jobs/{id}/cancel` | Cancel one job | Body `{reason?}`. |
| `POST /api/v1/runs/{id}/cancel` | Cancel every active job of a run | Body `{reason?}` (CIC-206). |

- **No-op cancel = 404, no audit row.** An unknown id, or a job/run that is already
  terminal, returns `404` and writes **no** `ActionCancel` audit row — a false "cancelled"
  record is never produced. Only a real in-flight state change is audited.
- **Fail-closed by absence.** Each capability is registered **only when its dependency
   is present**: a server built without a `CICADO_SECRET_KEY` exposes no secrets route,
   and a server without a wired cancel path exposes no cancel route.

### CLI counterpart

`cmd/cicado/ops.go` ships thin HTTP clients (the exact inverse of the routes) under the
`cicado` binary. Flag values default to env vars (`CICADO_SERVER`, `CICADO_API_TOKEN`);
`--reason` defaults to *"cancelled by operator via CLI"*:

- `cicado secrets set --org <org> [--repo <repo>] --name <n> (--value <v> | --value-stdin)` → `POST /api/v1/secrets`
- `cicado secrets delete --org <org> [--repo <repo>] --name <n>` → `DELETE /api/v1/secrets`
- `cicado run cancel --reason <r> <jobID>` → `POST /api/v1/jobs/{id}/cancel`
- `cicado run cancel-run --reason <r> <runID>` → `POST /api/v1/runs/{id}/cancel`

`--value-stdin` keeps the secret off the process listing (argv); `--reason` is also
available on both cancel subcommands.

### Security note (fail-closed default; see CIC-150)

The write routes require the **same** `CICADO_API_TOKEN` as every other `/api/v1` route,
so they are unreachable **when a token is set**. With *no* `CICADO_API_TOKEN`, the API
falls back to `noOpAuthenticator`, which **admits every `/api/v1` request — including the
write routes** — and that start-up path is **refused unless `--insecure-api` is also
passed**. So: always run the server with a real `CICADO_API_TOKEN`; `--insecure-api` with an
empty token is a **development-only** opt-out that exposes the write surface to anyone who
can reach the port.

## Tests

| Command | What it runs |
|---|---|
| `make test` | Unit tests only. Hermetic, no database, fast. |
| `make test-all` | Everything, with `-race`. Needs `CICADO_TEST_DATABASE_URL`. |
| `make test-e2e` | The walking skeleton, against a throwaway Postgres. |
| `make check` | What CI runs: generated-code check, lint, vet, full suite. |

Database tests **skip** rather than fail when `CICADO_TEST_DATABASE_URL` is
unset, so `go test ./...` is always safe to run. If you expected coverage and
got skips, that variable is why.

Each database test gets its own database, cloned from a migrated template with
`CREATE DATABASE ... TEMPLATE`. Tests are therefore genuinely independent, which
matters most for the queue tests: those are specifically about what concurrent
transactions do to each other, and a neighbouring test's rows would be
indistinguishable from a bug.

## Changing the protocol

`proto/cicado/runner/v1/runner.proto` is **frozen** (ADR-004). Additive changes
only: new fields with new tags, `reserved` on removals, behavior changes via
`Capabilities.features` flags rather than version sniffing.

```sh
make proto   # regenerate, lint, and breaking-change check
```

Generated code is committed. `make generate-check` fails CI if what is committed
does not match what the proto produces, so the two cannot drift.

## Database changes

Migrations live in `server/store/migrations/`, are applied with goose, and are
**embedded in the server binary** (ADR-008) — so they always travel with the
code that expects them. Adding one means adding a file and rebuilding.

```sh
./bin/cicado-server -database-url "$DATABASE_URL" -migrate
```

Forward-only in production. `Down` exists for local work; rolling back a live
schema means writing a new forward migration.

## Layout

| Path | Contents |
|---|---|
| `server/` | Control plane: gRPC runner service, queue, store, dispatcher |
| `runner/` | Runner agent and executors |
| `proto/` | Frozen protocol definition and generated Go |
| `internal/testdb/` | Per-test database harness |
| `test/e2e/` | The walking skeleton |
| `project-docs/` | Plan, architecture, ADRs — the source of truth |

## Before opening a PR

`make check` must pass. Beyond that, see [CONTRIBUTING.md](../CONTRIBUTING.md) —
particularly the non-goals and the ADR-backed decisions that are not open for
re-litigation in a PR.
