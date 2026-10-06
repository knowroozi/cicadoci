// Copyright 2024 Kaveh Nowroozi. Apache-2.0.
//
// Regression tests for CIC-193: the approval-gate audit append in main.go must
// survive a SIGTERM-canceled request context. The onResolved callback that
// records a terminal gate transition can fire during graceful shutdown: a gate
// timeout whose timer fires just after shutdown began, or a gate resolve that
// lands as the servers drain. CloseAll is a load-failure path, not a SIGTERM
// handler (CIC-204 tracks the shutdown-timer and abandoned-pending cases, out of
// scope for #141). If onResolved used the signal-canceled request context, the
// write would fail with context.Canceled and the transition would go un-audited.

package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/audit"
)

// recordingAuditLog is an audit.Log fake that captures, AT THE TIME Append is
// called, whether the passed context was already canceled and whether it carries
// a deadline. The call-time snapshot matters: appendGateAudit detaches the
// context and then defers its cancel(), so a context captured after
// appendGateAudit returns would already be canceled.
type recordingAuditLog struct {
	gotCanceled bool // true if the context was Done at Append-call time
	gotDeadline bool // true if the context carries a deadline
}

func (r *recordingAuditLog) Append(ctx context.Context, _ audit.Entry) (int64, error) {
	r.gotCanceled = ctx.Err() != nil
	_, r.gotDeadline = ctx.Deadline()
	return 1, nil
}

func (r *recordingAuditLog) Get(context.Context, int64) (audit.Entry, error) {
	return audit.Entry{}, nil
}

func (r *recordingAuditLog) List(context.Context, int64, int) ([]audit.Entry, error) {
	return nil, nil
}

func (r *recordingAuditLog) VerifyChain(context.Context) error { return nil }

// TestAppendGateAudit_DetachedFromCanceledContext is the CIC-193 regression:
// when the request context is already canceled (as it is on SIGTERM), the audit
// Append must still run on a context that is NOT canceled at call time, so the
// terminal gate transition is recorded.
func TestAppendGateAudit_DetachedFromCanceledContext(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &recordingAuditLog{}

	// Simulate SIGTERM: the parent request context is already Done.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("setup: the simulated SIGTERM ctx is not canceled")
	}

	appendGateAudit(ctx, fake, audit.Entry{Action: audit.ActionApprove, Target: "run-1"}, log, "run-1")

	if fake.gotCanceled {
		t.Error("Append received an already-canceled context; it must be detached " +
			"from the SIGTERM-canceled parent so the gate transition is audited (CIC-193)")
	}
}

// TestAppendGateAudit_BoundedByTimeout guards the second half of the fix: the
// detached context must still carry a deadline so a hung DB connection cannot
// block graceful shutdown.
func TestAppendGateAudit_BoundedByTimeout(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &recordingAuditLog{}

	appendGateAudit(context.Background(), fake, audit.Entry{Target: "run-1"}, log, "run-1")

	if !fake.gotDeadline {
		t.Error("the append context carries no deadline; a hung DB connection " +
			"could block shutdown")
	}
	if fake.gotCanceled {
		t.Error("a live parent should not produce a canceled append context")
	}
}

// TestAppendGateAudit_NilAuditLogIsNoop: a nil audit log (gates audit disabled)
// must be a no-op, never a panic.
func TestAppendGateAudit_NilAuditLogIsNoop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Must not panic; nothing is appended.
	appendGateAudit(context.Background(), nil, audit.Entry{Target: "run-1"}, log, "run-1")
}

// countingAuditLog is an audit.Log fake that COUNTS appends and captures, at
// call time, the entry's action and whether the context was already canceled.
// It is used by the G2 end-to-end test to assert exactly one, non-canceled
// append after the drain.
type countingAuditLog struct {
	entries []recordedEntry
}

type recordedEntry struct {
	action          audit.Action
	cancelledAtCall bool
}

func (c *countingAuditLog) Append(ctx context.Context, e audit.Entry) (int64, error) {
	c.entries = append(c.entries, recordedEntry{
		action:          e.Action,
		cancelledAtCall: ctx.Err() != nil,
	})
	return int64(len(c.entries)), nil
}

func (c *countingAuditLog) Get(context.Context, int64) (audit.Entry, error) {
	if len(c.entries) == 0 {
		return audit.Entry{}, nil
	}
	return audit.Entry{Action: c.entries[len(c.entries)-1].action}, nil
}

func (c *countingAuditLog) List(_ context.Context, _ int64, _ int) ([]audit.Entry, error) {
	return nil, nil
}

func (c *countingAuditLog) VerifyChain(context.Context) error { return nil }

// TestBuildApprovalGates_DrainInFlightAuditOnCloseAll is the G2 end-to-end
// acceptance: not just the leaf (appendGateAudit) but the wiring. It builds the
// real Manager + its drain wait-func via buildApprovalGates under a context that
// is then canceled (as on SIGTERM), opens a pending gate, and lets CloseAll drive
// the GateTimedOut onResolved. It asserts the drain yields exactly one,
// non-canceled GateTimeout append, so a pool close during shutdown cannot race
// the in-flight write. The three TestAppendGateAudit_* leaf tests are kept;
// this one exercises the wait-func the shutdown path now calls.
func TestBuildApprovalGates_DrainInFlightAuditOnCloseAll(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &countingAuditLog{}

	ctx, cancel := context.WithCancel(context.Background())
	mgr, _, waitGateAudit, err := buildApprovalGates(ctx, true, true, "u-1", fake, log)
	if err != nil {
		t.Fatalf("buildApprovalGates: %v", err)
	}

	// Simulate SIGTERM: the context the wiring was built on is now done.
	cancel()
	if err := mgr.Open(context.Background(), &approvalgates.Gate{
		RunID:   "run-1",
		Channel: "c1",
		Reason:  "deploy",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"u-1"},
	}); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	// CloseAll drives the timed-out transition here as a stand-in for a
	// shutdown-driven resolve. Under the CIC-193 G1a mutex design that write is
	// joined by waitGateAudit when it lands before shutdown, or run synchronously
	// once closing is set; both paths are joined here by waitGateAudit.
	mgr.CloseAll("run-1")
	waitGateAudit()

	if n := len(fake.entries); n != 1 {
		t.Fatalf("append count after drain = %d, want exactly 1", n)
	}
	e := fake.entries[0]
	if e.action != audit.ActionGateTimeout {
		t.Errorf("action = %q, want %q", e.action, audit.ActionGateTimeout)
	}
	if e.cancelledAtCall {
		t.Error("the drained append saw a canceled context; a pool close would have raced it (CIC-193 G2)")
	}
}

// blockingAuditLog delegates to countingAuditLog but blocks inside Append
// between "entered" and "release", so a test can force the drain
// (waitGateAudit) to have a genuinely in-flight, not-yet-finished append to
// join. This is what G4 needs: G2 cannot detect the drain because the
// synchronous write finishes inside CloseAll, so nothing is left to join.
type blockingAuditLog struct {
	countingAuditLog
	entered chan struct{}
	release chan struct{}
}

func (b *blockingAuditLog) Append(ctx context.Context, e audit.Entry) (int64, error) {
	close(b.entered) // signal the append started
	<-b.release      // block until the test releases us
	return b.countingAuditLog.Append(ctx, e)
}

// TestBuildApprovalGates_DrainJoinsBlockInFlightAudit is the G4 acceptance test:
// it proves the drain joins a genuinely BLOCKED, in-flight append. CloseAll fires
// onResolved(GateTimedOut), which writes synchronously; here the append blocks
// on a channel, so waitGateAudit must block until the append is released.
// Removing the drainWG.Wait() makes the 100ms "still blocked" assertion fail --
// confirming the test guards the join, not the reverse.
func TestBuildApprovalGates_DrainJoinsBlockInFlightAudit(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &blockingAuditLog{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	mgr, _, waitGateAudit, err := buildApprovalGates(ctx, true, true, "u-1", fake, log)
	if err != nil {
		t.Fatalf("buildApprovalGates: %v", err)
	}

	// Simulate SIGTERM: the context the wiring was built on is now done.
	cancel()
	if err := mgr.Open(context.Background(), &approvalgates.Gate{
		RunID:   "run-1",
		Channel: "c1",
		Reason:  "deploy",
		Allow:   "approve",
		Deny:    "deny",
		Allowed: []string{"u-1"},
	}); err != nil {
		t.Fatalf("open gate: %v", err)
	}

	// CloseAll fires onResolved synchronously; the append then blocks on
	// fake.release, so CloseAll does not return until it is released. Run it in
	// a goroutine.
	closeAllDone := make(chan struct{})
	go func() {
		mgr.CloseAll("run-1")
		close(closeAllDone)
	}()

	// Confirm the blocking append is in flight.
	select {
	case <-fake.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight append never started (CloseAll did not reach Append)")
	}

	// waitGateAudit must BLOCK on the in-flight append: it has not returned after
	// 100ms. This is the assertion that fails if drainWG.Wait() is removed.
	waitDone := make(chan struct{})
	go func() {
		waitGateAudit()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("waitGateAudit() returned before the in-flight append finished; " +
			"the drain is not joining it (CIC-193 G4)")
	case <-time.After(100 * time.Millisecond):
		// expected: the drain is still blocked on the in-flight append
	}

	// Release the append; the drain and CloseAll must both finish within 2s.
	close(fake.release)
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waitGateAudit() did not return within 2s of releasing the append (CIC-193 G4)")
	}
	select {
	case <-closeAllDone:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseAll did not finish within 2s of releasing the append")
	}

	if n := len(fake.entries); n != 1 {
		t.Fatalf("append count = %d, want exactly 1", n)
	}
}
