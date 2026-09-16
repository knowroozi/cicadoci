# CICaDo Runner Protocol — Design v1

Status: **v1 frozen** (2026-09-16, pending implementation feedback only) · Refines ADR-004 · Companion proto: [proto/runner_v1.proto](proto/runner_v1.proto)

This is the hardest-to-change contract in the system. Nothing in this doc is "obvious enough to skip writing down."

## 1. Goals & non-goals

**Goals**
- Runners work behind NAT/firewalls with zero inbound ports: every connection is dialed **outbound** by the runner.
- Survive restarts on both sides without losing job state or duplicating side effects at the protocol layer.
- One TLS connection per runner carrying everything (control + logs), so operators firewall exactly one egress: `server:443`.
- Explicit versioning with a documented compatibility window (server supports runner N-1).
- Secrets never sit in redeliverable/journaled messages.

**Non-goals**
- Exactly-once *execution*. The protocol guarantees at-most-once execution **per lease** and exactly-once *recording* (idempotent uploads). A network-partitioned runner whose lease expires may race its replacement until it reconnects and is told to abandon — jobs should tolerate re-runs (documented user-facing).
- Server-initiated connections, peer-to-peer runner traffic, or runner-hosted APIs.

## 2. Transport & connection topology

- **gRPC over TLS (HTTP/2)**, runner → server. Runner credential sent as call metadata on every RPC.
- One connection, **two long-lived streams + a few unary RPCs**:

| Channel | Kind | Purpose |
|---|---|---|
| `Session` | bidi stream | Control: hello/resume, job assignment, acks, status, heartbeat, cancel, throttle |
| `LogStream` | bidi stream | Bulk: log chunks + structured events ↑, watermark acks ↓ |
| `Register` | unary | One-time enrollment with a registration token |
| `GetJobSecrets` | unary | Secret material fetch at job start (never journaled) |
| `RotateCredential` | unary | Runner credential rotation |

**Why logs are NOT on the Session stream** (delta from the ADR-004 sketch): a gRPC stream is a single HTTP/2 stream with its own flow-control window; a log burst would head-of-line-block cancellation and heartbeats behind megabytes of buffered chunks. Separating the lanes keeps control latency bounded regardless of log volume, while still sharing the one TCP+TLS connection. ADR-004 carries an amendment noting this.

## 3. Identity & enrollment

1. Operator mints a **registration token** server-side: expiry, max uses, and **pre-bound labels** (the token dictates the runner's tags — a stolen token can't self-assign `prod-deploy`).
2. Runner calls `Register(registration_token, name, labels, capabilities{executors, os, arch, runner_version})`.
3. Server returns `runner_id` + an opaque **runner credential** (bearer; stored hashed server-side, plaintext only on the runner's disk, `0600`).
4. Credential is rotatable (`RotateCredential`) and revocable server-side; revocation kills the session on next message.

## 4. Session lifecycle

```
runner                                   server
  │── Session open, msg 1: Hello ──────────▶│  protocol_version, runner_version,
  │                                         │  resume: [{job_id, lease_id,
  │                                         │            last_status_seq, last_log_seq}]
  │◀────────────── ServerHello ─────────────│  session_id, heartbeat_interval,
  │                                         │  limits{chunk bytes, rates}, min/latest version,
  │                                         │  per-job resume verdicts: RESUME | ABANDON
  │◀─ JobAssignment ────  ...steady state...│
  │── JobAck / StatusUpdate / Heartbeat ───▶│
```

- **Hello is always the first client message**; anything else on a fresh stream is a protocol error (stream closed with `FAILED_PRECONDITION`).
- `ServerHello` is authoritative for heartbeat cadence and limits — runners never hardcode them.
- Version negotiation: server rejects `protocol_version` below its floor with a typed error carrying `min_supported`; runner surfaces this loudly (it's an operator action, not a retry).

## 5. Job assignment & leases

- Server → `JobAssignment{job_id, lease_id, lease_expires_at, spec}`. `spec` contains steps, images (digest-pinned when available), env *and secret references* — **no secret values**.
- Runner → `JobAck{job_id, lease_id, ACCEPTED | REJECTED{reason}}` (reject: capacity, missing executor, label mismatch — server requeues, records reason).
- **Lease = the single source of execution authority.** Heartbeats carrying `active_job_ids` renew all listed leases. Lease TTL = 3 × heartbeat interval (floor 60s). The server-side sweeper requeues jobs whose lease expired.
- Assignments are **redeliverable**; runner dedupes on `(job_id, lease_id)`. Same job with a *newer* lease_id after an abandon verdict is a legitimate re-run.
- At `PREPARING`, runner calls `GetJobSecrets(job_id, lease_id)`; server authorizes against the live lease. Secret values live only in runner memory for the job's duration and are registered in the runner's local redaction masker (defense in depth; the server-side ingest redactor per 02-architecture §8 remains the source of truth).

## 6. Status & idempotency

- `StatusUpdate{job_id, seq, state, detail}` with **seq monotonically increasing per job**, assigned by the runner. States: `PREPARING → RUNNING_STEP{index} → UPLOADING → {SUCCEEDED{exit}, FAILED{exit, error_class}, CANCELLED}`.
- Server upserts keyed `(job_id, seq)`; duplicates are no-ops; out-of-order arrivals below the high-water mark are discarded. Server acks `StatusAck{job_id, acked_seq}` so the runner trims its journal.
- Terminal states are **sticky**: the server never accepts a transition out of a terminal state for a given lease (protects against zombie duplicates).
- Server clock is authoritative for displayed timestamps; runner timestamps are advisory (skew tolerated, ordering comes from `seq`, never wall time).

## 7. Logs & structured events

- `LogChunk{job_id, step_index, io (STDOUT|STDERR), seq, bytes, ts}` — chunk size ≤ `ServerHello.max_chunk_bytes` (default 64 KiB); runner batches lines up to chunk size or 200 ms, whichever first.
- `StructuredEvent{job_id, step_index, seq, kind, json}` shares the lane and the seq space (kinds: step_started, step_finished, error_classified, artifact_uploaded…). This is the substrate for the UI, `classify_failure`, and MCP.
- Server replies with **watermarks**: `LogAck{job_id, acked_seq}`. Runner buffers unacked chunks (memory, spilling to a bounded disk journal). On overflow: **drop oldest log chunks and insert a gap marker event; never drop StructuredEvents or StatusUpdates.**
- Resume: after reconnect, runner restarts each job's log stream from `last acked watermark + 1`.
- Server may send `Throttle{job_id?, bytes_per_sec}`; runner complies (backpressure beats OOM).

## 8. Cancellation

- Server → `CancelJob{job_id, lease_id, reason, grace_seconds}` on the Session stream (never queued behind logs — see §2).
- Runner: signal step process/container → wait grace → SIGKILL/container rm → final `CANCELLED` status. Cancel for an unknown/finished job → `StatusUpdate` replay of the terminal state (idempotent).
- Cancel issued while a runner is disconnected is delivered as an `ABANDON` verdict or explicit Cancel on resume — whichever the reconnect finds first; both converge.

## 9. Reconnect & restart matrix (adversarial review)

| Scenario | Behavior |
|---|---|
| Server restarts mid-job | Runner's streams break; jittered exp backoff (1s → 60s cap) reconnect; Hello lists in-flight jobs; leases were persisted in Postgres so verdict = RESUME; uploads resume from watermarks. |
| Runner process crashes mid-job | Local containers orphaned → runner startup does workspace/container reconciliation (labels every container with job_id; kills unknowns). Lease expires server-side → job requeued. Journal replays any unacked terminal status from before the crash (dedup makes this safe). |
| Network partition < lease TTL | Invisible: reconnect, RESUME, watermark replay. |
| Partition > lease TTL (split brain) | Server requeued the job (new lease). Old runner reconnects → verdict ABANDON → kills local execution, discards journal for that job. Its uploads after requeue were already rejected (stale lease_id). This is the documented at-most-once-per-lease boundary. |
| Duplicate assignment delivery | Dedup on (job_id, lease_id) → single execution, re-ACK. |
| Log burst starving control | Impossible by construction: separate streams (§2) + Throttle. |
| Runner accepts then immediately dies | No heartbeat → lease expiry → requeue. Assignment is not execution; only leases count. |
| Credential revoked mid-session | Next message fails `UNAUTHENTICATED`; runner does NOT retry-loop; kills jobs after grace; requires operator re-enrollment. |

## 10. Versioning policy

- Proto package `cicado.runner.v1`. Additive changes only within v1: new fields with new tags, `reserved` on any removal, unknown fields tolerated both directions. Behavior changes ride **capability flags** exchanged in Hello/ServerHello, never inferred from version strings.
- Breaking change ⇒ `cicado.runner.v2` served side-by-side for a published deprecation window. Server floor policy: current + previous minor release of the runner (N-1); below floor ⇒ typed rejection + `UpdateAvailable` guidance.

## 11. Resolved decisions (2026-09-16)

1. **No HTTP/1.1 fallback in v1.** gRPC-only; the documented requirement is clean HTTP/2 egress to `:443`. A Connect/gRPC-Web fallback is added only on demonstrated demand from a real deployment.
2. **Secrets: values-at-start.** `GetJobSecrets` returns all job secret values at `PREPARING`. Per-step handles are revisited with the untrusted-jobs tier (Phase 1).
3. **Journal disk budget: 256 MiB default, operator-tunable** via runner flag (`--journal-max-bytes`) from day one; server-advertised `Limits.journal_disk_bytes` acts as an upper bound.
4. **Host metrics ship in v1** as the optional `HostStats` field on heartbeats, feeding the runner-fleet page.
