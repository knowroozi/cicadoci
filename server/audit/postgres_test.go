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
