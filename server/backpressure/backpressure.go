// Package backpressure implements per-repo concurrency limits and
// global log-fan-out caps (CIC-60).
//
// Per-repo limits prevent any single org/repo from monopolizing
// the runner pool. The default is 4 concurrent jobs per repo.
// Org-level and system-level limits can override.
package backpressure

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Limits is the concurrency configuration.
type Limits struct {
	// PerRepo is the max concurrent jobs per repo.
	PerRepo int

	// PerOrg is the max concurrent jobs per org (0 = unlimited).
	PerOrg int

	// System is the global max across all repos/orgs (0 = unlimited).
	System int
}

// DefaultLimits returns the default concurrency limits.
func DefaultLimits() Limits {
	return Limits{PerRepo: 4, PerOrg: 0, System: 0}
}

// Tracker monitors concurrent job counts per repo and org.
type Tracker struct {
	limits    Limits
	mu        sync.Mutex
	repoCount map[string]*atomic.Int32
	orgCount  map[string]*atomic.Int32
	total     atomic.Int32
	// runs maps a run ID to the slot it holds (CIC-106). Guarded by mu.
	runs map[string]slot
}

// slot is the repo/org pair whose counters a run's Acquire incremented.
type slot struct{ repoID, orgID string }

// NewTracker creates a tracker with the given limits.
func NewTracker(limits Limits) *Tracker {
	if limits.PerRepo == 0 {
		limits.PerRepo = 4
	}
	return &Tracker{
		limits:    limits,
		repoCount: make(map[string]*atomic.Int32),
		orgCount:  make(map[string]*atomic.Int32),
		runs:      make(map[string]slot),
	}
}

// Acquire atomically checks whether the repo/org has room for another
// job. Returns true if the caller may start the job, and if so,
// the counts have already been incremented. The caller MUST pair
// this with Release when the job finishes.
func (t *Tracker) Acquire(ctx context.Context, repoID, orgID string) (bool, error) {
	_ = ctx // context used in production for cancellation; not needed in this seed

	t.mu.Lock()
	defer t.mu.Unlock()

	// Check system limit.
	if t.limits.System > 0 && int(t.total.Load()) >= t.limits.System {
		return false, nil
	}

	// Check org limit.
	if t.limits.PerOrg > 0 {
		c := t.orgCount[orgID]
		if c == nil {
			c = &atomic.Int32{}
			t.orgCount[orgID] = c
		}
		if int(c.Load()) >= t.limits.PerOrg {
			return false, nil
		}
	}

	// Check per-repo limit.
	rc := t.repoCount[repoID]
	if rc == nil {
		rc = &atomic.Int32{}
		t.repoCount[repoID] = rc
	}
	if int(rc.Load()) >= t.limits.PerRepo {
		return false, nil
	}

	// All checks passed: increment and allow.
	rc.Add(1)
	if t.limits.PerOrg > 0 {
		oc := t.orgCount[orgID]
		if oc == nil {
			oc = &atomic.Int32{}
			t.orgCount[orgID] = oc
		}
		oc.Add(1)
	}
	t.total.Add(1)
	return true, nil
}

// Release decrements the counters for a finished job.
//
// It is safe to call Release more times than Acquire: counters are
// floored at zero so an unmatched release does not push them
// negative (which would silently defeat the concurrency cap).
func (t *Tracker) Release(repoID, orgID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseLocked(repoID, orgID)
}

// Register records that runID holds the slot an Acquire(repoID, orgID) took,
// so ReleaseByRunID can return it when the run finishes (CIC-106). Register
// it before the run's jobs are enqueued: a run that finishes before it is
// registered would otherwise keep its slot forever.
func (t *Tracker) Register(runID, repoID, orgID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runs[runID] = slot{repoID: repoID, orgID: orgID}
}

// ReleaseByRunID releases the slot registered for runID and forgets the
// registration. It reports whether a slot was released. Calling it again,
// or for a run that was never registered, is a no-op, so every path that
// ends a run may call it without risking a double release.
func (t *Tracker) ReleaseByRunID(runID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.runs[runID]
	if !ok {
		return false
	}
	delete(t.runs, runID)
	t.releaseLocked(s.repoID, s.orgID)
	return true
}

// releaseLocked decrements the counters for one slot. t.mu must be held.
func (t *Tracker) releaseLocked(repoID, orgID string) {
	if c, ok := t.repoCount[repoID]; ok {
		if v := c.Add(-1); v < 0 {
			c.Add(1) // floor at zero
		}
	}
	if t.limits.PerOrg > 0 {
		if oc, ok := t.orgCount[orgID]; ok {
			if v := oc.Add(-1); v < 0 {
				oc.Add(1) // floor at zero
			}
		}
	}
	if v := t.total.Add(-1); v < 0 {
		t.total.Add(1) // floor at zero
	}
}

// Count returns the current active job count for a repo.
func (t *Tracker) Count(repoID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.repoCount[repoID]; ok {
		return int(c.Load())
	}
	return 0
}

// TotalCount returns the global active job count.
func (t *Tracker) TotalCount() int {
	return int(t.total.Load())
}

// LogFanOut caps the number of concurrent SSE log subscribers
// (CIC-60: "bounded SSE log fan-out, global cap").
type LogFanOut struct {
	max    int
	active atomic.Int32
}

// NewLogFanOut creates a fan-out limiter with the given cap.
func NewLogFanOut(maxPerJob int) *LogFanOut {
	return &LogFanOut{max: maxPerJob}
}

// Acquire attempts to register a new subscriber.
//
// Returns false if the cap has been reached.
func (l *LogFanOut) Acquire() bool {
	if l.max > 0 && int(l.active.Load()) >= l.max {
		return false
	}
	n := l.active.Add(1)
	if l.max > 0 && int(n) > l.max {
		l.active.Add(-1)
		return false
	}
	return true
}

// Release removes a subscriber.
//
// It is floored at zero so an unmatched Release does not push the
// active count negative (which would allow the cap to be silently
// exceeded on the next Acquire cycle).
func (l *LogFanOut) Release() {
	if v := l.active.Add(-1); v < 0 {
		l.active.Add(1) // floor at zero
	}
}

// Active returns the current number of active subscribers.
func (l *LogFanOut) Active() int {
	return int(l.active.Load())
}

// String returns a human-readable summary.
func (l *LogFanOut) String() string {
	return fmt.Sprintf("LogFanOut{max=%d, active=%d}", l.max, l.Active())
}
