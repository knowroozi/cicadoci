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

## Workflow — one PR per ticket, stacked

- **One pull request per Linear ticket.** Never bundle a milestone, or several tickets, into a single PR. Work that spans tickets spans PRs.
- **Stack the branches.** Each PR targets the branch of the ticket it builds on, not `master`, so its diff shows only that ticket's changes. Order the stack so every step builds and its tests pass on its own.
- **Take branch names from Linear** — the issue's `gitBranchName` field (e.g. `knowroozi/cic-7-postgres-schema-v1-migration-0001_initsql`). Linear then links the PR to the ticket automatically.
- Put the ticket ID in the PR title and body.
- **Review is manual and is meant to teach.** Keep each PR small enough to read in one sitting, and put the reasoning for non-obvious decisions in the PR body — especially anything a reviewer might reasonably have done differently.
- When a PR earlier in the stack changes, rebase the rest of the stack onto it.

Every PR body ends with a two-part checklist, and the split between the parts is the point of it:

- **Verified** — what was actually executed, and how. A command that was run, a test that passed, a behavior observed. Nothing goes here on the strength of having written it carefully.
- **Unverified / needs manual testing** — what this change touches that was *not* exercised, why not, and what the reviewer should try. Anything that could not be run in the working environment, anything only a human can judge, anything asserted from reading rather than running.

Listing something as verified when it was not actually run is the one thing that makes the checklist worse than having none: it converts an unknown into a false assurance.

## Conventions

- Estimates in docs are engineer-weeks (ew), not calendar time
- Work is tracked in Linear (team `CIC`, project "CICaDo MVP"), milestones M0–M9
- Update the doc in the same PR as the change it describes
- Current milestone: **M2 — Workflow definition**
