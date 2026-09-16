# CICaDo — Project Plan: Path to MVP

Status: Draft v1 · Owner: Kaveh · Last updated: 2026-09-07 · Name: see ADR-007

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
