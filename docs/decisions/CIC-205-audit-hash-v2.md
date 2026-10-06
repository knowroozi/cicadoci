# CIC-205: audit hash v2

## Problem

CIC-192 added three new columns to `audit_events`: `org_id`,
`principal_type`, `surface`. These are NOT folded into the v1
`hashEntry`, so they are not tamper-evident.

## Decision

Add a `hash_version` column to `audit_events`:
- `1` = legacy hash (action/actor/target/reason/timestamp/prev_hash)
- `2` = v1 fields + principal_type/surface/org_id

`hashForRow(entry)` dispatches on `entry.HashVersion`:
- `0` or `1`: `hashEntry` (the v1 function)
- `2`: `hashEntryV2` (extends the v1 JSON with 3 additional fields)

### Migration

`0006_audit_hash_v2.sql`:
- `ALTER TABLE audit_events ADD COLUMN hash_version smallint
  NOT NULL DEFAULT 1 CHECK (hash_version IN (1, 2))`

All existing rows default to `1` (legacy, no rehash).
God approved: no backfill of historical rows.

### Go changes

1. `Entry` struct: new `HashVersion int` field (zero = legacy).
2. `hashEntryV2(e)`: v1 JSON + `principal_type`, `surface`, `org_id`.
3. `hashForRow(e)`: dispatch on `HashVersion`.
4. `MemoryLog.Append`: sets `HashVersion = 2` before hashing.
5. `PostgresLog.Append`: sets `HashVersion = 2`, uses `hashForRow`,
   writes `hash_version = 2` in INSERT.
6. `PostgresLog.Get`/`List`: read `coalesce(hash_version, 1)` into
   `Entry.HashVersion`.
7. `PostgresLog.VerifyChain`: reads `hash_version` and `principal_type`,
   `surface`, `org_id` per row; uses `hashForRow` (not `hashEntry`).

### Test coverage

- `TestHashV1ExcludesPrincipalFields`: v1 hash ignores principal fields.
- `TestHashV2IncludesPrincipalFields`: v2 hash changes when any of
  principal_type/surface/org_id changes.
- `TestHashForRowDispatch`: `hashForRow(0)==hashEntry`, `hashForRow(2)==hashEntryV2`.
- `TestMemoryLogAppendWritesV2`: Append sets `HashVersion=2`.
- `TestVerifyChainMixedVersions`: chain of v1 + v2 rows verifies.
- `TestVerifyChainDetectsV2Tamper`: mutating a v2 row's `PrincipalType`
  fails chain verification.

## No schema changes beyond 0006.
