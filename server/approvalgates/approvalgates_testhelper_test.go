// Test helper for approvalgates_test.go (T1 guard test). It lives in the
// same package so it can reach the unexported `gates` map and `onTimeout`
// method that the external test file (approvalgates_test.go) references.
//
// recordFor returns the live GateRecord by identity so the test can drive
// onTimeout directly and check the pending guard's no-op behavior without
// relying on CloseAll to stop the timer (which would hide the guard).
package approvalgates

import (
	"sync"
	"testing"
)

// recordFor returns the live record for runID under the manager lock, or nil
// if the gate is not open. It is a thin test-only accessor; production code
// never needs it because the guard on onTimeout is checked against the
// live record by identity.
func (m *Manager) recordFor(t *testing.T, runID string) *GateRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.gates[runID]
	if !ok {
		t.Helper()
		t.Fatalf("recordFor: gate %q not found", runID)
	}
	return rec
}

// ensure the sync import is not unused when the helper file is compiled
// alone. It is used by the tests that use the helper.
var _ = sync.Mutex{}
