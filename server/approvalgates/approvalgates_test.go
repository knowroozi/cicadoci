package approvalgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cicadoci.com/cicado/server/chat"
)

func TestOpenAndResolve(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 1)
	mgr := NewManager(provider, func(_ string, s GateState) {
		if s == GateApproved || s == GateDenied {
			resolved <- s
		}
	})

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy to production?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1", "user-2"},
		Timeout: time.Hour,
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	if mgr.State("run-1") != GatePending {
		t.Fatal("expected GatePending")
	}

	if err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resolved:
		if got != GateApproved {
			t.Fatalf("expected GateApproved, got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for resolution")
	}
}

func TestUnauthorizedActor(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "user-evil", "approve")
	if err == nil {
		t.Fatal("expected error for unauthorized actor")
	}
	if mgr.State("run-1") != GatePending {
		t.Fatal("gate should still be pending after unauthorized attempt")
	}
}

func TestEmptyAllowList(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: nil,
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve")
	if err == nil {
		t.Fatal("expected error with empty allow-list (fail-closed)")
	}
}

func TestEmptyActorID(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "", "approve")
	if err == nil {
		t.Fatal("expected error for empty actor ID")
	}
}

func TestDenyAction(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	if err := mgr.Resolve(context.Background(), "run-1", "user-1", "deny"); err != nil {
		t.Fatal(err)
	}
	if mgr.State("run-1") != GateDenied {
		t.Fatalf("expected GateDenied, got %v", mgr.State("run-1"))
	}
}

func TestIdempotentResolve(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	_ = mgr.Resolve(context.Background(), "run-1", "user-1", "approve")
	// Second resolve should be idempotent.
	err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve")
	if err != nil {
		t.Fatalf("expected idempotent resolve, got %v", err)
	}
}

func TestCloseAllTimesOut(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 1)
	mgr := NewManager(provider, func(_ string, s GateState) {
		resolved <- s
	})

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)
	mgr.CloseAll("run-1")

	select {
	case got := <-resolved:
		if got != GateTimedOut {
			t.Fatalf("expected GateTimedOut, got %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestUnknownRunID(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)
	err := mgr.Resolve(context.Background(), "nonexistent", "user-1", "approve")
	if err == nil {
		t.Fatal("expected error for unknown run ID")
	}
}

func TestUnknownAction(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	_ = mgr.Open(context.Background(), &gate)

	err := mgr.Resolve(context.Background(), "run-1", "user-1", "unknown-action")
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
	if mgr.State("run-1") != GatePending {
		t.Fatal("gate should still be pending after unknown action")
	}
}

func TestResolveClosesButtons(t *testing.T) {
	// Catches the "CloseApproval silently fails" bug: without verifying
	// provider.Records() a wrong msgID would go undetected.
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-1",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
		Channel: "#ci",
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	// After Open, the record should exist with Action = ActionApproval.
	records := provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Action != chat.ActionApproval {
		t.Fatalf("expected ActionApproval, got %q", records[0].Action)
	}

	if err := mgr.Resolve(context.Background(), "run-1", "user-1", "approve"); err != nil {
		t.Fatal(err)
	}
	// After resolve, the record should have Action = ActionClose.
	records = provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record after resolve, got %d", len(records))
	}
	if records[0].Action != chat.ActionClose {
		t.Fatalf("expected ActionClose after resolve, got %q — CloseApproval silently failed", records[0].Action)
	}
	if records[0].Status != chat.ApprovalApproved {
		t.Fatalf("expected ApprovalApproved status, got %v", records[0].Status)
	}
}

func TestCloseAllClosesButtons(t *testing.T) {
	// Catches the "CloseAll doesn't close the provider-side buttons" bug.
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-2",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
		Channel: "#ci",
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	records := provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Action != chat.ActionApproval {
		t.Fatalf("expected ActionApproval before CloseAll, got %q", records[0].Action)
	}

	mgr.CloseAll("run-2")
	records = provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 record after CloseAll, got %d", len(records))
	}
	if records[0].Action != chat.ActionClose {
		t.Fatalf("expected ActionClose after CloseAll, got %q — CloseAll didn't close buttons", records[0].Action)
	}
	if records[0].Status != chat.ApprovalTimedOut {
		t.Fatalf("expected ApprovalTimedOut status, got %v", records[0].Status)
	}
}

// failingProvider is a ChatProvider whose RequestApproval always fails, used to
// prove Open removes a half-open gate when the post itself fails.
type failingProvider struct {
	chat.SlackProvider
}

func (f *failingProvider) RequestApproval(_ context.Context, _ string, _ string, _ chat.ApprovalRequest) (string, string, error) {
	return "", "", fmt.Errorf("post failed")
}

// TestFailedPostDropsGate is the CIC-108 "gate recorded on a failed post"
// regression: when RequestApproval fails, Open must clean up the record so the
// gate is neither recorded nor later resolvable (no dead record, no dangling
// callback, no dangling timer).
func TestFailedPostDropsGate(t *testing.T) {
	provider := &failingProvider{}
	mgr := NewManager(provider, nil)

	gate := Gate{
		RunID:   "run-fail",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	if err := mgr.Open(context.Background(), &gate); err == nil {
		t.Fatal("expected Open to fail when the post fails")
	}
	// No half-open record: the gate must be unresolvable.
	if err := mgr.Resolve(context.Background(), "run-fail", "user-1", "approve"); err == nil {
		t.Fatal("expected no recorded gate after a failed post")
	}
	// And no half-open record lingered as pending.
	if st := mgr.State("run-fail"); st != GatePending {
		// State() returns GatePending for an unknown run too, so this just guards
		// against a stale transition; the Resolve error above is the real proof.
		t.Fatalf("unexpected state %v after failed post", st)
	}
}

// TestTimeoutFiresGate is the CIC-108 "timeout never enforced" regression: a
// gate with a positive Timeout that nobody resolves transitions to GateTimedOut
// on its own, closes its buttons, and fires the callback.
// TestStaleTimerDoesNotTimeOutNewerGate pins CIC-108 B1: a stale auto-timeout
// armed by an earlier Open for a run must never time out a newer gate opened for
// the same run. Open(r, 40ms) then Open(r, 400ms): the 40ms timer must be
// stopped at the second Open (and, as defense in depth, its callback's
// record-identity guard must make a stray fire a no-op), so after 120ms the gate
// is still GatePending, and it times out only on its own 400ms timer. Without
// the fix the 40ms timer fires onTimeout looking up the now-current (400ms)
// record and moves it to GateTimedOut early, so the 120ms assertion fails.
func TestStaleTimerDoesNotTimeOutNewerGate(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 4)
	mgr := NewManager(provider, func(_ string, s GateState) { resolved <- s })

	base := Gate{
		RunID:   "run-reopen",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Channel: "#ci",
	}
	g1 := base
	g1.Timeout = 40 * time.Millisecond
	if err := mgr.Open(context.Background(), &g1); err != nil {
		t.Fatal(err)
	}
	// Re-Open the same run with a longer timeout; this must stop g1's timer.
	g2 := base
	g2.Timeout = 400 * time.Millisecond
	if err := mgr.Open(context.Background(), &g2); err != nil {
		t.Fatal(err)
	}

	// Well after the 40ms timer but before the 400ms timer: the newer gate must
	// still be pending -- the stale timer must not have moved it.
	time.Sleep(120 * time.Millisecond)
	if st := mgr.State("run-reopen"); st != GatePending {
		t.Fatalf("stale timer clobbered the newer gate: state at 120ms = %v, want GatePending", st)
	}
	if n := len(resolved); n != 0 {
		t.Fatalf("stale timer produced %d resolution callbacks; want 0", n)
	}

	// The gate's own 400ms timer must eventually fire and be the one that times
	// it out.
	deadline := time.Now().Add(800 * time.Millisecond)
	for mgr.State("run-reopen") != GateTimedOut {
		if time.Now().After(deadline) {
			t.Fatal("gate never timed out on its own timer")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The resolution callback fires a touch after State flips (it runs
	// outside the lock, after the close), so wait for it with a bounded
	// deadline rather than an immediate default-branch check.
	select {
	case got := <-resolved:
		if got != GateTimedOut {
			t.Fatalf("unexpected resolution state %v, want GateTimedOut", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected a GateTimedOut callback from the gate's own timer")
	}
	// Exactly one provider close, from the gate's own timeout.
	closes := 0
	for _, r := range provider.Records() {
		if r.Action == chat.ActionClose {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("expected exactly 1 provider close, got %d", closes)
	}
}

func TestTimeoutFiresGate(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 1)
	mgr := NewManager(provider, func(_ string, s GateState) { resolved <- s })

	gate := Gate{
		RunID:   "run-timeout",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Channel: "#ci",
		Timeout: 30 * time.Millisecond,
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resolved:
		if got != GateTimedOut {
			t.Fatalf("expected auto-timeout GateTimedOut, got %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout was never enforced: gate stuck at GatePending")
	}
	// The provider-side buttons must be closed as timed out.
	records := provider.Records()
	if len(records) != 1 {
		t.Fatalf("expected 1 provider record, got %d", len(records))
	}
	if records[0].Action != chat.ActionClose || records[0].Status != chat.ApprovalTimedOut {
		t.Fatalf("expected a timed-out close, got action=%q status=%v", records[0].Action, records[0].Status)
	}
}

// TestResolveBeatsTimeout guards the other side of the race: a manual resolve
// must win over a pending auto-timeout, so the later timer callback is a no-op
// and does not overwrite the resolution or double-close.
func TestResolveBeatsTimeout(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 4)
	mgr := NewManager(provider, func(_ string, s GateState) { resolved <- s })

	gate := Gate{
		RunID:   "run-race",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Channel: "#ci",
		Timeout: 50 * time.Millisecond,
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	// Resolve quickly, well before the timer would fire.
	if err := mgr.Resolve(context.Background(), "run-race", "user-1", "approve"); err != nil {
		t.Fatal(err)
	}
	if mgr.State("run-race") != GateApproved {
		t.Fatalf("expected GateApproved, got %v", mgr.State("run-race"))
	}
	// Let the timer fire; it must be a no-op, not a second transition.
	time.Sleep(80 * time.Millisecond)
	if got := mgr.State("run-race"); got != GateApproved {
		t.Fatalf("resolved gate was clobbered by the timer: got %v, want GateApproved", got)
	}

	// N2: assert exactly one resolution callback fired and exactly one provider
	// close happened, so a double-transition (e.g. the timer also closing)
	// would be caught.
	var callbacks int
drain:
	for {
		select {
		case <-resolved:
			callbacks++
		default:
			break drain
		}
	}
	if callbacks != 1 {
		t.Fatalf("expected exactly 1 resolution callback, got %d", callbacks)
	}
	closes := 0
	for _, r := range provider.Records() {
		if r.Action == chat.ActionClose {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("expected exactly 1 provider close, got %d", closes)
	}
}

// TestCallbackRunsOutsideLock is the CIC-108 "callback under lock" regression:
// the onResolved callback may re-enter the manager (e.g. to read State); with the
// callback invoked under the lock the old CloseAll/Resolve would deadlock.
func TestCallbackRunsOutsideLock(t *testing.T) {
	provider := chat.NewSlackProvider()
	done := make(chan GateState, 1)
	var mgr *Manager
	mgr = NewManager(provider, func(runID string, s GateState) {
		// Re-enter the manager from inside the callback: deadlocks if the
		// callback is invoked while m.mu is still held.
		if got := mgr.State(runID); got == s {
			done <- s
		}
	})
	gate := Gate{
		RunID:   "run-cb",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: time.Hour,
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}
	// N1: Call CloseAll in a goroutine so that, if a regression returns the
	// callback-invoked-under-lock bug, the deadlock happens in THAT goroutine
	// and the test's 2-second guard below fires as a fast failure, instead of
	// the deadlock blocking this test goroutine forever.
	go mgr.CloseAll("run-cb")
	select {
	case got := <-done:
		if got != GateTimedOut {
			t.Fatalf("expected GateTimedOut, got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback deadlocked on the manager lock (callback-under-lock bug)")
	}
}
