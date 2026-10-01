# CICaDo — Project Plan: Path to MVP

Status: Active · Owner: Kaveh · Last updated: 2026-09-30 · Name: see ADR-007

## 1. Vision & thesis

A fully open-source, fully self-hostable CI/CD platform that beats the incumbents on three axes none of them own together:

1. **Operational simplicity** — one Go binary for the server (UI embedded), one for the runner, Postgres as the *only* stateful dependency, S3 for blobs. "Easier than Jenkins" made concrete: no JVM, no plugin zoo in-process with the controller, no Redis/Kafka/K8s prerequisite.
2. **ChatOps-native** — deploys, approvals, reruns, and diagnostics from Slack as a first-class surface, not a notification webhook.
3. **AI-agent-native** — structured logs, failure classification, and a built-in MCP server so agents (Claude, etc.) can debug, diagnose, and triage runs safely.

**MVP definition:** a team can install the platform on AWS via a Terraform module, connect it to GitHub as a GitHub App, define a YAML workflow, and run git/bash/AWS-CLI jobs on their own EC2 runners with live logs, secrets, Slack notifications + approvals, and status reported back to PRs via the Checks API.

## 2. Non-goals for MVP

Explicitly out of scope until post-MVP (resist all temptation):

- Kubernetes executor, autoscaling runner fleets, Firecracker/gVisor isolation
- Plugin marketplace/registry (the *container plugin execution model* ships; the marketplace does not)
- WASM plugin tier, reusable workflow packages (orbs equivalent)
- GitLab/Bitbucket adapters (interface designed for them; only GitHub implemented)
- Matrix builds, caching subsystem (artifacts yes, cache no)
- Multi-cloud deployment targets (interfaces abstracted; only AWS implemented)
- Teams/Discord/Mattermost (chat provider interface designed; only Slack implemented)
- Write-capable MCP tools (read/diagnose only in MVP)
- Windows/macOS runners (Linux x86_64 + arm64 only)

## 3. Success criteria (MVP exit)

- [ ] `terraform apply` from a clean AWS account to a working install in < 30 minutes
- [ ] GitHub PR push → check appears pending → job runs → check completes, end-to-end p50 < 15s overhead (excluding job runtime)
- [ ] A secret set in the UI never appears in any log (verified by adversarial test suite)
- [ ] A deploy job assumes an AWS role via OIDC with zero stored AWS keys
- [ ] An approval requested in Slack can be granted by an authorized user and denied for an unauthorized one (tested)
- [ ] Claude, pointed at the MCP server, can correctly explain why a seeded failing build failed (manual eval, 10 scenarios)
- [ ] The platform builds, tests, and deploys **itself** (dogfooding gate)
- [ ] Fresh-eyes install test: someone who didn't build it self-hosts from docs alone

## 4. Workstreams & milestones

Estimates are engineer-weeks (ew) of focused FTE effort. As a solo/nights-weekends project, multiply by your availability factor. Milestones are sequenced but workstreams within them can interleave. **Total: ~40–48 ew to MVP exit.**

### M0 — Foundations (3 ew)
- Monorepo layout (`/server`, `/runner`, `/proto`, `/web`, `/terraform`, `/docs`)
- Protobuf/gRPC contracts for the runner protocol (see ADR-004) — **this is the hardest-to-change artifact in the project; spend real design time here**
- Postgres schema v1: orgs, repos, workflows, runs, jobs, steps, log_chunks, secrets, runners, tokens, audit_events
- CI-for-the-CI (bootstrap on GitHub Actions, migrate to self once dogfoodable)
- Apache-2.0 license, CONTRIBUTING, code of conduct, ADR process
- **Exit:** `make dev` brings up server + Postgres + one runner in docker-compose; a hardcoded job runs `echo hello` end to end.

### M1 — Core engine (6 ew)
- Server: HTTP API skeleton, gRPC runner service, scheduler loop
- Job queue on Postgres: `FOR UPDATE SKIP LOCKED` claim, `LISTEN/NOTIFY` doorbell, visibility timeouts, retry with backoff, dead-lettering
- Runner: registration (token-based), outbound gRPC stream, heartbeat, graceful drain
- Executor abstraction with `docker` (fresh container per step, workspace volume) and `shell` executors
- Step model: env injection, exit-code capture, timeouts, cancellation propagation (context-driven, kills containers)
- **Exit:** multi-step job defined in the DB runs on a remote runner over the wire; kill/cancel works; runner survives server restarts (reconnect + resume).

### M2 — Workflow definition (4 ew)
- YAML schema v1 (published JSON Schema — doubles as the AI/agent contract): triggers, jobs, steps, `needs` DAG, per-step `image`/`run`/`env`, secrets refs, approval gates
- Parser with **precise, positional error messages** (a top-3 UX differentiator; GitHub Actions' silent YAML failures are a known pain point)
- DAG resolution, fan-out/fan-in scheduling
- `validate` CLI subcommand + API endpoint (also exposed to agents later)
- Guardrail: no expressions/loops/conditionals beyond simple `if:` on step status + branch/tag filters. Logic lives in steps. (Jenkins Groovy lesson.)
- **Exit:** workflows load from repo files; invalid YAML produces line/column errors a human and an agent can act on.

### M3 — GitHub App integration (5 ew)
- GitHub App: webhook receiver (`push`, `pull_request`, `check_suite`, `check_run` rerequests), signature verification, delivery replay tolerance (idempotency keys)
- Checks API reporting: pending → in_progress → completed with summary + annotations; re-run from GitHub UI
- Short-lived installation tokens minted per job for clone; never persisted
- Fork-PR policy: untrusted PRs get no secrets, restricted runner pool tag
- Forge adapter interface (`Forge` interface in Go) so GitLab/Bitbucket are later drivers, not rewrites
- **Exit:** push to a connected repo triggers a run; PR shows live check status; re-run works from both sides.

### M4 — Logs & artifacts (4 ew)
- Runner → server log chunk streaming over the existing gRPC stream (multiplexed)
- **Secret redaction on ingest** — every registered secret value (and derived encodings: base64, URL-encoded) scrubbed before persistence or fan-out. Adversarial test suite. Non-negotiable, day one. (tj-actions CVE-2025-30066 lesson: log exfiltration needs no network.)
- Hot logs in Postgres, flush-to-S3 on completion, transparent read-through
- SSE fan-out to browsers with `Last-Event-ID` resume
- Artifacts: upload/download via presigned S3 URLs, per-run namespacing, retention policy
- Structured event log per step: JSON events (step start/stop, exit code, phase, error class) alongside raw text — the substrate for both the UI and MCP
- **Exit:** watch a live log in the browser mid-run over SSE; artifacts round-trip; a seeded secret printed by a job shows as `***`.

### M5 — Secrets & identity (4 ew)
- Secrets: envelope encryption (KMS data keys), org/repo/environment scoping, write-only API, audit on every read
- Per-job ephemeral platform tokens, scoped to the run (claims: repo, run, job, branch)
- **OIDC issuer**: the server signs job identity JWTs; documented Terraform for the AWS side (IAM OIDC provider + role trust conditions on `sub`/`aud`) → jobs do `sts:AssumeRoleWithWebIdentity`. Zero stored cloud keys.
- Audit event pipeline: every state-changing action (human, chat, or agent) recorded with principal, source surface, and payload hash
- **Exit:** demo deploy job assumes an AWS role via OIDC and runs `aws s3 ls`; audit trail shows who/what/when.

### M6 — Web UI (5 ew)
- Per ADR-003: Go `html/template`, assets via `embed.FS`, htmx (vendored) + SSE extension, small vanilla JS islands
- Screens: repo list, run list, run detail (DAG + step logs with live tail, virtualized log viewer island), workflow file viewer w/ validation, secrets management, runner fleet status, settings/tokens
- AuthN: GitHub OAuth (reuse the App); AuthZ: simple role model (admin/member) for MVP
- **Exit:** all MVP operations doable in the browser; zero frontend build step; `go build` produces the whole product.

### M7 — ChatOps: Slack (4 ew)
- Slack app: Socket Mode (default, self-hosting friendly — no ingress) *and* Events API (documented for scale)
- Slash commands: `/ci status <repo>`, `/ci run <repo> <workflow> [ref]`, `/ci rerun <run>`, `/ci logs <run> [step]` (tail)
- **Approval gates as Block Kit buttons** — with mandatory acting-user authorization on every interaction (buttons are clickable by anyone in-channel; the handler must check), 3-second ack discipline, edit-in-place threaded run status (no channel spam)
- Chat provider interface (`ChatProvider`) with Slack as sole driver
- All chat actions go through the public API + audit pipeline — chat is a client, never a side door
- **Exit:** a deploy can be triggered, watched, approved, and re-run without leaving Slack; unauthorized approval attempts are rejected and audited.

### M8 — MCP server & AI surface (3 ew)
- Read-only MCP tools: `list_runs`, `get_run`, `get_job`, `get_failed_step_logs`, `get_structured_events`, `classify_failure`, `diff_against_last_green`, `validate_workflow`
- Failure classifier v1: rule-based taxonomy (exit-code + phase + log-pattern → {infra, deps, test, timeout, oom, auth, user-script}) emitted into structured events
- MCP auth: scoped tokens (`read` scope only in MVP), every invocation audited with principal
- Eval harness: 10 seeded failure scenarios, manual grading of agent explanations
- **Exit:** the success-criteria Claude eval passes ≥8/10.

### M9 — AWS packaging, hardening & launch prep (4–6 ew)
- Terraform module: VPC, ALB, ECS Fargate (control plane), RDS Postgres, S3 (logs/artifacts), Secrets Manager + KMS, EC2 ASG runners (Docker installed; Fargate can't — no privileged/DinD), IAM OIDC provider
- Runner AMI or cloud-init; `curl | sh` installer for on-prem runners
- Backpressure & limits: max concurrent jobs, log rate caps, payload size caps, webhook flood protection
- Security pass: threat model doc, dependency audit, secret-redaction fuzzing, authz test matrix
- Docs site: install guide, workflow reference (generated from JSON Schema), Slack setup, MCP setup
- Dogfood cutover: this project's own CI moves onto itself
- **Exit:** all §3 success criteria green.

## 5. Sequencing notes & critical path

```
M0 → M1 → M2 → M3 → M4 → M5 → {M6, M7, M8 in parallel} → M9
```

- The **runner protocol (M0/M1)** and **executor interface (M1)** are the two decisions that are near-impossible to change later. Everything else can be refactored.
- M6/M7/M8 are all thin clients over the same API — build the API right in M1–M5 and these go fast; the estimates assume that discipline holds.
- Dogfooding starts the moment M3 lands (run this repo's tests on itself), not at M9.

## 6. Risks & mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Runner protocol needs breaking changes post-launch | Med | High | Version the protobuf from day 1; runner auto-update channel; server supports N-1 runners |
| Secret leakage bug | Med | Critical | Redaction-on-ingest + adversarial tests + fuzzing in CI; secrets never in env of the *server* process |
| Scope creep toward feature parity with Actions | High | High | §2 non-goals enforced; every addition needs an ADR |
| Solo-founder bandwidth; 40+ ew is long | High | High | Ship M3 as a public "toy" release for early feedback/contributors; Apache-2.0 + good CONTRIBUTING to attract help |
| Postgres queue hits limits | Low (MVP scale) | Med | Queue behind interface; documented migration threshold (sustained k-jobs/sec) → NATS/SQS driver |
| GitHub API/webhook quirks eat time | Med | Med | Budget padding in M3; replay/idempotency from the start |
| Fargate constraint surprises (image builds) | Low | Med | Known: runners on EC2; document kaniko/BuildKit-rootless path for users who need Fargate |
| Market: "CI compute is a commodity" (Earthly lesson) | — | Strategic | This is OSS-first; monetization (if ever) = managed control plane / compliance add-ons, never core compute |

## 7. Post-MVP roadmap (abridged)

- **Phase 1 (hardening/extensibility):** container plugin capability manifests + cosign verification, verified/community plugin tiers, gVisor or Firecracker option for untrusted jobs, write-capable MCP tools behind human-in-the-loop consent, caching subsystem, matrix builds
- **Phase 2 (scale/portability):** Kubernetes executor, BlobStore/SecretStore/Queue drivers for GCP/Azure/MinIO/Vault/NATS, reusable workflow packages, Starlark programmable-config track, Wasm (Extism) in-process hooks, Teams/Mattermost/Discord adapters, GitLab/Bitbucket forge drivers

## 8. Open questions (need decisions, tracked as future ADRs)

1. ~~Project name + domain~~ **Resolved: CICaDo** (ADR-007) — cicadoci.com (site) + cicado.dev (hosted app) purchased; GitHub/social handle `cicadoci`; npm/Docker namespaces, USPTO knockout, and pre-launch counsel review still pending
2. Org auth model beyond GitHub OAuth (SAML/OIDC SSO is a classic open-core paid line — decide posture early and publicly)
3. ~~Runner ↔ server transport fallback~~ **Resolved (protocol §11): gRPC-only, no HTTP/1.1 fallback** — clean HTTP/2 egress to :443 is a documented requirement; revisit only on demonstrated demand
4. Log retention defaults and S3 lifecycle policy defaults
5. Telemetry: opt-in anonymous usage stats? (Community-sensitive — decide before v0.1)

## 9. Implementation status snapshot

_Last refreshed 2026-10-01 against `master` (`5d6722f`). Per-ticket status tracked in Linear (cicado-mvp board); this section summarizes milestone-level progress. The 2026-09-30 snapshot (`c8d89fc`) overstated two *wired* claims — `approvalgates` and `backpressure` — corrected 2026-10-01 (QC, ticket 3bb095): both are on `master` but inert in the shipped binary (see the QC note below).

> **Read note (2026-09-30).** `gh` reporting a PR as `MERGED` does **not** by itself mean its content is on `master` during the M4–M9 recovery: bottom-up rebase promoted most of it, but a merged recovery PR can merge into its base branch without landing on the final `master` line. The authoritative check is `git ls-tree origin/master`. One such case — `server/slack/` (PR #65 / CIC-49) is `MERGED` but **not on `master`** — is flagged under M7 and in the recovery table.

> **QC note (2026-09-30, from `master` @ `c8d89fc`).** Each milestone in this snapshot now reports *two* axes: **code on `master`** (merged) and **wired into the `cicado-server` binary** (active at runtime). These can differ: as of `c8d89fc` the running server serves webhooks + runner gRPC + the Postgres queue with a `NoOpForge`, so M3 check reporting/rerequest and the M4 log/artifact packages are *on `master` but not wired* (CIC-81, CIC-82), and M5 secrets cannot be delivered end-to-end yet (CIC-79). End-to-end wiring is tracked by CIC-78–CIC-83; code-comment cleanups are CIC-83. (The M7 `server/slack/` item, below, is a *merge* anomaly, distinct from this wiring gap.) **Correction (2026-10-01):** two 2026-09-30 claims below were *overstated* — `approvalgates` and `backpressure` are on `master` but inert in the shipped binary (CIC-82, CIC-106).

### M0 — Foundations ✅ Complete
All deliverables shipped: monorepo layout, runner protocol v1 (frozen), Postgres schema v1, CI-for-the-CI (GitHub Actions), Apache-2.0, ADR process. PRs #1 through #8 (CIC-13–CIC-22).

### M1 — Core engine ✅ Complete
HTTP API skeleton (store.Reader seam), gRPC runner service, `FOR UPDATE SKIP LOCKED` job queue with `LISTEN/NOTIFY`, runner registration/heartbeat/drain, docker + shell executors, step model with context cancellation. PRs #12–#18 (CIC-23–CIC-29). M3 E2E walking-skeleton test validates end-to-end.

### M2 — Workflow definition ✅ Complete
YAML schema v1 + published JSON Schema, positional-error parser, topological DAG, `validate` CLI subcommand, step-level `if:` conditions (parsed/validated but **not yet enforced at the runner**, CIC-107) + branch/tag filters. JSON Schema validation. PRs #19–#21 (CIC-30–CIC-32). Note: CIC-32 code (archive) currently in PR #64.

### M3 — GitHub App integration ✅ Code on master; runtime partial (CIC-81)
Webhook receiver, HMAC-SHA256 signature verification with replay tolerance, short-lived installation JWTs, fork-PR policy (no secrets + restricted runner tag), `Forge` adapter interface. PRs #23–#26 (CIC-33–CIC-37). CIC-37 (secrets store) in master; CIC-35 covered by CIC-33.
- **Wired into `cicado-server`:** webhook receipt, signature verification, and `push`/`pull_request` → run creation are served over the HTTP API — *with two runtime caveats*: on real GitHub payloads `pull_request` runs resolve an **empty ref/SHA and `fork=false`** (CIC-102), and workflow jobs **carry no steps yet** (CIC-104).
- **Code on master, not wired (CIC-81):** *Checks API reporting* and *check_suite / check_run rerequests* are inert at runtime — the server starts with `controller.NoOpForge{}` (`server/cmd/cicado-server/main.go:157`) and `HandleCheckSuite`/`HandleCheckRun` early-return with a "no-op until job resolution lands" log. Reporting/rerequest wiring is CIC-81.

### M4 — Logs & artifacts ✅ Code on master; not yet wired into cicado-server (CIC-82)
The following packages are present on `master` (shipped via the merged recovery PRs). As of `c8d89fc`, none is imported by any binary entrypoint (`cmd/`, `server/cmd/`, `runner/` — verified), so they are code-on-master but not yet wired:
- `server/redact/` — secret redaction on ingest with the adversarial test suite (CIC-31, PR #30).
- `server/blobstore/` + `server/artifacts/` — S3 blob storage, per-run namespacing, retention/compaction foundation (CIC-34, PR #56 merged). **Not imported by any binary.**
- `server/archive/` — hot/cold log archiving to the blobstore (CIC-32, PR #64 merged). **Not imported by any binary.**
- `server/logs/` + `server/runnersvc/` — `runnersvc` *is* wired (log chunks persist on the runner stream), but the **SSE browser fan-out is not routed**: `logs.NewSSE` is never called outside `server/logs` and the `LogPublish` callback is never assigned, so chunks persist but are not fanned out to browsers (CIC-33, PR #54; wiring is CIC-82).

### M5 — Secrets & identity 🔄 In progress
**On master:**
- `server/secrets/` — NUL-byte composite key, env scoping, write-only API, plus the Postgres backend (CIC-37, PR #45; CIC-38 wiring).
- `GetJobSecrets` RPC — per-job secret fetch over gRPC; on `master` and registered on the gRPC server, **but a running job cannot yet receive secrets end-to-end** — the requesting path that would call it is not wired (CIC-79). (CIC-38, PR #83 merged 2026-09-29; [decision doc](../docs/decisions/CIC-38-getjobsecrets-rpc.md)).
- `server/approvalgates/` — approval-gate state machine with per-user authz and the MessageID fix (CIC-51, PR #66 merged).
- Note: `server/mcp/` was previously listed under M5 in this snapshot; it is an M8 deliverable and is on master (see M8).

**Not yet started (Linear Backlog) — confirmed absent from `master` on 2026-09-30:**
- CIC-39: OIDC issuer — server signs job-identity JWTs; AWS IAM OIDC provider + `sts:AssumeRoleWithWebIdentity` (no issuer package on master).
- CIC-40: Per-job ephemeral platform tokens, scoped to run claims (no dedicated subsystem on master).
- CIC-42: M5 E2E — deploy job assumes an AWS role via OIDC (not started).

### M6 — Web UI ❌ Not started (all Backlog in Linear)
No code in master or in any open PR. All 6 tickets in Backlog:
- CIC-43: UI foundation — `html/template`, `embed.FS`, htmx vendored, zero build step
- CIC-44: GitHub OAuth authN + admin/member role model
- CIC-45: Repo list, run list screens
- CIC-46: Run detail — DAG view + virtualized log viewer island (SSE live tail)
- CIC-47: DAG island SVG rendering
- CIC-48: Secrets management, runner fleet status, settings/tokens screens

_Largest remaining gap: ~5 ew._

### M7 — ChatOps: Slack 🔄 In progress
**On `master` (via merged recovery PRs). Runtime status (as of `5d6722f`):** `approvalgates` is **code on `master` but *not wired*** — `controller.DefaultConfig()` leaves `ApprovalGates` nil, so `maybeOpenGate` (`server/controller/controller.go:350`) short-circuits and **no approval gate ever opens in the shipped binary** (the import exists, but a nil tracker is a no-op; CIC-82). `chat`, `runstatus`, and `slash` are likewise code on `master` but **not yet imported by any binary**. Per-feature wiring is tracked by CIC-78–CIC-83.
- `server/chat/` — `ChatProvider` interface + Slack driver (`server/chat/slack.go`), typed `Action` constants — the Slack foundation that is on master (CIC-53, PR #58 merged).
- `server/approvalgates/` — Block Kit approval buttons with mandatory acting-user authz (CIC-51, PR #66 merged).
- `server/runstatus/` — threaded, edit-in-place run status via `ChatProvider` (CIC-52, PR #67 merged).
- `server/slash/` — Slack slash-command parser (`/ci status|run|rerun|logs`) (CIC-50, PR #57 merged).

> **Flag — not on master:** PR [#65](https://github.com/cicadoci/core/pull/65) (CIC-49, `server/slack/`) is `MERGED` in `gh`, but its merge commit `33f47d6` is **not an ancestor of `master`** and `server/slack/` is absent from `master`'s tree. The standalone `server/slack/` Slack-app package did **not** promote; the Slack foundation present on master is `server/chat/slack.go` (CIC-53). Left for the owner to reconcile (PR #65 vs the CIC-49 ticket).

### M8 — MCP server & AI surface ✅ On master (2026-09-30 recovery)
All M8 packages are present on `master`:
- `server/classify/` — rule-based failure taxonomy, nine categories (CIC-54, PR #52 merged).
- `server/mcp/` — read-only MCP server: list_runs, get_run, get_job, get_failed_step_logs, get_structured_events, classify_failure (CIC-55, PR #59 merged). (The MCP JSON-RPC transport itself is deferred to M9 — see [CIC-76](../docs/decisions/CIC-76-mcp-defer.md).)
- `server/mcpauth/` — scoped-token auth + per-invocation audit (CIC-56, PR #60 merged).
- `server/ai/` — 10-scenario eval harness; `EvalCase.Expected` is `classify.Classification` (depends on classify, in master) (CIC-57, PR #61 merged).

### M9 — AWS packaging, hardening & launch prep 🔄 In progress
**On `master` (via recovery PRs). Runtime status (as of `5d6722f`):** `server/backpressure` is **code on `master` in both the controller and `server/dispatch`**, but in the shipped binary only the *controller's* tracker is non-nil — `dispatch.DefaultConfig` leaves its `Backpressure` nil and `cmd/cicado-server/main.go:145` passes no tracker — so it caps **concurrent webhook handlers, not run dispatch** (CIC-106). `runner/lambda` is a separate Lambda entrypoint and `Dockerfile`/`build.sh` are packaging.
- `runner/lambda/` — AWS Lambda runner adapter (`Dispatch()` accepts `*JobEvent`, records `journal.LaneStatus`) (CIC-59, PR #62 merged).
- `server/backpressure/` — max concurrent jobs, log rate caps, payload size caps (CIC-60, PR #63 merged).
- `Dockerfile`, `build.sh` — distroless server image, debian runner image (CIC-58, PR #47 merged).

**Not yet started (Linear Backlog):**
- CIC-61: Security pass — threat model doc, dependency audit, secret-redaction fuzzing, authz test matrix
- CIC-62: Docs site — install guide, workflow reference (from JSON Schema), Slack/MCP setup
- CIC-63: Dogfood cutover — this project's CI moves onto itself
- CIC-64: Telemetry/SSO decisions (see open questions §8.2, §8.5)
- CIC-65: Name registration tail — npm/Docker namespaces, USPTO, counsel review

### Control bridge — webhook → run → job DAG (CIC-71 series, cross-cutting)
Not one of the §4 milestones, but the integration that wires M1–M5 packages into a live path. As of 2026-09-30:
- **On master:** `server/controller/` + `server/dispatch/` + `server/events/` + `server/policy/` — first real webhook-runs dispatcher (CIC-71a, PR #75), real org/repo resolution via the store (CIC-71b, PR #76), workflow→job-DAG creation (CIC-71c, PR #77), and the dispatcher wired into the `cicado-server` binary (CIC-71d, PR #78). Supporting hardening merged too: `0003_artifacts.sql` migration (CIC-72), masked-masking/token-registration fix (CIC-74), audit-events implementation (CIC-75).
- **Also on `master` (CIC-71e–h merged, as of `5d6722f`):** CIC-71e `StoreLoader` reads workflow definitions from the DB (PR #79), CIC-71f run-creation audit events (PR #80), CIC-71g backpressure tracker present in controller *and* dispatch (PR #81) — but the dispatch-side tracker is nil in the shipped binary, inactive there (CIC-106); CIC-71h added the `approvalgates` field + call to the controller (PR #82) — but `ApprovalGates` is nil in `DefaultConfig`, so **no gate opens** in the shipped binary (CIC-82).
- **Open (as of 2026-09-30):** CIC-77 per-PR knowledge-graph currency (PR #84, still open). (Deferred-but-documented: S3 `PreSignedURLProvider` → M9, CIC-73; MCP JSON-RPC transport → M9, CIC-76.)
- Decision records: `docs/decisions/CIC-71a-controller-dispatcher.md`, `CIC-71b-real-org-lookup.md`, `CIC-71c-workflow-dag.md`, `CIC-71d-server-wiring.md`, `CIC-72-artifacts-migration.md`, `CIC-73-presigned-url-defer.md`, `CIC-74-masker-wiring.md`, `CIC-75-audit-postgres.md`, `CIC-76-mcp-defer.md`.

### Recovery PR summary
14 individual recovery PRs (#54–#67) cover M4/M5/M7/M8/M9 packages that were lost from `master` when intermediate stacked branches superseded each other without promoting to master. Merged bottom-up: each wave-2 PR's base auto-resolves to master when its dependency merges.

| PR | Ticket | Package(s) |
|---|---|---|
| [#55](https://github.com/cicadoci/core/pull/55) | CIC-41 | `server/audit/` |
| [#56](https://github.com/cicadoci/core/pull/56) | CIC-34 | `server/blobstore/` + `server/artifacts/` |
| [#64](https://github.com/cicadoci/core/pull/64) | CIC-32 | `server/archive/` |
| [#54](https://github.com/cicadoci/core/pull/54) | CIC-33 | `server/logs/` + `server/runnersvc/` |
| [#58](https://github.com/cicadoci/core/pull/58) | CIC-53 | `server/chat/` |
| [#66](https://github.com/cicadoci/core/pull/66) | CIC-51 | `server/approvalgates/` |
| [#65](https://github.com/cicadoci/core/pull/65) | CIC-49 | `server/slack/` |
| [#67](https://github.com/cicadoci/core/pull/67) | CIC-52 | `server/runstatus/` |
| [#57](https://github.com/cicadoci/core/pull/57) | CIC-50 | `server/slash/` |
| [#59](https://github.com/cicadoci/core/pull/59) | CIC-55 | `server/mcp/` |
| [#60](https://github.com/cicadoci/core/pull/60) | CIC-56 | `server/mcpauth/` |
| [#61](https://github.com/cicadoci/core/pull/61) | CIC-57 | `server/ai/` |
| [#62](https://github.com/cicadoci/core/pull/62) | CIC-59 | `runner/lambda/` |
| [#63](https://github.com/cicadoci/core/pull/63) | CIC-60 | `server/backpressure/` |

**State (2026-09-30): all 14 recovery PRs are `MERGED` in `gh`.** Their packages are present on `master` **except** `server/slack/` (PR #65 / CIC-49): its merge commit (`33f47d6`) is not an ancestor of `master`, so that package did not promote and is not on `master` — see the M7 flag. The Slack foundation that is on master lives in `server/chat/` (CIC-53). **Wiring status (CIC-78–CIC-83):** being merged (code on `master`) is not the same as being wired into `cicado-server`; as of `5d6722f` the wired subset is webhook→run creation, run-creation audit events, and the controller's webhook-handler backpressure cap; **`approvalgates` and the dispatch-loop backpressure tracker are on `master` but inert in the shipped binary** (`ApprovalGates` nil in `DefaultConfig`, CIC-82; dispatch `Backpressure` nil, CIC-106), as are the M4 log/artifact packages and M5 end-to-end secrets.

### Overall assessment (refreshed 2026-10-01, master @ `5d6722f`; 2026-09-30 `c8d89fc` snapshot corrected re approvalgates/backpressure)
**Two axes — *code on master* vs *wired into cicado-server* — are tracked separately throughout this snapshot:**
- **M0–M3**: 100% on `master`. M3 check reporting + rerequest are **code-on-master but inert at runtime** (`NoOpForge`; CIC-81). Webhook receipt + run creation are wired, but with runtime caveats: `pull_request` runs resolve **empty ref/SHA and `fork=false`** on real GitHub payloads (CIC-102), workflow jobs **carry no steps yet** (CIC-104), and step-level `if:` conditions are **parsed but not enforced** at the runner (CIC-107).
- **M4 / M8**: code on `master` (logs/artifacts + MCP/AI, all merged) but **not yet wired** into binaries — M4 SSE/artifact packages are unimported, and M8 has no served surface (transport deferred to M9, CIC-76).
- **M5**: `server/secrets` + `GetJobSecrets` RPC (CIC-38) + `approvalgates` on `master`; the RPC is registered but **no job can receive secrets end-to-end yet** (CIC-79). Still missing the OIDC issuer (CIC-39), per-job platform tokens (CIC-40), and M5 E2E (CIC-42).
- **M7**: `approvalgates` is on `master` but **not wired** into the shipped binary (`ApprovalGates` nil in `DefaultConfig` → no gate opens; CIC-82); `chat`/`runstatus`/`slash` are on `master` but not yet wired; `server/slack/` (CIC-49) did **not** promote — see the flag above.
- **M9**: `server/backpressure` is code-on-master in controller + dispatch, but in the shipped binary only the controller's cap is active — it caps **concurrent webhook handlers, not run dispatch** (dispatch tracker nil; CIC-106); `runner/lambda` (separate Lambda entrypoint) and image packaging are on `master`; security pass / docs site / dogfood cutover / telemetry-SSO / name-registration tail remain backlogged.
- **Control bridge** (CIC-71/72–76): the full CIC-71 series (71a–h) is **on `master`** as of `5d6722f`; wiring is still progressing via CIC-78–CIC-83. CIC-77 (knowledge-graph currency, PR #84) is still open.
- **M6 Web UI**: still 0% in code (`web/` is `README.md` only) — 6 tickets backlogged, ~5 ew, the largest remaining gap.

