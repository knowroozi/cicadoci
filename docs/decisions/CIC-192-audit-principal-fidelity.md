## CIC-192 — Audit-event principal fidelity: real principal_type, surface, org_id

**Date:** 2026-10-05

### Situation

`audit.PostgresLog.Append` wrote every new audit row with the literal
`VALUES (NULL, 'user', $1, 'system', …)` — i.e. `org_id` always NULL,
`principal_type` always `'user'`, and `surface` always `'system'` — ignoring
`e.PrincipalType` / `e.Surface` / `e.OrgID`. As a result, the audit trail could
not tell a system event from a human one, could not tell which surface it came
from, and could not filter by tenant — directly contradicting the invariant "every
state-changing action writes an audited event *with its principal*".

### What this PR adds

1. `audit.Entry` gains three fields — `PrincipalType`, `OrgID`, `Surface` —
   with documentation tying each to its `0001_init.sql` CHECK set
   (`principal_type IN ('user','runner','agent','token','system')`,
   `surface IN ('web','chat','mcp','api','forge','system')`).
2. `PostgresLog.Append` binds all three (`org_id` via
   `NULLIF($1,'')::uuid` so a blank org stores NULL, not a broken cast),
   and `Get`/`List` read them back.
3. The callers are wired to pass meaningful values:
   - `gateAuditEntry` (cmd main.go): approve/deny → `(user, chat)`; timeout →
     `(system, system)`.
   - `emitAudit` (controller): `OrgID` from the repo's `orgID`, `Surface`
     `forge`, `PrincipalType` `user` for push/PR events, `system` for the
     automated rerequest source.
   - `jobtoken.AuditLog.Issue/Use/Revoke`: `(system/system)`,
     `(token/system)`, `(system/system)`.
4. Tests assert the principal fidelity per surface and the stored `org_id`.

### Decision: hash kept stable

`hashEntry` continues to hash **`Action, Actor, Target, Reason, Timestamp,
PrevHash` only.** The three new fields are **not** included in the hash.

This is deliberate and follows the CIC-75 precedent of excluding a field
(`ID`) that cannot be known at hash time. Including the new fields would have
rehashed every historical entry on the first post-migration row, breaking
`VerifyChain` for the entire existing table. Keeping `hashEntry` stable means
the hash chain stays intact for old and new rows alike.

**The trade-off: `principal_type`, `surface`, and `org_id` are stored but
NOT tamper-evident.** A motivated attacker with DB write access could alter
those three columns without breaking the hash chain. `Action`, `Actor`,
`Target`, `Reason`, and `Timestamp` remain tamper-evident; the new
classification fields do not.

### Follow-up

Tracked as **CIC-205**: add a **versioned hash (v2)** that folds in
`principal_type`, `surface`, and `org_id`, and `VerifyChain` should read a
per-row version field (or detect by presence) to verify old chains with the v1
algorithm and new chains with the v2. CIC-205 also covers the historical-row
backfill of `principal_type`/`surface`/`org_id`. Until then, treat the new
fields as *operational metadata*, not integrity-checked fields.

### Cross-reference

- `docs/decisions/CIC-75-audit-postgres.md` — the `PostgresLog` this PR extends;
   its "`ID` excluded from `hashEntry`" decision is the same stability-first
   principle followed here.
