package logs

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/archive"
)

func TestHub_SubscribeAndPublish(t *testing.T) {
	h := NewHub(8)
	jobID := uuid.New()

	s := h.Subscribe(jobID, 0)
	defer h.Unsubscribe(jobID, s)

	e := archive.Entry{Seq: 1, Kind: "chunk", Data: []byte("a")}
	N := h.Publish(jobID, e)
	if N != 1 {
		t.Fatalf("got %d deliveries, want 1", N)
	}

	select {
	case got := <-s.Entries:
		if got.Seq != 1 {
			t.Fatalf("got seq %d, want 1", got.Seq)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for entry")
	}
}

func TestHub_PublishWithNoSubscribers(t *testing.T) {
	h := NewHub(8)
	jobID := uuid.New()
	N := h.Publish(jobID, archive.Entry{Seq: 1})
	if N != 0 {
		t.Fatalf("got %d deliveries, want 0", N)
	}
}

func TestHub_MultipleSubscribers(t *testing.T) {
	h := NewHub(16)
	jobID := uuid.New()
	s1 := h.Subscribe(jobID, 0)
	s2 := h.Subscribe(jobID, 0)
	defer h.Unsubscribe(jobID, s1)
	defer h.Unsubscribe(jobID, s2)

	N := h.Publish(jobID, archive.Entry{Seq: 1, Data: []byte("a")})
	if N != 2 {
		t.Fatalf("got %d deliveries, want 2", N)
	}

	for _, s := range []*Sub{s1, s2} {
		select {
		case got := <-s.Entries:
			if got.Seq != 1 {
				t.Fatalf("got seq %d, want 1", got.Seq)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatal("timeout waiting for entry")
		}
	}
}

func TestHub_DropOnFullBuffer(t *testing.T) {
	h := NewHub(2) // small buffer
	jobID := uuid.New()
	s := h.Subscribe(jobID, 0)
	// Don't drain: fill the buffer and then one more.
	_ = h.Publish(jobID, archive.Entry{Seq: 1})
	_ = h.Publish(jobID, archive.Entry{Seq: 2})
	// The next publish should drop the subscriber since the buffer is full.
	h.Publish(jobID, archive.Entry{Seq: 3})

	select {
	case <-s.Done:
		// Expected: the subscriber was dropped.
	default:
		t.Fatal("expected Done to be closed after drop")
	}
}

func TestHub_UnsubscribeIsIdempotent(t *testing.T) {
	h := NewHub(8)
	jobID := uuid.New()
	s := h.Subscribe(jobID, 0)
	h.Unsubscribe(jobID, s)
	h.Unsubscribe(jobID, s) // second call is a no-op
	// Done should be closed.
	select {
	case <-s.Done:
		// good
	default:
		t.Fatal("Done should be closed after Unsubscribe")
	}
}

// TestHub_DropsDontBlockOtherSubscribers verifies that a drop on one
// subscriber (for backpressure) does not block delivery to other
// subscribers on the same job.
//
// Strategy: subscribe one "slow" sub (never drained) and one "fast"
// sub, each on their own hub (so the two tests don't interfere with
// each other). After dropping the slow sub, a new subscriber on the
// same job still receives entries.
func TestHub_DropsDontBlockOtherSubscribers(t *testing.T) {
	h := NewHub(1) // single-entry buffer
	jobID := uuid.New()

	// Slow sub: fill its buffer so the next publish drops it.
	_ = h.Publish(jobID, archive.Entry{Seq: 1})
	slow := h.Subscribe(jobID, 0)
	// This publish fills the slow sub's buffer; the next publish will
	// drop it.
	_ = h.Publish(jobID, archive.Entry{Seq: 2})

	// Now subscribe a "fast" sub that we drain synchronously.
	fast := h.Subscribe(jobID, 0)
	// Publish an entry that should reach the fast sub.
	_ = h.Publish(jobID, archive.Entry{Seq: 3})

	// The fast sub should have received the entry.
	select {
	case e := <-fast.Entries:
		if e.Seq != 3 {
			t.Fatalf("fast got seq %d, want 3", e.Seq)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("fast did not receive an entry")
	}

	// The slow sub should be dropped (its buffer was full when
	// the 3rd publish happened).
	select {
	case <-slow.Done:
	// expected
	default:
		t.Fatal("slow sub was not dropped despite a full buffer")
	}

	// A fresh subscriber on the same job still works.
	fresh := h.Subscribe(jobID, 0)
	_ = h.Publish(jobID, archive.Entry{Seq: 4})
	select {
	case e := <-fresh.Entries:
		if e.Seq != 4 {
			t.Fatalf("fresh got seq %d, want 4", e.Seq)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("fresh subscriber did not receive an entry")
	}
	_ = fast
	h.Unsubscribe(jobID, fresh)
}

func TestHub_Concurrent(t *testing.T) {
	h := NewHub(8)
	jobID := uuid.New()
	var wg sync.WaitGroup
	// Concurrent publish from multiple goroutines exercises the race
	// detector's coverage of the shared subs map and the non-blocking
	// sends in Publish.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.Publish(jobID, archive.Entry{Seq: int64(n*100000 + j)})
			}
		}(i)
	}
	wg.Wait()
	// No assertion: the test exists for the race detector.
}
