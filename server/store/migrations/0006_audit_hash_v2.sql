-- +goose Up
-- +goose StatementBegin

-- CIC-205: audit_events hash v2
--
-- The hash chain was computed with hashEntry() which excluded
-- principal_type, surface, and org_id (stored columns added in
-- CIC-192). These three are operational metadata, not in the hash.
--
-- This migration adds a `hash_version` column to tag each row
-- with the hash algorithm it was written with:
--   1 = legacy hashAction (action, actor, target, reason, timestamp)
--   2 = full hash including principal_type, surface, org_id
--
-- Until a backfill decision is made, this column defaults to 1
-- for all existing rows and for new writes until the Go code
-- is updated to write version 2.
--
-- DRAFT: the backfill question remains open with God.
-- The backfill strategy (rehash all legacy rows or leave as-is
-- with a version discriminator in VerifyChain) depends on the
-- answer to: do we accept that pre-CIC-192 rows cannot be
-- tamper-evidently checked for principal_type/surface/org_id?
--
-- See docs/decisions/CIC-192-audit-principal-fidelity.md,
-- §"The trade-off" and "Follow-up".

ALTER TABLE audit_events
	ADD COLUMN hash_version smallint NOT NULL DEFAULT 1
		CHECK (hash_version IN (1, 2));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE audit_events
	DROP COLUMN IF EXISTS hash_version;

-- +goose StatementEnd
