package audit

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewMemoryLogHasGenesis(t *testing.T) {
	ml := NewMemoryLog()
	if ml.Len() != 1 {
		t.Fatalf("expected 1 (genesis) entry, got %d", ml.Len())
	}
	e, err := ml.Get(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != 0 {
		t.Fatalf("expected ID 0, got %d", e.ID)
	}
}

func TestAppendAndGet(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()

	e := Entry{
		Action:    ActionApprove,
		Actor:     "user-42",
		Target:    "run/r123",
		Reason:    "looks good",
		Timestamp: time.Now(),
	}
	id, err := ml.Append(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("expected ID 1, got %d", id)
	}

	got, err := ml.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != ActionApprove {
		t.Fatalf("expected ActionApprove, got %q", got.Action)
	}
	if got.Actor != "user-42" {
		t.Fatalf("expected user-42, got %q", got.Actor)
	}
	if got.Hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if got.PrevHash == "" {
		t.Fatal("expected non-empty prev hash")
	}
}

func TestGetNotFound(t *testing.T) {
	ml := NewMemoryLog()
	_, err := ml.Get(context.Background(), 99)
	if err == nil {
		t.Fatal("expected error for nonexistent entry")
	}
}

func TestList(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()

	// Append 5 entries.
	for i := 0; i < 5; i++ {
		ml.Append(ctx, Entry{Action: ActionRerun, Actor: "u", Target: "r"})
	}

	entries, err := ml.List(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != 3 {
		t.Fatalf("expected first entry ID 3, got %d", entries[0].ID)
	}
	if entries[1].ID != 4 {
		t.Fatalf("expected second entry ID 4, got %d", entries[1].ID)
	}
}

func TestListNoResults(t *testing.T) {
	ml := NewMemoryLog()
	entries, err := ml.List(context.Background(), 99, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

func TestVerifyChainGood(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()

	// Append several entries.
	for i := 0; i < 5; i++ {
		ml.Append(ctx, Entry{Action: ActionDeploy, Actor: "u", Target: "r"})
	}

	if err := ml.VerifyChain(ctx); err != nil {
		t.Fatalf("expected no chain error, got %v", err)
	}
}

func TestVerifyChainBroken(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()

	// Append an entry.
	ml.Append(ctx, Entry{Action: ActionCancel, Actor: "u", Target: "r"})

	// Tamper with the hash.
	ml.mu.Lock()
	ml.entries[1].Hash = "tampered"
	ml.mu.Unlock()

	if err := ml.VerifyChain(ctx); err == nil {
		t.Fatal("expected chain verification to fail after tampering")
	}
}

func TestAppendSetsTimestamp(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()

	// Entry with zero timestamp should be filled in.
	ml.Append(ctx, Entry{Action: ActionApprove, Actor: "u"})

	got, _ := ml.Get(ctx, 1)
	if got.Timestamp.IsZero() {
		t.Fatal("expected timestamp to be set")
	}
}

func TestVerifyChainDetectsPrevHashBreak(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()

	ml.Append(ctx, Entry{Action: ActionApprove, Actor: "u"})
	ml.Append(ctx, Entry{Action: ActionCancel, Actor: "u"})

	// Tamper: break the prev-hash link.
	ml.mu.Lock()
	ml.entries[2].PrevHash = "wrong"
	ml.mu.Unlock()

	if err := ml.VerifyChain(ctx); err == nil {
		t.Fatal("expected chain verification to fail after prev-hash break")
	}
}

func TestCompileTimeInterface(t *testing.T) {
	var _ Log = NewMemoryLog()
}

// TestHashV1ExcludesPrincipalFields verifies that the v1 hash does NOT
// include principal_type, surface, org_id.
func TestHashV1ExcludesPrincipalFields(t *testing.T) {
	e1 := Entry{
		Action:      ActionApprove,
		Actor:       "user-1",
		Target:      "run-1",
		Reason:      "approve",
		Timestamp:   time.Now().Truncate(time.Second),
		PrevHash:    genesisHash,
		HashVersion: 1,
	}
	e2 := e1
	e2.PrincipalType = PrincipalSystem
	e2.Surface = SurfaceChat
	e2.OrgID = "some-org-id"
	// v1 hash: identical (ignores principal fields).
	if hashEntry(e1) != hashEntry(e2) {
		t.Error("v1 hash must NOT depend on principal metadata")
	}
	// v2 hash: must differ (principal fields present).
	if hashEntryV2(e1) == hashEntryV2(e2) {
		t.Error("v2 hash must differ when principal metadata differs")
	}
}

// TestHashV2IncludesPrincipalFields verifies that the v2 hash DOES
// include principal_type, surface, org_id.
func TestHashV2IncludesPrincipalFields(t *testing.T) {
	base := Entry{
		Action:      ActionApprove,
		Actor:       "user-1",
		Target:      "run-1",
		Reason:      "approve",
		Timestamp:   time.Now().Truncate(time.Second),
		PrevHash:    genesisHash,
		HashVersion: 2,
	}
	// Change principal_type only.
	v1 := base
	v1.PrincipalType = PrincipalSystem
	if hashEntryV2(base) == hashEntryV2(v1) {
		t.Error("v2 hash must change when principal_type changes")
	}
	// Change surface only.
	v2 := base
	v2.Surface = SurfaceMCP
	if hashEntryV2(base) == hashEntryV2(v2) {
		t.Error("v2 hash must change when surface changes")
	}
	// Change org_id only.
	v3 := base
	v3.OrgID = "org-123"
	if hashEntryV2(base) == hashEntryV2(v3) {
		t.Error("v2 hash must change when org_id changes")
	}
}

// TestHashForRowDispatch verifies hashForRow routes to the right algo.
func TestHashForRowDispatch(t *testing.T) {
	e := Entry{Action: ActionApprove, Actor: "u", Target: "t",
		Reason: "r", Timestamp: time.Now().Truncate(time.Second), PrevHash: genesisHash}
	if hashForRow(Entry{HashVersion: 0}) != hashEntry(Entry{}) {
		t.Error("v0 must use v1 hash")
	}
	if hashForRow(e) != hashEntry(e) {
		t.Error("v1 must use v1 hash")
	}
	e2 := e
	e2.HashVersion = 2
	if hashForRow(e2) != hashEntryV2(e2) {
		t.Error("v2 must use v2 hash")
	}
}

// TestMemoryLogAppendWritesV2 verifies Append sets HashVersion=2.
func TestMemoryLogAppendWritesV2(t *testing.T) {
	ml := NewMemoryLog()
	ml.Append(context.Background(), Entry{Action: ActionApprove, Actor: "u"})
	got, _ := ml.Get(context.Background(), 1)
	if got.HashVersion != 2 {
		t.Errorf("expected HashVersion=2, got %d", got.HashVersion)
	}
}

// TestVerifyChainMixedVersions simulates a pre-CIC-205 v1 chain, then
// appends a v2 row, and verifies the chain holds across both versions.
func TestVerifyChainMixedVersions(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()

	// Write two v1 rows.
	ml.Append(ctx, Entry{Action: ActionApprove, Actor: "u"})
	ml.Append(ctx, Entry{Action: ActionCancel, Actor: "u"})

	// Downgrade to v1 (pre-CIC-205) and recompute both the hash and
	// the chained PrevHash.
	ml.mu.Lock()
	ml.entries[1].HashVersion = 1
	ml.entries[1].Hash = hashEntry(ml.entries[1])
	// Update entry[2]'s PrevHash to match the new entry[1].Hash.
	ml.entries[2].PrevHash = ml.entries[1].Hash
	ml.entries[2].HashVersion = 1
	ml.entries[2].Hash = hashEntry(ml.entries[2])
	ml.mu.Unlock()

	// Append a v2 row — should chain off the last hash.
	ml.Append(ctx, Entry{
		Action:        ActionApprove,
		Actor:         "user",
		PrincipalType: PrincipalUser,
		Surface:       SurfaceWeb,
	})

	if err := ml.VerifyChain(ctx); err != nil {
		t.Fatalf("mixed v1/v2 chain should verify: %v", err)
	}
}

// TestVerifyChainDetectsV2Tamper verifies that tampering with a
// principal field on a v2 row fails verification.
func TestVerifyChainDetectsV2Tamper(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()
	ml.Append(ctx, Entry{
		Action:        ActionApprove,
		Actor:         "u",
		PrincipalType: PrincipalUser,
		Surface:       SurfaceWeb,
	})
	// Tamper: change PrincipalType after hash was computed.
	ml.mu.Lock()
	ml.entries[1].PrincipalType = PrincipalSystem
	ml.mu.Unlock()
	if err := ml.VerifyChain(ctx); err == nil {
		t.Fatal("expected chain check to fail after v2 principal tamper")
	}
}

// TestHashEntryV2UTCInvariance verifies that hashEntryV2 produces the same
// digest regardless of the local timezone of e.Timestamp.
//
// The v2 hash normalises the timestamp to UTC via .UTC() before json.Marshal,
// so an Entry stored in a local-TZ column and recovered in a different TZ must
// produce the same v2 hash as the same timestamp stored in UTC.
// TestHashEntryV2UTCInvariance verifies that hashEntryV2 produces the same
// digest regardless of the local timezone of e.Timestamp.
//
// The v2 hash normalises the timestamp to UTC via .UTC() before json.Marshal,
// so an Entry stored in a local-TZ column and recovered in a different TZ must
// produce the same v2 hash as the same timestamp stored in UTC.
func TestHashEntryV2UTCInvariance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	// A UTC entry.
	utc := Entry{
		Action:        ActionApprove,
		Actor:         "user-1",
		Target:        "run-1",
		Reason:        "approve",
		Timestamp:     now,
		PrevHash:      genesisHash,
		PrincipalType: PrincipalSystem,
		Surface:       SurfaceMCP,
		OrgID:         "org-42",
		HashVersion:   2,
	}
	// Same instant in a non-UTC local zone via FixedZone (deterministic, no
	// IANA load and no ignored error, per N1/T1).
	la := time.FixedZone("PST", -8*3600)
	local := utc
	local.Timestamp = now.In(la)
	// hashEntryV2 must be identical for both.
	if hashEntryV2(utc) != hashEntryV2(local) {
		t.Fatalf("v2 hash depends on local timezone: utc=%s la=%s; want equal",
			hashEntryV2(utc), hashEntryV2(local))
	}
	// v1 (hashEntry) does NOT normalise, so it must differ, proving the fix is
	// v2-only and scoped.
	if hashEntry(utc) == hashEntry(local) {
		t.Fatal("v1 hash coincidentally matched across timezones; expected a difference")
	}
}

// TestVerifyChainDetectsDowngrade verifies (CIC-211) that once a chain has cut
// over to v2, downgrading a later row back to v1 fails verification. The three
// v2-only fields (principal_type, surface, org_id) are un-hashed by the v1
// formula, so a downgrade that re-hashes the suffix as v1 would otherwise pass;
// the monotonic version guard catches it.
func TestVerifyChainDetectsDowngrade(t *testing.T) {
	ml := NewMemoryLog()
	ctx := context.Background()
	// Two legitimate v2 rows (what Append writes)...
	ml.Append(ctx, Entry{Action: ActionApprove, Actor: "u",
		PrincipalType: PrincipalUser, Surface: SurfaceWeb, OrgID: "org-1"})
	ml.Append(ctx, Entry{Action: ActionCancel, Actor: "u",
		PrincipalType: PrincipalUser, Surface: SurfaceWeb, OrgID: "org-1"})

	// ...then downgrade the second row to v1, re-hashing it as v1 so the only
	// anomaly left is the version drop (the hash still verifies).
	ml.mu.Lock()
	ml.entries[2].HashVersion = 1
	ml.entries[2].Hash = hashEntry(ml.entries[2])
	ml.mu.Unlock()

	err := ml.VerifyChain(ctx)
	if err == nil {
		t.Fatal("expected VerifyChain to reject a v2-to-v1 downgrade")
	}
	if !strings.Contains(err.Error(), "hash version downgrade") {
		t.Fatalf("expected the monotonic guard to reject the downgrade, got a different error: %v", err)
	}
}
