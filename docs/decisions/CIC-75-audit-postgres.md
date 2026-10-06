## CIC-75 — Audit event pipeline: PostgresLog implementation

**Date:** 2026-09-28 (original: 2026-09-28, updated after review)

### Situation

CIC-41 was marked Done but nothing in the codebase ever called `Append`
(only a doc-comment mention in `server/workflow/types.go`). No Postgres
backed implementation existed. CLAUDE.md's invariant: "Every
state-changing action writes an audit event."

### What this PR adds

1. `server/audit/postgres.go`: `PostgresLog` using `pgxpool.Pool`
   (matching `queue.Postgres` pattern, not `*sql.DB`)
2. `server/audit/postgres_test.go`: four tests exercising Append, Get,
   List, VerifyChain, tamper detection, and empty-chain
3. Fix to `server/audit/audit.go` `hashEntry`: exclude `ID` from the
   hashed fields

### Review-blocking bugs found and fixed

**Blocker 1** — `jsonb_build_object` type inference

Postgres cannot infer the type of `$4` and `$5` in
`jsonb_build_object('reason', $4, 'prev_hash', $5)` under the extended
query protocol used by pgx. Both parameters are Go `string` but
Postgres sees them as untyped. Fix: add explicit `::text` casts:

```sql
jsonb_build_object('reason', $4::text, 'prev_hash', $5::text)
```

Without this, every `Append` call errors out.

**Blocker 2** — `ID` included in hash computation (timing mismatch)

`hashEntry` (in `audit.go`) hashed `Entry.ID` among its fields. But
`ID` is assigned by Postgres (bigserial) — it is unknowable at hash
time. `PostgresLog.Append` set `e.ID = 0` before calling `hashEntry`,
so the stored hash uses `ID=0`. When `Get`/`List`/`VerifyChain`
reconstruct the `Entry` with the real `ID`, `hashEntry` produces a
different hash. The chain was always broken, on the very first entry.

Fix: exclude `ID` from `hashEntry`'s hashed struct. `MemoryLog` still
works because its `VerifyChain` calls `hashEntry` on the same `Entry`
struct it stored — the `ID` was never used to check integrity, only
the chain pointer (`PrevHash`).

### Design notes

**`PostgresLog` uses `pgxpool.Pool`, not `*sql.DB`,** matching
`queue.Postgres`. This means tests use `testdb.New(t).Pool()` — no
need to add `github.com/jackc/pgx/v5/stdlib` to `go.mod`.

`payload_hash bytea` stores the raw 32-byte hash. `metadata jsonb`
stores `{reason, prev_hash}` for convenience queries. The `prev_hash`
metadata field is `hash_{n-1}`; `payload_hash` is `hash_n`. The
`lastPrevHash` method reads `payload_hash` (via
`SELECT encode(payload_hash, 'hex') ... ORDER BY id DESC LIMIT 1`),
not `metadata->>'prev_hash'` — that reads `hash_{n-1}`, one step too
early.

### No call-site wiring in this PR

The call sites are CIC-08's responsibility (when CIC-71 wires
main.go). This PR makes the chokepoint usable; CIC-08 wires the
surfaces through it.

### Follow-up: CIC-192

CIC-192 (audit principal fidelity) extended `PostgresLog` to store a real
`principal_type`, `surface`, and `org_id` rather than the hardcoded defaults,
and -- following this document's stability-first precedent -- deliberately
**kept `hashEntry` unchanged**, so the three new fields are stored but not
tamper-evident. See `docs/decisions/CIC-192-audit-principal-fidelity.md`.
