# Project Documents — CICaDo

CICaDo (plain spelling "Cicado"; binary `cicado`) · Site: **cicadoci.com** · Hosted app: **cicado.dev** · GitHub/social: **cicadoci** — see [ADR-007](adr/ADR-007-name-cicado.md) for the naming decision and registration checklist.

Self-hostable, OSS CI/CD platform: operationally simple, ChatOps-native, AI-agent-native.

## Contents

| Doc | Purpose |
|---|---|
| [01-project-plan.md](01-project-plan.md) | Complete plan to MVP: scope, milestones, workstreams, estimates, risks, success criteria |
| [02-architecture.md](02-architecture.md) | System architecture, components, data model, runner protocol, security baseline |
| [03-frontend-approach.md](03-frontend-approach.md) | No-build frontend strategy: server-rendered HTML + SSE + htmx/vanilla islands |
| [04-runner-protocol.md](04-runner-protocol.md) | Runner protocol design v1: streams, leases, reconnect/resume, versioning |
| [proto/runner_v1.proto.md](proto/runner_v1.proto.md) | Pointer to the canonical frozen proto at [`/proto/cicado/runner/v1/runner.proto`](../proto/cicado/runner/v1/runner.proto) |
| [adr/](adr/) | Architecture Decision Records (one decision per file, immutable once accepted) |

## ADR index

| ADR | Decision | Status |
|---|---|---|
| [ADR-000](adr/ADR-000-template.md) | Template + ADR process | — |
| [ADR-001](adr/ADR-001-language-go.md) | Go for control plane + runner | Accepted |
| [ADR-002](adr/ADR-002-postgres-queue.md) | Postgres as the only queue/store for MVP | Accepted |
| [ADR-003](adr/ADR-003-frontend-no-build.md) | No-build frontend: server-rendered HTML + SSE, htmx/vanilla | Accepted |
| [ADR-004](adr/ADR-004-runner-protocol.md) | Outbound-only gRPC streaming runner protocol | Accepted |
| [ADR-005](adr/ADR-005-plugin-model.md) | Container plugins with capability manifests + signing | Accepted |
| [ADR-006](adr/ADR-006-license.md) | Apache-2.0 core license | Accepted |
| [ADR-007](adr/ADR-007-name-cicado.md) | Product name: CICaDo | Accepted (registration checklist pending) |
| [ADR-008](adr/ADR-008-migrations-goose-embedded.md) | Schema migrations: goose as a library, embedded in the binary | Accepted |

## Conventions

- Docs are the source of truth; update the doc in the same PR as the change it describes.
- New significant decisions get a new ADR; superseded ADRs are marked, never deleted.
- Estimates are in **engineer-weeks (ew)** — full-time-equivalent effort, not calendar time.
