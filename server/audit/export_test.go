// Copyright 2024 Kaveh Nowroozi. Apache-2.0.

package audit

// The following aliases are exported for white-box use from the external
// (package audit_test) tests. The CIC-211 Postgres downgrade test must
// RECOMPUTE the v1 hash of a stored row and overwrite its payload_hash so that
// the plain hash check passes and ONLY the monotonic version guard can reject
// the downgrade. That requires access to the unexported hashEntry from the
// black-box side.
var (
	// HashEntry is the legacy v1 hash (action/actor/target/reason/
	// timestamp/prev_hash; no principal_type/surface/org_id).
	HashEntry = hashEntry
	// HashEntryV2 is the v2 hash (v1 fields + principal_type/surface/org_id,
	// timestamp normalised to UTC).
	HashEntryV2 = hashEntryV2
)
