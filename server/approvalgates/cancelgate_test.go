// CIC-208: close approval-gate record on run cancel.
package approvalgates

import (
	"context"
	"testing"
	"time"

	"cicadoci.com/cicado/server/chat"
)

// TestCancelGate_ClosesPendingGate verifies that CancelGate transitions a
// still-pending gate to GateTimedOut and fires onResolved. Without CIC-208,
// a cancel would leave the gate record dangling (timer + map entry).
func TestCancelGate_ClosesPendingGate(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 1)
	mgr := NewManager(provider, func(_ string, s GateState, _ string) {
		resolved <- s
	})

	gate := Gate{
		RunID:   "run-42",
		Channel: "ci",
		Reason:  "needs approval",
		Allow:   "allow",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: 0, // no auto-timeout — the gate stays open until explicitly closed.
	}
	_ = mgr.Open(context.Background(), &gate)
	if got := mgr.State("run-42"); got != GatePending {
		t.Fatalf("pre-condition: state = %v, want GatePending", got)
	}
	mgr.CancelGate("run-42")
	if got := mgr.State("run-42"); got != GateTimedOut {
		t.Fatalf("after CancelGate: state = %v, want GateTimedOut", got)
	}
	select {
	case got := <-resolved:
		if got != GateTimedOut {
			t.Fatalf("onResolved state = %v, want GateTimedOut", got)
		}
	case <-time.After(time.Second):
		t.Fatal("onResolved not called by CancelGate")
	}
}

// TestCancelGate_AlreadyResolvedIsNoop verifies that cancelling a gate after
// it has resolved is a no-op and does not panic.
func TestCancelGate_AlreadyResolvedIsNoop(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-43",
		Channel: "ci",
		Allow:   "allow",
		Deny:    "deny",
		Allowed: []string{"user-1"},
	}
	_ = mgr.Open(context.Background(), &gate)
	if err := mgr.Resolve(context.Background(), "run-43", "user-1", "allow"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := mgr.State("run-43"); got != GateApproved {
		t.Fatalf("pre-condition: state = %v, want GateApproved", got)
	}
	mgr.CancelGate("run-43") // must be a no-op — gate is already resolved.
	if got := mgr.State("run-43"); got != GateApproved {
		t.Fatalf("after CancelGate on resolved gate: state = %v, want GateApproved", got)
	}
}

// TestCancelGate_UnknownRunIsNoop verifies that cancelling a gate for a run
// that has never been opened does not panic.
func TestCancelGate_UnknownRunIsNoop(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)
	mgr.CancelGate("no-such-run") // must not panic.
	if got := mgr.State("no-such-run"); got != GatePending {
		t.Fatalf("unknown run: state = %v, want GatePending", got)
	}
}
