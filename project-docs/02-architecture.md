# System Architecture

Status: Draft v1 · Companion to 01-project-plan.md

## 1. Component overview

```
                        ┌─────────────────────────────────────────┐
  GitHub ──webhooks──▶  │                SERVER (Go, one binary)  │
  Slack  ──socket────▶  │  ┌──────────┐ ┌───────────┐ ┌────────┐ │
  Browser ─https/sse─▶  │  │ HTTP API │ │ Scheduler │ │  MCP   │ │
  Agents ──mcp───────▶  │  │ + UI     │ │  (queue)  │ │ server │ │
                        │  └────┬─────┘ └─────┬─────┘ └───┬────┘ │
                        │       └──────┬──────┴───────────┘      │
                        │        ┌─────▼──────┐  ┌────────────┐  │
                        │        │  Postgres  │  │ gRPC runner│  │
                        │        │ (only DB)  │  │  service   │  │
                        │        └─────┬──────┘  └─────▲──────┘  │
                        └──────────────┼───────────────┼─────────┘
                                 S3 (logs/artifacts)   │ outbound only
                                                 ┌─────┴──────┐
                                                 │ RUNNER (Go)│
                                                 │ executors: │
                                                 │ docker,    │
                                                 │ shell      │
                                                 └────────────┘
```

Two binaries. Server embeds the web UI (`embed.FS`). Postgres is the only stateful service; S3 is the only blob store. Everything else is a client of the HTTP API (browser, Slack service, MCP) — one authz + audit path.

## 2. Data model (v1 core tables)

`orgs → repos → workflows → runs → jobs → steps`

- **runs**: trigger metadata (event, ref, sha, actor), status, timings
- **jobs**: the schedulable unit; runner tag requirements; queue and lease fields
  (claimed_by, claimed_at, lease_id, lease_expires_at, attempts, available_at). `needs` edges live
  in **job_dependencies** so "which jobs just became eligible" is an index lookup, not a scan
- **job_status_updates**: (job_id, seq) idempotent status upserts, per the protocol's redelivery rules
- **steps**: image/run/env resolved; exit code; structured event stream reference
- **log_entries**: (job_id, seq) hot storage for log chunks *and* structured events in one
  sequence space — the protocol gives them a shared seq so a single watermark covers both and
  replay ordering after a reconnect is total, so schema v1 makes that a primary key rather than
  an application invariant across two tables. Compacted to an S3 object on completion
  (**log_archives** records where it went).
- **secrets**: name, scope (org/repo/env), KMS-wrapped ciphertext, version
- **runners**: identity, tags, last_heartbeat, capacity
- **tokens**: hashed, scoped claims, expiry (job tokens, MCP tokens, API tokens)
- **audit_events**: principal, surface (web/chat/mcp/api), action, subject, payload_hash, ts

## 3. Job queue (Postgres)

- Enqueue happens **in the same transaction** as run/job creation (no dual-write).
- Claim: `UPDATE jobs SET status='claimed', claimed_by=$runner, claimed_at=now() WHERE id IN (SELECT id FROM jobs WHERE status='queued' AND tags <@ $runner_tags ORDER BY priority, created_at FOR UPDATE SKIP LOCKED LIMIT $n) RETURNING *;`
- `LISTEN/NOTIFY` wakes the dispatcher; a periodic sweep (visibility timeout on `claimed_at` + missing heartbeats) requeues orphaned jobs.
- Retry with capped exponential backoff; `attempts` counter; terminal → dead-letter status with reason.
- Queue is behind a Go interface (`Queue`) so NATS/SQS become drivers post-MVP. Migration trigger: sustained thousands of jobs/sec or vacuum pressure degrading p99.

## 4. Runner protocol (see ADR-004)

- Runner dials **outbound** gRPC over TLS to the server; no inbound ports on runners (NAT/firewall friendly — matches GitHub/GitLab/Buildkite direction).
- One outbound TLS connection carries two gRPC streams: **Session** (job assignment ↓, ack/status/heartbeat ↑, cancellation ↓) and **LogStream** (log chunks + structured events ↑, watermark acks ↓) — separated so log bursts can't head-of-line-block control messages. Full design: 04-runner-protocol.md.
- Registration: one-time registration token → runner identity + credentials; JIT/ephemeral runner mode planned post-MVP.
- Reconnect semantics: runner resumes in-flight jobs after transient disconnect (server holds claim through visibility timeout); idempotent status upserts keyed by (job, seq).
- Protobuf versioned from day 1; server supports N-1 runner versions.

## 5. Executors

`Executor` interface: `Prepare(ctx, job) / RunStep(ctx, step) → (exit, events) / Cleanup(ctx)`

- **docker** (default): fresh container per step, shared workspace volume per job, network default-off unless step requests it, resource limits from job spec.
- **shell**: direct host execution (trusted infra jobs only; poisonable shared state — documented loudly).
- Post-MVP drivers: kubernetes, firecracker/gvisor (untrusted tier). The interface is the portability seam — get it right in M1.

## 6. Workflow definition (v1)

YAML with a published JSON Schema (the schema doubles as the agent-facing contract for `validate_workflow`).

```yaml
name: deploy-api
on:
  push: { branches: [main] }
jobs:
  test:
    runs-on: [linux, docker]
    steps:
      - uses: checkout            # built-in step, not a marketplace fetch
      - run: ./scripts/test.sh
        image: golang:1.24
  deploy:
    needs: [test]
    approval: { channel: "#deploys", allow: [team:platform] }
    steps:
      - run: aws s3 sync ./dist s3://my-bucket
        image: amazon/aws-cli
        identity: aws            # ← OIDC role assumption, no keys
        env: { AWS_ROLE_ARN: secrets.DEPLOY_ROLE }
```

Deliberate constraints: no loops, no expressions beyond status/branch conditionals, no templating language. Logic goes in scripts/containers (Jenkins Groovy lesson). Reuse arrives post-MVP as versioned packages, not YAML anchors.

## 7. Security baseline (day one)

- **No stored cloud keys**: server is an OIDC issuer; jobs exchange identity JWTs for STS creds via `AssumeRoleWithWebIdentity` (trust conditions on `sub` = org/repo/branch/env, `aud`).
- **Secret redaction on log ingest**: all secret values + common encodings (base64, URL) scrubbed before persist/fan-out. Adversarial tests in CI. (CVE-2025-30066 class: exfil via logs needs no network.)
- **Per-job ephemeral tokens** scoped to run/repo; GitHub installation tokens minted per job, never persisted.
- **Fork-PR isolation**: no secrets, restricted runner tag, no deploy identities.
- **Pinning**: step images referenced by digest where provided; plugin execution (post-MVP) requires digest + cosign verification.
- **Audit everything**: every state change from any surface (web/chat/MCP/API) → audit_events with principal.
- **Server process hygiene**: secrets decrypted only in the job-dispatch path; never in server env or logs.

## 8. Log pipeline

runner → gRPC chunk ↑ → redactor → Postgres (hot) → SSE fan-out to browsers (Last-Event-ID resume)
                                          └→ on completion: compact → S3 (cold), Postgres rows pruned
Structured events (JSON: phase, exit, error class) flow in parallel with raw text and feed the UI, `classify_failure`, and MCP tools.

## 9. AWS reference deployment (MVP Terraform module)

| Concern | Choice | Note |
|---|---|---|
| Control plane | ECS Fargate + ALB | Stateless; fine on Fargate |
| DB | RDS Postgres | Queue + metadata; multi-AZ optional |
| Blobs | S3 | Logs, artifacts; lifecycle policies |
| Secrets at rest | KMS envelope (data keys) | Secrets Manager for platform's own creds |
| Runners | EC2 ASG + Docker | **Fargate can't run runners that build images** (no privileged/DinD); EC2 keeps options open. kaniko/BuildKit-rootless documented for Fargate-only shops |
| Job cloud identity | IAM OIDC provider → roles | Trust conditions per repo/branch/env |

Portability seams (interfaces with AWS as first driver): `BlobStore`, `SecretStore`, `Queue`, `Forge`, `ChatProvider`, `Executor`.

## 10. Surfaces

- **Web UI**: server-rendered, no build step (ADR-003 / 03-frontend-approach.md)
- **Slack**: Socket Mode default (no ingress needed — self-hosting friendly) or Events API; Block Kit approvals with mandatory acting-user authz; edit-in-place threaded status
- **MCP**: read-only tool set in MVP (`get_failed_step_logs`, `classify_failure`, `diff_against_last_green`, `validate_workflow`, …), scoped tokens, per-invocation audit
- **CLI** (thin, post-M2): `validate`, `run`, `logs -f` — same API
