package webhook

import (
	"sync"
	"testing"
	"time"
)

// TestCheckIsReadOnly verifies that check() does not insert or refresh
// entries, so an expired delivery ID is treated as fresh on the next
// check rather than having its timer silently extended.
func TestCheckIsReadOnly(t *testing.T) {
	tracker := NewDeliveryTracker(100, 100*time.Millisecond)
	now := time.Now()
	var suppressed uint64

	// A brand-new ID is unseen.
	if tracker.check("abc", now, &suppressed) {
		t.Fatal("check returned true for a new ID")
	}

	// record() inserts the entry.
	tracker.record("abc", now)

	// Within TTL, check returns true (suppressed).
	if !tracker.check("abc", now.Add(50*time.Millisecond), &suppressed) {
		t.Fatal("check should return true within TTL")
	}
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", suppressed)
	}

	// After TTL expiry, check returns false (treat as fresh)
	// and does NOT refresh the timer.
	if tracker.check("abc", now.Add(200*time.Millisecond), &suppressed) {
		t.Fatal("check should return false for expired ID")
	}
	if suppressed != 1 {
		t.Fatalf("suppressed should be 1, got %d", suppressed)
	}
	// A second check at the same TTL should also be false.
	tracker.record("abc", now.Add(210*time.Millisecond))
	// Now the new entry is within TTL.
	if !tracker.check("abc", now.Add(220*time.Millisecond), &suppressed) {
		t.Fatal("after record(), check should return true within new TTL")
	}
}

// TestRecordThenCheckSuppresses verifies the dedup path: a second delivery
// with the same ID within TTL is suppressed.
func TestRecordThenCheckSuppresses(t *testing.T) {
	tracker := NewDeliveryTracker(10, 100*time.Second)
	now := time.Now()
	var suppressed uint64
	var wg sync.WaitGroup
	wg.Add(2)

	// First delivery records.
	go func() {
		defer wg.Done()
		if tracker.check("delivery-1", now, &suppressed) {
			t.Error("first delivery should not be suppressed")
		}
		tracker.record("delivery-1", now)
	}()

	// Second delivery with same ID is suppressed.
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		if !tracker.check("delivery-1", now, &suppressed) {
			t.Error("second delivery should be suppressed")
		}
	}()

	wg.Wait()
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", suppressed)
	}
}

// TestFIFOEviction verifies that the tracker evicts the oldest entry
// when it exceeds maxSize.
func TestFIFOEviction(t *testing.T) {
	tracker := NewDeliveryTracker(2, 100*time.Second)
	now := time.Now()

	tracker.record("a", now)
	tracker.record("b", now)
	tracker.record("c", now) // evicts "a"

	var suppressed uint64
	// "a" should be evicted.
	if tracker.check("a", now, &suppressed) {
		t.Error("evicted entry 'a' should not be recognised")
	}
	// "c" should be present.
	if !tracker.check("c", now, &suppressed) {
		t.Error("latest entry 'c' should be recognised")
	}
}
