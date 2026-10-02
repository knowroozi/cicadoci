package webhook

import (
	"sync"
	"sync/atomic"
	"time"
)

// deliveryTracker implements delivery-ID idempotency.  When GitHub retries
// a delivery it sends the same X-Github-Delivery header value; the first
// delivery is processed, subsequent ones are suppressed with a "duplicate"
// outcome so run creation is exactly-once per unique delivery event.
//
// The MVP implementation is an in-memory bounded FIFO+TTL tracker (not true
// LRU: refreshed/hit entries are not repositioned in the eviction order, so a
// frequently-redelivered ID can still be evicted on its original insertion
// order while genuinely-idle IDs stay resident until the slice is trimmed).
// At maxSize=10_000 the difference is immaterial; see PR #23 review.
// A Postgres-backed tracker is a future ticket that makes duplicate
// suppression survive process restarts and supports multi-instance deployments.
type deliveryTracker struct {
	mu      sync.Mutex
	items   map[string]*entry // key = delivery ID
	order   []string          // insertion order (oldest first) for FIFO eviction
	maxSize int
	ttl     time.Duration
}

type entry struct {
	seenAt time.Time
}

// NewDeliveryTracker returns a thread-safe delivery tracker that remembers up
// to maxSize delivery IDs and treats any entry older than ttl as unseen.
func NewDeliveryTracker(maxSize int, ttl time.Duration) *deliveryTracker {
	if maxSize <= 0 {
		maxSize = 10_000
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &deliveryTracker{
		items:   make(map[string]*entry),
		order:   make([]string, 0, maxSize),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// seen returns true if the delivery ID has been recorded within the TTL
// window, and records it if new.  It is safe for concurrent use.
//
// DEPRECATED: Use check() + record() to avoid recording failed dispatches.
//
// The atomic counter records how many deliveries were suppressed (returned
// true) for observability.
func (t *deliveryTracker) seen(id string, now time.Time, suppressed *uint64) bool {
	if t.check(id, now, suppressed) {
		return true
	}
	t.record(id, now)
	return false
}

// check returns true if the delivery ID was recorded within the TTL window
// without inserting a new entry.  Safe for concurrent use.
func (t *deliveryTracker) check(id string, now time.Time, suppressed *uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.items[id]
	if ok {
		if now.Sub(e.seenAt) < t.ttl {
			if suppressed != nil {
				atomic.AddUint64(suppressed, 1)
			}
			return true
		}
		// Expired: treat as unseen; do NOT refresh the timer, or an
		// attacker redelivering an old ID can keep the slot alive indefinitely.
	}
	return false
}

// record inserts the delivery ID into the tracker after a successful
// dispatch.  Safe for concurrent use.
func (t *deliveryTracker) record(id string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.items[id]; ok {
		// Already present; refresh the timestamp.
		t.items[id] = &entry{seenAt: now}
		return
	}
	t.items[id] = &entry{seenAt: now}
	t.order = append(t.order, id)
	if len(t.order) > t.maxSize {
		oldest := t.order[0]
		t.order = t.order[1:]
		delete(t.items, oldest)
	}
}
