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

## UTC normalization (CIC-205 follow-up, PR #153 or similar)

`hashEntryV2` now normalises the timestamp to UTC before hashing:
`Timestamp: e.Timestamp.UTC()`. This makes the v2 hash invariant to the
local timezone of the machine that computes it. A Postgres row written
in one TZ and read back in a different TZ verifies the same chain.

`hashEntry` (v1) is unchanged — it continues to hash `e.Timestamp`
without normalisation. Old rows (hash_version=1) verify via v1 and are
untouched.

Test: `TestHashEntryV2UTCInvariance` in `server/audit/audit_test.go`:
same `Entry` timestamped in `time.UTC` and `time.LoadLocation("America/Los_Angeles")`
produces the same v2 hash.

## PostgresLog.Append fix (CIC-205 follow-up, PR #153 r2)

Three defects in the original `PostgresLog.Append` were fixed:

1. **Column/value count mismatch**: the `INSERT INTO audit_events`
   listed 10 columns but passed 11 values (a trailing literal
   `2` that was meant to be `hash_version`). Fixed by adding
   `hash_version` to the column list.

2. **v1 hash for v2 rows**: `e.Hash` was computed with `hashEntry`
   (v1) but `hash_version` was stored as `2`. Fixed by moving the
   PrincipalType/Surface defaults before the hash and calling
   `hashForRow(e)` which dispatches on `e.HashVersion`.

3. **PrincipalType/Surface defaults after hash**: the defaults
   (`e.PrincipalType = "user"` etc.) were applied after the hash
   was computed, so `hashEntryV2` saw empty values. Fixed by moving
   the defaults before the hash computation.

After these fixes, `PostgresLog.Append` stores hash_version as a real
column and `hashEntryV2` is used for new rows. `VerifyChain`
dispatches via `hashForRow`, so old v1 rows still verify correctly.
