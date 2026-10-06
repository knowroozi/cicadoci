// Copyright 2024 Kaveh Nowroozi. Apache-2.0.
//
// Test suite for PostgresLog against a real Postgres instance.
//
// These tests skip (not fail) when CICADO_TEST_DATABASE_URL is unset,
// matching the convention of every other Postgres-backed test in this
// repo (queue, store, runner, etc.).
//
// The original PR #73 shipped without a test; the reviewer proved that
// this is why two independent bugs (jsonb_build_object type inference +
// ID-in-hash mismatch) went undetected. These tests cover both issues
// and must run green.
package audit_test

import (
	"context"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/audit"
)

// TestPostgresAppendVerifyChain exercises the Append -> VerifyChain
// round-trip against a real database. This directly reproduces
// the blocker conditions the reviewer identified.
func TestPostgresAppendVerifyChain(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)

	log := audit.NewPostgresLog(db.Pool())

	// Genesis: first entry, prev_hash = genesisHash.
	id1, err := log.Append(ctx, audit.Entry{
		Action: audit.ActionApprove,
		Actor:  "alice",
		Target: "run-42",
		Reason: "first entry",
	})
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if id1 != 1 {
		t.Fatalf("append first: got id=%d, want 1", id1)
	}

	// Second entry, chained off the first.
	id2, err := log.Append(ctx, audit.Entry{
		Action: audit.ActionCancel,
		Actor:  "bob",
		Target: "run-42",
		Reason: "second entry",
	})
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if id2 != 2 {
		t.Fatalf("append second: got id=%d, want 2", id2)
	}

	// VerifyChain must pass on a clean chain.
	if err := log.VerifyChain(ctx); err != nil {
		t.Fatalf("VerifyChain after 2 appends: %v", err)
	}

	// Spot-check Get.
	e1, err := log.Get(ctx, 1)
	if err != nil {
		t.Fatalf("get id 1: %v", err)
	}
	if e1.Action != audit.ActionApprove {
		t.Errorf("e1.Action = %s, want %s", e1.Action, audit.ActionApprove)
	}
	if e1.Actor != "alice" {
		t.Errorf("e1.Actor = %q, want alice", e1.Actor)
	}
	if e1.Hash == "" {
		t.Error("e1.Hash is empty")
	}
	if e1.PrevHash != "0000000000000000000000000000000000000000000000000000000000000000" {
		t.Errorf("e1.PrevHash = %s, want genesisHash", e1.PrevHash)
	}

	// List returns both entries.
	entries, err := log.List(ctx, 0, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("list returned %d entries, want 2", len(entries))
	}
	if entries[0].ID != 1 {
		t.Errorf("entries[0].ID = %d, want 1", entries[0].ID)
	}

	// e2.PrevHash must equal e1.Hash (chain integrity).
	e2, err := log.Get(ctx, 2)
	if err != nil {
		t.Fatalf("get id 2: %v", err)
	}
	if e2.PrevHash != e1.Hash {
		t.Errorf("e2.PrevHash = %s, want e1.Hash = %s",
			e2.PrevHash, e1.Hash)
	}
}

// TestPostgresTamperDetected overwrites a row's payload_hash and
// confirms that VerifyChain detects the tamper.
func TestPostgresTamperDetected(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	log := audit.NewPostgresLog(db.Pool())

	// Two clean entries.
	if _, err := log.Append(ctx, audit.Entry{
		Action: audit.ActionApprove, Actor: "alice",
		Target: "run-1", Reason: "ok",
	}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if _, err := log.Append(ctx, audit.Entry{
		Action: audit.ActionApprove, Actor: "bob",
		Target: "run-2", Reason: "ok",
	}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	// Verify clean chain.
	if err := log.VerifyChain(ctx); err != nil {
		t.Fatalf("clean chain: %v", err)
	}

	// Tamper entry 0's payload_hash.
	_, err := db.Pool().Exec(ctx,
		`UPDATE audit_events
			SET payload_hash = $1
			WHERE id = 1`,
		[]byte{0, 0, 0, 0, 0, 0, 0, 0})
	if err != nil {
		t.Fatalf("tamper update: %v", err)
	}

	// VerifyChain must fail.
	if err := log.VerifyChain(ctx); err == nil {
		t.Fatal("VerifyChain did not fail after tampering with entry 0")
	}
}

// TestPostgresGetMissingID confirms that a non-existent id returns an
// error (not a zero-value Entry).
func TestPostgresGetMissingID(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	log := audit.NewPostgresLog(db.Pool())

	_, err := log.Get(ctx, 999999)
	if err == nil {
		t.Fatal("Get(missing) returned nil error; want pgx.ErrNoRows")
	}
}

// TestPostgresEmptyChain confirms that an empty table verifies cleanly
// without any entries.
func TestPostgresEmptyChain(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	log := audit.NewPostgresLog(db.Pool())

	if err := log.VerifyChain(ctx); err != nil {
		t.Fatalf("empty chain: %v", err)
	}
}

// TestPostgresPrincipalFidelity pins the CIC-192 fix: a stored row must carry
// the Entry's real principal_type, surface, and org_id, not the historical
// hardcoded 'user'/'system' literals. Regression: a system-initiated event
// (e.g. a gate timeout) was stored as principal_type='user', principal_id='system'.
func TestPostgresPrincipalFidelity(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	log := audit.NewPostgresLog(db.Pool())

	// Seed a real org: org_id is a foreign key into orgs, so a non-empty
	// OrgID must reference an existing org. The empty-OrgID case below still
	// exercises the NULL path (NULLIF -> NULL) without a seed.
	var org string
	if err := db.Pool().QueryRow(ctx,
		`INSERT INTO orgs (slug, name, created_at, updated_at)
		VALUES ($1, $2, now(), now()) ON CONFLICT (slug) DO UPDATE SET updated_at = now() RETURNING id`,
		"fidelity-org", "Fidelity Org").Scan(&org); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	// A system-initiated event must NOT be stored as user/system.
	id, err := log.Append(ctx, audit.Entry{
		Action:        audit.ActionGateTimeout,
		Actor:         "gate-controller",
		Target:        "run-42",
		Reason:        "approval gate timed out",
		PrincipalType: "system",
		Surface:       "chat",
		OrgID:         org,
	})
	if err != nil {
		t.Fatalf("append system entry: %v", err)
	}
	got, err := log.Get(ctx, id)
	if err != nil {
		t.Fatalf("get system entry: %v", err)
	}
	if got.Action != audit.ActionGateTimeout {
		t.Errorf("action = %q; want %q", got.Action, audit.ActionGateTimeout)
	}
	if got.PrincipalType != "system" {
		t.Errorf("principal_type = %q; want %q (CIC-192 bug: was 'user')", got.PrincipalType, "system")
	}
	if got.Surface != "chat" {
		t.Errorf("surface = %q; want %q (CIC-192 bug: was 'system')", got.Surface, "chat")
	}
	if got.Actor != "gate-controller" {
		t.Errorf("principal_id = %q; want gate-controller", got.Actor)
	}
	if got.OrgID != org {
		t.Errorf("org_id = %q; want %q", got.OrgID, org)
	}

	// A runner principal surfaces distinctly from a user.
	id2, err := log.Append(ctx, audit.Entry{
		Action:        audit.ActionCreateRun,
		Actor:         "runner:7",
		Target:        "job-9",
		Reason:        "job started",
		PrincipalType: "runner",
		Surface:       "mcp",
	})
	if err != nil {
		t.Fatalf("append runner entry: %v", err)
	}
	got2, err := log.Get(ctx, id2)
	if err != nil {
		t.Fatalf("get runner entry: %v", err)
	}
	if got2.PrincipalType != "runner" {
		t.Errorf("principal_type = %q; want runner", got2.PrincipalType)
	}
	if got2.Surface != "mcp" {
		t.Errorf("surface = %q; want mcp", got2.Surface)
	}
	if got2.OrgID != "" {
		t.Errorf("org_id = %q; want empty (stored as NULL)", got2.OrgID)
	}

	// An unset principal defaults to the historical literals.
	id3, err := log.Append(ctx, audit.Entry{
		Action: audit.ActionApprove,
		Actor:  "alice",
		Target: "job-1",
		Reason: "no principal set",
	})
	if err != nil {
		t.Fatalf("append default entry: %v", err)
	}
	got3, err := log.Get(ctx, id3)
	if err != nil {
		t.Fatalf("get default entry: %v", err)
	}
	if got3.PrincipalType != "user" {
		t.Errorf("default principal_type = %q; want user", got3.PrincipalType)
	}
	if got3.Surface != "system" {
		t.Errorf("default surface = %q; want system", got3.Surface)
	}

	// The hash chain must still verify: hashEntry is unchanged (stable), so the
	// new fields do not affect the chain.
	if err := log.VerifyChain(ctx); err != nil {
		t.Errorf("verify chain after fidelity inserts: %v", err)
	}
}

// TestPostgresPrincipalViaList confirms the new fields also round-trip through
// the List read path, not just Get.
func TestPostgresPrincipalViaList(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	log := audit.NewPostgresLog(db.Pool())

	_, err := log.Append(ctx, audit.Entry{
		Action:        audit.ActionGateTimeout,
		Actor:         "gate-controller",
		Target:        "run-42",
		Reason:        "resolved via chat",
		PrincipalType: "agent",
		Surface:       "chat",
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	entries, err := log.List(ctx, 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *audit.Entry
	for i := range entries {
		if entries[i].Reason == "resolved via chat" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("appended entry not found in List result")
	}
	if found.Surface != "chat" {
		t.Errorf("List surface = %q; want chat", found.Surface)
	}
	if found.PrincipalType != "agent" {
		t.Errorf("List principal_type = %q; want agent", found.PrincipalType)
	}
	if found.Actor != "gate-controller" {
		t.Errorf("List principal_id = %q; want gate-controller", found.Actor)
	}
}
