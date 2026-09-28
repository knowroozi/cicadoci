package audit

import (
	"context"
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
