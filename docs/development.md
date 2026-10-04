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
