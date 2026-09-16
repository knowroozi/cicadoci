# Contributing to CICaDo

Thanks for your interest. CICaDo is Apache-2.0 ([ADR-006](project-docs/adr/ADR-006-license.md)) and intends to stay that way.

Start with [`project-docs/README.md`](project-docs/README.md) — the plan, architecture, frozen runner protocol, and ADRs are the source of truth for what this project is and is not.

## Before you write code

1. **Read [§2 Non-goals](project-docs/01-project-plan.md#2-non-goals-for-mvp).** Feature-parity-with-GitHub-Actions PRs will be declined, however good the code is. Scope discipline is the project's main survival strategy.
2. **Check the locked decisions in [CLAUDE.md](CLAUDE.md).** Go, Postgres-as-the-only-dependency, no frontend build step, frozen runner protocol, no third-party code in the control-plane process. These are ADR-backed. Changing one means writing a new ADR and getting it accepted — not opening a PR that quietly does it.
3. **Open an issue first** for anything non-trivial. Work is tracked in Linear (team `CIC`, project "CICaDo MVP"); GitHub issues get triaged in.

## Development setup

```sh
make dev-deps   # install pinned buf + golangci-lint into ./bin
make dev        # server + Postgres + one runner via docker-compose
make test       # unit tests (no external deps)
make test-all   # unit + integration; needs a Postgres (see below)
make check      # what CI runs: generate-check, lint, vet, race tests
```

Integration and end-to-end tests need a Postgres. Either point `CICADO_TEST_DATABASE_URL` at one you already have, or let the harness start a throwaway instance for you:

```sh
make test-e2e   # spins up a temporary Postgres, runs the walking skeleton
```

See [`docs/development.md`](docs/development.md) for the details.

## The rules that aren't negotiable

These come from the security invariants in [CLAUDE.md](CLAUDE.md) and the architecture doc. A PR that violates one does not get merged, regardless of what it enables:

- **Secret values** appear only in `GetJobSecrets` responses, authorized against the live lease. Never in job assignments, journals, status updates, or anything redeliverable.
- **Redaction runs on log ingest**, before persistence and before fan-out — not at render time.
- **No long-lived cloud credentials** anywhere in the system. Jobs federate via OIDC.
- **Every state-changing action**, from any surface (web, chat, MCP, API), writes an audit event.
- **The runner protocol is frozen.** Additive changes only: new fields with new tags, `reserved` on removals. Behavior changes ride capability flags in `Capabilities.features`, never version sniffing. `make proto` runs `buf breaking` against `main`; if it fails, the answer is a new field, not a changed one.

## Pull requests

- One logical change per PR. Reference the Linear issue (`CIC-123`) in the description.
- **Update the doc in the same PR as the change it describes.** Stale docs are treated as bugs.
- New significant decisions get a new ADR in `project-docs/adr/`, using [`ADR-000-template.md`](project-docs/adr/ADR-000-template.md). Superseded ADRs are marked superseded, never deleted or edited away.
- `make check` must pass. CI runs the same target.
- Generated code (`proto/gen/`) is committed; run `make proto` and include the result.
- Tests: new behavior needs a test. Anything touching secrets, redaction, authz, or the queue claim path needs a test that would fail if the protection were removed.

## Commit messages

Imperative subject, reference the issue, explain *why* in the body when it isn't obvious:

```
Add visibility-timeout sweeper to the job queue

Jobs claimed by a runner that dies without sending a terminal status
would sit in 'claimed' forever. The sweeper requeues anything whose
lease expired, capped at max_attempts before dead-lettering.

CIC-12
```

## Code of conduct

Participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).
