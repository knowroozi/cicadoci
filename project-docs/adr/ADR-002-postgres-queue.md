# ADR-002: Postgres as the only queue and store for MVP

Status: Accepted · 2026-09-07

## Context
Every extra stateful dependency is a self-hosting tax. Queue needs transactional enqueue with domain writes.

## Decision
Postgres is the sole database AND job queue: FOR UPDATE SKIP LOCKED claiming, LISTEN/NOTIFY doorbell, visibility-timeout sweep for orphaned claims. Queue sits behind a Go interface.

## Rationale
- Eliminates the dual-write problem (enqueue in the same tx as run creation).
- Proven pattern (Oban, Que, graphile-worker).
- One system to operate, back up, and reason about.

## Consequences
- Known ceiling: sustained thousands of jobs/sec or vacuum pressure → introduce NATS/SQS driver behind the interface. Threshold documented in the plan.
- Log hot-path also lands in Postgres; compaction to S3 on completion controls bloat.
