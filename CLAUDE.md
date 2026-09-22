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

## Workflow

- **One PR, one ticket.** Every code change traces to exactly one Linear issue, and that issue's whole story lives in the body of its own single pull request. Do not open a separate "cleanup" or refactor issue when the work is an inseparable part of another ticket — fold it into that ticket rather than spinning up extra review surface for reviewers who don't have context.

- **Stack the branches.** Each PR targets the branch of the ticket it builds on, not `master`, so its diff shows only that ticket's changes. Order the stack so every step builds and its tests pass on its own.
  - If a lower-down PR changes something an upper one depended on, rebase the rest of the stack — don't merge out of order or let it break silently.

- **Small enough to review.** If you realize while coding that this will be too big for one person to meaningfully read in a sitting, stop and split the ticket. The reviewer's ability to understand your change is a design constraint, not a formality — so build toward that.
  - Put your reasoning *in the PR description.* The body of a code-diff is where people look at what changed; it isn't a place they'll find your thinking about why.
    - Explain the approach, note any trade-offs you accepted or rejected, and if anything is deliberately left for later (a future milestone), say so in one line rather than burying it.

- **Verify honestly — every PR ends with a two-part checklist.**
  Every PR body ends with a two-part `Verified` / `Unverified — needs manual testing` section. This split isn't formality; the whole point is that *you know something and your reader doesn't yet*, so being upfront about which parts are proven versus assumed is what makes review actually teachable for the next person instead of turning into a guessing game.
    - `Verified`: things **you** ran or checked locally — say it, name it, state exactly how (e.g. "ran `go test` in a Docker Postgres; suite still green"). A claim you didn't execute goes in Unverified. If you can only confirm something is *not broken* rather than prove it works, say so plainly ("no regression found locally"; not "works" or "passing").
    - `Unverified — needs manual testing`: things that genuinely need a human on the other end (UI clicks against a live build, real secrets/credentials in place, external service wiring). Be exhaustive; under-listing this section is worse than over-listing. This list is how reviewers know where to actually spend their time once they pick it up — which means *you* are teaching them the test surface by handoff, and that's an explicit part of what "small enough to review" demands: small on size AND on cognitive load for whoever takes this one a step further toward merge-ready, including genuinely knowing exactly where their own hands need to go in order to make it so.
    - **Never claim anything under Verified that wasn't actually executed** — a wrong claim here is worse than no checklist at all, because it converts an unknown into a false assurance. If you're unsure whether something was checked, it goes in Unverified until it isn't.

- **Hand back before merge; keep merges human-owned.** This project explicitly does not self-merge: a PR is reviewed by another person (or an agent acting strictly on their instruction) in the same teach-oriented way a careful human reviewer would — and only when they've actually read the diff do we consider it done. Keep "the work" small and legible *so that* step stays fast; don't treat a green CI run as a substitute for someone having genuinely looked at what changed, since automated checks can confirm nothing was broken but say nothing about whether this is the right design to have in the first place — that second half of the job belongs to the human on the receiving end.
## Conventions

- Estimates in docs are engineer-weeks (ew), not calendar time
- Work is tracked in Linear (team `CIC`, project "CICaDo MVP"), milestones M0–M9
- Update the doc in the same PR as the change it describes
- Current milestone: **M0 — Foundations**
