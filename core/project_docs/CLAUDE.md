# CLAUDE.md — CICaDo

Read `project-docs/README.md` first. It indexes the plan, architecture, frontend approach, frozen runner protocol, and ADRs.

## What this is

CICaDo: an open-source, self-hostable CI/CD platform. Differentiators are operational simplicity, ChatOps-native (Slack), and AI-agent-native (MCP). Not feature parity with GitHub Actions.

## Locked decisions — do not re-litigate

These are recorded in ADRs. Changing one requires a new ADR, not a code change.

- **Go** for server and runner; no JVM, no Node runtime for users (ADR-001)
- **Postgres is the only stateful dependency** — it is also the job queue (`FOR UPDATE SKIP LOCKED` + `LISTEN/NOTIFY`). No Redis/SQS/Kafka in MVP (ADR-002)
- **No frontend build step** — Go `html/template` + `embed.FS` + vendored htmx + SSE + vanilla ES-module islands. No npm, no bundler, no React, no templ. `go build` produces the whole product (ADR-003)
- **Runner protocol v1 is FROZEN** — additive changes only (new fields/new tags, `reserved` on removals). Behavior changes ride capability flags, never version sniffing. See `project-docs/04-runner-protocol.md` + `proto/` (ADR-004)
- **No third-party code ever runs in the control-plane process** — plugins are containers with capability manifests (ADR-005)
- **Apache-2.0 core, permanently** (ADR-006)
- **Name**: CICaDo / `cicado` binaries / `cicadoci` namespaces (ADR-007)

## Security invariants (never trade these for velocity)

- Secret values never appear in assignments, journals, or anything redeliverable — only in `GetJobSecrets`, authorized against the live lease
- Secret redaction runs on log ingest, before persistence or fan-out
- No long-lived cloud keys anywhere: jobs federate to AWS via OIDC → `AssumeRoleWithWebIdentity`
- Fork PRs get no secrets, no deploy identities, restricted runner tags
- Every state-changing action from any surface (web/chat/MCP/API) writes an audit event
- Slack interactive payloads must authorize the *acting user* — buttons are clickable by anyone in the channel

## Conventions

- Estimates in docs are engineer-weeks (ew), not calendar time
- Work is tracked in Linear (team `CIC`, project "CICaDo MVP"), milestones M0–M9
- Update the doc in the same PR as the change it describes
- Current milestone: **M0 — Foundations**
