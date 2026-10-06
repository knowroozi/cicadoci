package approvalgates

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"cicadoci.com/cicado/server/chat"
)

func TestOpenAndResolve(t *testing.T) {
	provider := chat.NewSlackProvider()
	resolved := make(chan GateState, 1)
	mgr := NewManager(provider, func(_ string, s GateState, _ string) {
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
	mgr := NewManager(provider, func(_ string, s GateState, _ string) {
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
	mgr := NewManager(provider, func(_ string, s GateState, _ string) { resolved <- s })

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
	mgr := NewManager(provider, func(_ string, s GateState, _ string) { resolved <- s })

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
	mgr := NewManager(provider, func(_ string, s GateState, _ string) { resolved <- s })

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
	mgr = NewManager(provider, func(runID string, s GateState, _ string) {
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

// TestDrainAllClosesAndAudits is CIC-204's core assertion: before process exit,
// every still-pending approval gate is moved to a terminal state (GateTimedOut),
// its interactive buttons are closed, and its transition is handed to onResolved
// -- i.e. it is audited -- rather than being "neither audited nor closed." The
// drain is idempotent: a second call drains nothing.
func TestDrainAllClosesAndAudits(t *testing.T) {
	provider := chat.NewSlackProvider()
	var mu sync.Mutex
	audited := map[string]GateState{}
	mgr := NewManager(provider, func(runID string, s GateState, _ string) {
		mu.Lock()
		audited[runID] = s
		mu.Unlock()
	})

	for _, id := range []string{"run-a", "run-b", "run-c"} {
		gate := Gate{
			RunID:   id,
			Reason:  "Deploy?",
			Allow:   "approve",
			Deny:    "deny",
			Allowed: []string{"user-1"},
			// Zero timeout = no auto-timeout, so the gate stays pending and is
			// the drain's responsibility to close.
		}
		if err := mgr.Open(context.Background(), &gate); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
		if mgr.State(id) != GatePending {
			t.Fatalf("%s should be pending, got %v", id, mgr.State(id))
		}
	}

	if got := mgr.DrainAll(context.Background()); got != 3 {
		t.Fatalf("DrainAll drained %d gates, want 3 (no-op second call expected below)", got)
	}
	for _, id := range []string{"run-a", "run-b", "run-c"} {
		if st := mgr.State(id); st != GateTimedOut {
			t.Errorf("%s state after drain = %v, want GateTimedOut", id, st)
		}
		mu.Lock()
		got, ok := audited[id]
		mu.Unlock()
		if !ok {
			t.Errorf("%s was not audited via onResolved", id)
		} else if got != GateTimedOut {
			t.Errorf("%s audited as %v, want GateTimedOut", id, got)
		}
	}

	// Idempotent: nothing left to drain.
	if got := mgr.DrainAll(context.Background()); got != 0 {
		t.Errorf("second DrainAll drained %d gates, want 0", got)
	}
}

// TestDrainAllLateTimerDoesNotDoubleAudit is the race half of CIC-204: a gate
// whose auto-timeout fires AFTER the drain must not be audited a second time or
// move off its terminal state.
//
// Dwight T1: this test must exercise the pending-guard directly, NOT rely on
// CloseAll/drainOne to stop the timer (which hides the guard). We open a gate,
// capture its record by identity, call DrainAll, and then call onTimeout on the
// live record by hand to force the "late timer" path. The guard
// (rec.State != GatePending) must make it a no-op.
func TestDrainAllLateTimerDoesNotDoubleAudit(t *testing.T) {
	provider := chat.NewSlackProvider()
	var mu sync.Mutex
	count := 0
	mgr := NewManager(provider, func(_ string, _ GateState, _ string) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	gate := Gate{
		RunID:   "run-x",
		Reason:  "Deploy?",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: 10 * time.Millisecond,
	}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}

	// Capture the live record by identity BEFORE the drain, so onTimeout's
	// guard can be checked against the record DrainAll will move.
	rec := mgr.recordFor(t, "run-x")
	if rec == nil {
		t.Fatal("recordFor returned nil for an open gate")
	}

	// Drain before the timer can fire.
	if got := mgr.DrainAll(context.Background()); got != 1 {
		t.Fatalf("DrainAll drained %d, want 1", got)
	}
	if mgr.State("run-x") != GateTimedOut {
		t.Fatalf("state after drain = %v, want GateTimedOut", mgr.State("run-x"))
	}

	// Force a "late timer" by calling onTimeout directly on the record.
	// The pending-guard must make this a no-op: onResolved must not fire
	// a second time.
	mgr.onTimeout(rec)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("onResolved fired %d times after drain + direct onTimeout, want exactly 1 (no double audit)", count)
	}
	if mgr.State("run-x") != GateTimedOut {
		t.Fatalf("state after direct onTimeout = %v, want GateTimedOut (unchanged)", mgr.State("run-x"))
	}
}

// TestDrainAllReasonsShutdownDrain asserts the run's audit row gets a reason
// of "abandoned: server shutdown" -- not "approval timed out" -- when DrainAll
// closes the gate. This is the B1 fix: a process restart must not read as a
// user-facing timeout.
func TestDrainAllReasonsShutdownDrain(t *testing.T) {
	provider := chat.NewSlackProvider()
	var mu sync.Mutex
	reasons := map[string]string{}
	mgr := NewManager(provider, func(runID string, _ GateState, reason string) {
		mu.Lock()
		reasons[runID] = reason
		mu.Unlock()
	})

	gate := Gate{RunID: "run-d", Reason: "Deploy?", Allow: "approve", Deny: "deny", Allowed: []string{"user-1"}}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatal(err)
	}

	if got := mgr.DrainAll(context.Background()); got != 1 {
		t.Fatalf("DrainAll drained %d, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := reasons["run-d"]; got != ReasonShutdownDrain {
		t.Fatalf("shutdown drain reason = %q, want %q", got, ReasonShutdownDrain)
	}
}

// TestDrainAllCancelledCtxStopsEarly is the non-blocking test Dwight asked
// for: when the shutdown budget is exceeded the drain stops iterating so the
// process can exit; whatever was not drained is abandoned by process exit,
// which is the same outcome as no drain at all. The closed flag is still so
// a concurrent Open is refused.
func TestDrainAllCancelledCtxStopsEarly(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, func(_ string, _ GateState, _ string) {})

	for _, id := range []string{"a", "b", "c"} {
		g := Gate{RunID: id, Reason: "r", Allow: "approve", Deny: "deny", Allowed: []string{"u1"}}
		if err := mgr.Open(context.Background(), &g); err != nil {
			t.Fatal(err)
		}
	}

	// Cancel the ctx before calling DrainAll; it must return without draining
	// any gate because ctx.Err() is checked at the top of the per-gate loop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := mgr.DrainAll(ctx); got != 0 {
		t.Fatalf("DrainAll with a cancelled-then-drained ctx drained %d, want 0", got)
	}

	// The closed flag is set inside the snapshot lock even when ctx is
	// already cancelled, so a concurrent Open is still refused.
	late := Gate{RunID: "late", Reason: "late", Allow: "approve", Deny: "deny", Allowed: []string{"u1"}}
	if err := mgr.Open(context.Background(), &late); err == nil {
		t.Fatal("Open after a cancelled-ctx DrainAll succeeded; wanted refusal due to closed flag")
	}
}

// TestManagerClosedRefusesOpen is O1: after DrainAll has set the manager's
// closed flag, Open must refuse a new gate. A late-arriving webhook that
// races the drain would otherwise open a gate that never gets closed or
// audited -- the exact "neither audited nor closed" failure CIC-204 addresses.
// TestAwaitReturnsReasonAfterDrain (CIC-204 r3 T2) proves the B1 fix:
// a gate drained on SIGTERM surfaces (GateTimedOut,
// ReasonShutdownDrain) at the controller's await-and-read boundary,
// and the onResolved callback carries ReasonShutdownDrain so the
// run-failure audit row says "abandoned: server shutdown".
func TestAwaitReturnsReasonAfterDrain(t *testing.T) {
	provider := chat.NewSlackProvider()
	var gotReason string
	done := make(chan struct{}, 1)
	mgr := NewManager(provider, func(runID string, state GateState, reason string) {
		gotReason = reason
		done <- struct{}{}
	})
	gate := Gate{RunID: "run-drain", Reason: "Deploy?", Allow: "approve", Deny: "deny", Allowed: []string{"u1"}, Timeout: time.Hour}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatalf("open: %v", err)
	}
	if n := mgr.DrainAll(context.Background()); n != 1 {
		t.Fatalf("DrainAll = %d, want 1", n)
	}
	<-done // onResolved fired outside the lock
	if gotReason != ReasonShutdownDrain {
		t.Errorf("onResolved reason = %q, want %q", gotReason, ReasonShutdownDrain)
	}
	state, reason := mgr.Await(context.Background(), "run-drain")
	if state != GateTimedOut {
		t.Errorf("Await state = %v, want GateTimedOut", state)
	}
	if reason != ReasonShutdownDrain {
		t.Errorf("Await reason = %q, want %q", reason, ReasonShutdownDrain)
	}
}

// TestAwaitReturnsReasonAfterAutoTimeout (CIC-204 r3 T2) mirrors the
// drain test for the auto-timeout case: Await returns
// (GateTimedOut, ReasonApprovalTimedOut), NOT empty or
// ReasonShutdownDrain -- so a real auto-timeout keeps its own reason
// string on the run row.
func TestAwaitReturnsReasonAfterAutoTimeout(t *testing.T) {
	provider := chat.NewSlackProvider()
	var gotReason string
	done := make(chan struct{}, 1)
	mgr := NewManager(provider, func(runID string, state GateState, reason string) {
		gotReason = reason
		done <- struct{}{}
	})
	gate := Gate{RunID: "run-timeout", Reason: "Deploy?", Allow: "approve", Deny: "deny", Allowed: []string{"u1"}, Timeout: 5 * time.Millisecond}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatalf("open: %v", err)
	}
	<-done
	state, reason := mgr.Await(context.Background(), "run-timeout")
	if state != GateTimedOut {
		t.Errorf("Await state = %v, want GateTimedOut", state)
	}
	if reason != ReasonApprovalTimedOut {
		t.Errorf("Await reason = %q, want %q", reason, ReasonApprovalTimedOut)
	}
	if gotReason != ReasonApprovalTimedOut {
		t.Errorf("onResolved reason = %q, want %q", gotReason, ReasonApprovalTimedOut)
	}
}

// TestResolverResolveDoesNotSetTerminalReason (CIC-204 r3 T2 mutation
// guard) confirms Resolve leaves terminalReason empty so a user
// resolution falls back to the controller's state-based default
// ("approval denied" / "approval approved").
func TestResolverResolveDoesNotSetTerminalReason(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, func(string, GateState, string) {})
	gate := Gate{RunID: "resolve-id", Reason: "Deploy?", Allow: "approve", Deny: "deny", Allowed: []string{"U1"}, Timeout: time.Hour}
	if err := mgr.Open(context.Background(), &gate); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := mgr.Resolve(context.Background(), "resolve-id", "U1", "approve"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rec := mgr.recordFor(t, "resolve-id")
	if rec.terminalReason != "" {
		t.Errorf("terminalReason = %q after Resolve, want empty", rec.terminalReason)
	}
	state, reason := mgr.Await(context.Background(), "resolve-id")
	if state != GateApproved {
		t.Errorf("Await state = %v, want GateApproved", state)
	}
	if reason != "" {
		t.Errorf("Await reason = %q, want empty (Resolve never sets it)", reason)
	}
}

func TestManagerClosedRefusesOpen(t *testing.T) {
	provider := chat.NewSlackProvider()
	mgr := NewManager(provider, func(_ string, _ GateState, _ string) {})

	g := Gate{RunID: "run-1", Reason: "r", Allow: "approve", Deny: "deny", Allowed: []string{"u1"}}
	if err := mgr.Open(context.Background(), &g); err != nil {
		t.Fatal(err)
	}

	if got := mgr.DrainAll(context.Background()); got != 1 {
		t.Fatalf("DrainAll drained %d, want 1", got)
	}

	late := Gate{RunID: "late", Reason: "late", Allow: "approve", Deny: "deny", Allowed: []string{"u1"}}
	if err := mgr.Open(context.Background(), &late); err == nil {
		t.Fatal("Open after DrainAll succeeded; wanted refusal")
	}
}
