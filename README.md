# CICaDo

**An open-source, self-hostable CI/CD platform that is operationally simple, ChatOps-native, and AI-agent-native.**

Two Go binaries. Postgres as the only stateful dependency. S3 for blobs. No JVM, no Redis, no Kafka, no Kubernetes prerequisite, and no in-process plugin zoo.

> **Status: pre-alpha, under active development.** Milestone M0 (foundations) is in progress — the walking skeleton runs, but this is not yet usable as a CI system. See [the project plan](project-docs/01-project-plan.md) for the path to MVP.

## Why another CI system

Three things no incumbent owns together:

1. **Operational simplicity** — one binary for the server (UI embedded), one for the runner, one database. "Easier than Jenkins" made concrete rather than aspirational.
2. **ChatOps-native** — deploys, approvals, reruns, and diagnostics from Slack as a first-class surface, not a notification webhook bolted on the side.
3. **AI-agent-native** — structured per-step events, rule-based failure classification, and a built-in MCP server so agents can debug and triage runs safely, with read-only scoped tokens and per-invocation audit.

What it deliberately is **not**: feature parity with GitHub Actions. See [non-goals](project-docs/01-project-plan.md#2-non-goals-for-mvp).

## Repository layout

| Path | What lives there |
|---|---|
| `server/` | Control plane: HTTP API, gRPC runner service, scheduler, queue, storage |
| `runner/` | Runner agent: enrollment, session stream, executors (docker, shell) |
| `proto/` | The frozen runner protocol (`cicado/runner/v1`) and generated Go code |
| `web/` | Server-rendered UI assets — no build step ([ADR-003](project-docs/adr/ADR-003-frontend-no-build.md)) |
| `terraform/` | AWS reference deployment module |
| `docs/` | User-facing documentation |
| `project-docs/` | Plan, architecture, ADRs, protocol design — **the source of truth** |

## Quick start (development)

Requires Go 1.26+ and Docker.

```sh
make dev-deps    # install pinned tooling into ./bin
make dev         # server + Postgres + one runner, via docker-compose
make check       # everything CI runs: lint, vet, race tests, proto checks
```

Without Docker, `make test-e2e` runs the same end-to-end path natively against a temporary Postgres. See [`docs/development.md`](docs/development.md).

## Documentation

Start at [`project-docs/README.md`](project-docs/README.md), which indexes:

- [Project plan](project-docs/01-project-plan.md) — scope, milestones, estimates, risks, MVP exit criteria
- [Architecture](project-docs/02-architecture.md) — components, data model, queue, security baseline
- [Frontend approach](project-docs/03-frontend-approach.md) — the no-build strategy
- [Runner protocol](project-docs/04-runner-protocol.md) — streams, leases, reconnect/resume, versioning
- [ADRs](project-docs/adr/) — every significant decision, with its rationale and costs

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) first, especially the non-goals and the locked ADR-backed decisions. Vulnerability reports go through [SECURITY.md](SECURITY.md), never a public issue.

## License

[Apache-2.0](LICENSE), permanently ([ADR-006](project-docs/adr/ADR-006-license.md)).
