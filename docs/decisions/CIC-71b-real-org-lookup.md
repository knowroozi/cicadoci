# CIC-71b — Real org/repo resolution in the store

**Ticket:** CIC-71 (sub-blocker 2)
**PR:** knowroozi/cic-71b-real-org-lookup
**Date:** 2026-09-28

## Decision

Add `store.ResolveRepo`, `store.UpsertOrg`, and `store.FindOrgBySlug`
to `server/store/runs.go`. Update `controller.resolveRepo` to call
`store.ResolveRepo` instead of using a deterministic SHA-1 pseudo-UUID
for the org ID.

## Rationale

The pseudo-UUID approach from CIC-71a is acceptable for a prototype
but not for production: the backpressure tracker keys off the org ID,
and if two different owner strings produced the same org UUID
(deterministic SHA-1 of the owner string), cross-tenant backpressure
counts would be wrong. The real fix is to upsert orgs in the
`orgs` table and use the DB's `gen_random_uuid()`.

The `installationID` field is now stored on the `repos` row so that
future tickets (CIC-38, CIC-39, CIC-40) can use it to look up
GitHub App installation tokens rather than re-deriving them.

## Alternatives considered

1. Use `orgs.name` as the slug: rejected — `name` is a
     human-readable display field, not a stable identifier. A user
    would rename "Acme Inc." to "Acme Corp" and every org reference
    in the system would break.
2. Keep `FindOrCreateRepo` and add `UpsertOrg` as a separate call:
    rejected — `ResolveRepo` is the single call the controller needs,
    and it's cleaner to have the atomic upsert-then-find in one
    transaction.

## Consequences

- The pre-existing `repos.installation_id` column (added in migration
    `0001_init.sql`) is now written on first contact by `ResolveRepo`; it
    is not yet used for auth (CIC-38/39/40).
- The controller no longer depends on `uuid.NewSHA1` or
     `uuid.NameSpaceDNS` for org identity.
- CIC-71a's stub is fully replaced; the controller's `resolveRepo`
    now routes through the store.

## Open questions

1. What happens when `ResolveRepo` is called with a `owner` that is
     not in the `orgs` table but is a valid GitHub organisation (not
     yet installed)? Should the controller create the org row
     optimistically (current behaviour) or reject the event until
     an admin registers it?
2. Should `installationID` be validated against a stored installation
     record (in CIC-38/39/40) before upserting, to reject events
     from installations the operator has not configured?
