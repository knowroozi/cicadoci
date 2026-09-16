# ADR-004: Outbound-only gRPC streaming runner protocol

Status: Accepted · 2026-09-07

## Context
Runners live behind NAT/firewalls in customer networks. Inbound connections to runners are a non-starter. This protocol is the hardest-to-change contract in the system.

## Decision
Runner dials outbound gRPC over TLS; one bidirectional stream multiplexes job assignment, acks/status/heartbeats, log chunks, and cancellation. Protobuf versioned from day 1; server supports N-1 runner versions. Registration via one-time token.

## Rationale
- Matches the industry direction (GitHub Actions long-poll, GitLab Runner poll, Buildkite agent, Jenkins inbound-agent) — outbound-only is the proven NAT-friendly shape.
- gRPC streaming gives multiplexing + backpressure + typed contracts on one TLS connection.

## Consequences
- Must design reconnect/resume semantics (claims held through visibility timeout; idempotent status upserts).
- Open question: HTTP/1.1 long-poll fallback for gRPC-hostile proxies (tracked in plan §8).

## Amendment — 2026-09-16

Design detail refined in 04-runner-protocol.md: control (Session) and bulk logs
(LogStream) are SEPARATE gRPC streams sharing the one outbound TLS connection,
so log bursts can never head-of-line-block cancellation/heartbeats within a
single HTTP/2 stream's flow-control window. The "one bidirectional stream
multiplexes ... log chunks" wording above is superseded to that extent.
