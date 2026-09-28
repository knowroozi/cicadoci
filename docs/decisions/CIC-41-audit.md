## CIC-41 — Audit trail: append-only hash chain

**Date:** 2026-09-28 (recovered from lost stack)

**Decision:** New `server/audit/` package — append-only audit event pipeline. Every state-changing
action (human, chat, or agent) is recorded with a `principal`, `source` surface, and payload hash.
The hash chain uses SHA-256 of the previous event's hash + the current event content, making
tamper-evident. `MemoryLog` is the MVP in-memory implementation (unbounded, no persistence — a
Postgres-backed implementation is a follow-up). `Record(ctx, entry)` takes a `HashEntry` that
contains the event metadata; `Chain.Verify()` walks the chain forward and returns the first
tampered entry index if the chain is broken. All 8+ tests pass with `-race`.
