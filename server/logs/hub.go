// Package logs provides Server-Sent Events (SSE) fan-out for per-job log
// streams plus a Last-Event-ID resume protocol.
//
// The HTTP layer (server/api) wires the HTTP handler to the Hub; the
// runner-side ingest path (server/runnersvc) publishes new entries to
// the Hub as they are persisted. Subscribers pull a snapshot from the
// Compactor and then read live entries from the Hub.
//
// MVP scope: a per-job Hub with buffered per-subscriber channels, a
// drop-on-full policy with a one-time drop signal, and a
// Last-Event-ID resume path. Backpressure on the HTTP side is enforced
// by the request's context: a stalled client disconnects and the
// hub drops the subscription.
package logs

import (
	"errors"
	"sync"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/archive"
)

// Hub is the per-process fan-out registry.
//
// One Hub lives for the life of the server process. It is safe for
// concurrent use.
type Hub struct {
	mu      sync.RWMutex
	subs    map[uuid.UUID]map[uint64]*Sub // jobID → subscriberID → Sub
	counter uint64
	bufCap  int
}

// NewHub builds a Hub with the given per-subscriber buffer size.
//
// bufCap <= 0 falls back to a sensible default.
func NewHub(bufCap int) *Hub {
	if bufCap <= 0 {
		bufCap = 1024
	}
	return &Hub{
		subs:   make(map[uuid.UUID]map[uint64]*Sub),
		bufCap: bufCap,
	}
}

// Sub is an active subscription to a job's log stream.
//
// A Sub's lifecycle:
//  1. Subscribe returns a *Sub with a buffered Entries channel and an
//     open Done channel.
//  2. The caller reads from Sub.Entries to consume new log entries.
//  3. When the connection drops (or the hub drops the subscriber for
//     backpressure), Sub.Done is closed and further reads on
//     Sub.Entries drain the remaining buffered entries then return
//     zero + false.
type Sub struct {
	JobID   uuid.UUID
	Entries <-chan archive.Entry // receive-only view of the internal channel
	Done    chan struct{}        // closed when the sub is dropped or unsubscribed

	// send is the sendable side of the channel that backs Entries.
	// Internal only; callers must send via Publish.
	send   chan archive.Entry
	dropOn sync.Once
}

// Subscribe creates a new subscription for jobID.
//
// fromSeq is unused today: the hub doesn't retain history, so a
// subscriber always starts from the next published entry. For
// absolute resume, pair Subscribe with a Compactor.ReadAll replay of
// the historical segment.
func (h *Hub) Subscribe(jobID uuid.UUID, fromSeq int64) *Sub {
	_ = fromSeq
	c := make(chan archive.Entry, h.bufCap)
	s := &Sub{
		JobID:   jobID,
		Entries: (<-chan archive.Entry)(c),
		Done:    make(chan struct{}),
		send:    c,
	}
	h.mu.Lock()
	h.counter++
	id := h.counter
	if h.subs[jobID] == nil {
		h.subs[jobID] = make(map[uint64]*Sub)
	}
	h.subs[jobID][id] = s
	h.mu.Unlock()
	return s
}

// Unsubscribe removes a subscription from the hub.
//
// It is idempotent. The Done channel is closed to signal the subscriber
// to stop reading.
func (h *Hub) Unsubscribe(jobID uuid.UUID, s *Sub) {
	h.mu.Lock()
	if subs, ok := h.subs[jobID]; ok {
		for id, sub := range subs {
			if sub == s {
				delete(subs, id)
				break
			}
		}
		if len(subs) == 0 {
			delete(h.subs, jobID)
		}
	}
	h.mu.Unlock()
	closeOnce(&s.dropOn, s.Done)
}

// Publish delivers an entry to every active subscriber of jobID whose
// buffer has room. A full buffer triggers a drop: the subscriber is
// removed from the Hub and its Done channel is closed.
//
// Returns the number of subscribers the entry was delivered to (live
// count), not including any that were dropped.
func (h *Hub) Publish(jobID uuid.UUID, e archive.Entry) int {
	h.mu.RLock()
	subs := h.subs[jobID]
	// Copy the keys so we can hold no read lock during the non-blocking
	// sends. This avoids a deadlock between the RLock held here and the
	// Lock a concurrent Unsubscribe might try to take.
	ids := make([]uint64, 0, len(subs))
	for id := range subs {
		ids = append(ids, id)
	}
	h.mu.RUnlock()
	if len(ids) == 0 {
		return 0
	}
	delivered := 0
	for _, id := range ids {
		s := subs[id]
		// Try to deliver. If the subscriber's buffer is full, drop it
		// and close its Done channel so the HTTP handler can tear
		// down cleanly.
		select {
		case s.send <- e:
			delivered++
		default:
			closeOnce(&s.dropOn, s.Done)
			h.remove(jobID, id)
		}
	}
	return delivered
}

// remove deletes a subscriber under the write lock.
//
// It is safe to call even if the subscriber was already removed by a
// concurrent Unsubscribe.
func (h *Hub) remove(jobID uuid.UUID, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.subs[jobID]
	if cur == nil {
		return
	}
	delete(cur, id)
	if len(cur) == 0 {
		delete(h.subs, jobID)
	}
}

// SubscriberCount returns the number of active subscribers for jobID.
//
// It is for metrics/telemetry, not for control flow.
func (h *Hub) SubscriberCount(jobID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[jobID])
}

// ErrDropped is returned by the SSE handler when a subscriber was
// dropped from the hub for backpressure.
var ErrDropped = errors.New("logs: subscriber dropped for backpressure")

// closeOnce closes c the first time. It is used to ensure the Done
// channel is closed exactly once, regardless of whether Unsubscribe or
// a backpressure drop fired first.
func closeOnce(onec *sync.Once, c chan struct{}) {
	onec.Do(func() { close(c) })
}
