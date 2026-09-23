package runner

import (
	"context"
	"testing"
	"time"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
)

// newTestAgent builds an Agent without a real transport connection: Run() is not
// the thing under test here, only the pure/jitter and drain machinery the Run
// loop drives. jobCtx/jobCancel are wired up so drain has a context to cancel,
// exactly as Run would have set them.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := New(Config{
		Executors:     []string{"shell"},
		Capacity:      2,
		WorkspaceRoot: t.TempDir(),
	}, nil)
	a.jobCtx, a.jobCancel = context.WithCancel(context.Background())
	return a
}

// ---------------------------------------------------------------------------
// Jittered reconnect backoff (sub-task 2)
// ---------------------------------------------------------------------------

// The window grows by 2x per attempt and is capped at max, regardless of a long
// attempt count. rnd returns the full window so we observe the raw window.
func TestJitteredBackoffWindowExpandsThenCaps(t *testing.T) {
	const (
		base = 10 * time.Millisecond
		max  = 80 * time.Millisecond
	)
	top := func(window int64) int64 { return window } // worst case: full window

	want := []time.Duration{base, base * 2, base * 4, base * 8, max, max, max}
	for i, w := range want {
		if got := jitteredBackoff(i, base, max, top); got != w {
			t.Errorf("attempt %d: got %v, want %v", i, got, w)
		}
	}
	// A very high attempt is still capped and does not overflow int64.
	if got := jitteredBackoff(1<<20, base, max, top); got != max {
		t.Errorf("huge attempt: got %v, want capped %v", got, max)
	}
}

// A zero draw must floor to base, never a busy-loop.
func TestJitteredBackoffFloorsZeroDraw(t *testing.T) {
	const base = 10 * time.Millisecond
	zero := func(window int64) int64 { return 0 }
	for attempt := 0; attempt <= 6; attempt++ {
		if got := jitteredBackoff(attempt, base, 30*time.Second, zero); got != base {
			t.Fatalf("attempt %d: a zero draw should floor to base %v, got %v", attempt, base, got)
		}
	}
}

// The production jitter source draws strictly inside the window so a real
// reconnect never sleeps the whole window and never busy-loops. The v2 global
// source is auto-seeded, so this is a property check rather than a fixed-trace one.
func TestRandJitterStaysWithinWindow(t *testing.T) {
	windows := []int64{1, 10, 1_000_000_000, int64(30 * time.Second)}
	for _, window := range windows {
		for i := 0; i < 300; i++ {
			if v := randJitter(window); v < 0 || v >= window {
				t.Fatalf("randJitter(%d) = %d; want a value in [0, %d)", window, v, window)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Graceful drain (sub-task 3)
// ---------------------------------------------------------------------------

// A drain with no in-flight work returns promptly and stops admissions.
func TestDrainNoInflight(t *testing.T) {
	a := newTestAgent(t)

	start := time.Now()
	a.drain(0)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("drain with no in-flight jobs took %s", elapsed)
	}
	if !a.draining.Load() {
		t.Error("drain did not set the draining flag")
	}
}

// A job that finishes on its own within the grace window is not cancelled: drain
// returns via the "finished" arm and leaves jobCtx untouched.
func TestDrainJobFinishesWithinGrace(t *testing.T) {
	a := newTestAgent(t)

	// One in-flight job that finishes before the grace window closes.
	a.drainWG.Add(1)
	go func() {
		defer a.drainWG.Done()
		time.Sleep(40 * time.Millisecond)
	}()

	done := make(chan struct{})
	go func() {
		a.drain(5 * time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not return after the in-flight job finished within grace")
	}

	if err := a.jobCtx.Err(); err != nil {
		t.Fatalf("job context was cancelled although it finished within grace: %v", err)
	}
}

// A straggler that outlives the grace window is cancelled by the drain, and the
// drain does not wait out the job's own (long) timeout.
func TestDrainCancelsStragglerAfterGrace(t *testing.T) {
	a := newTestAgent(t)

	grace := 150 * time.Millisecond
	a.drainWG.Add(1)
	go func() {
		defer a.drainWG.Done()
		select {
		case <-a.jobCtx.Done():
		case <-time.After(10 * time.Second): // would over-run the grace window
		}
	}()

	start := time.Now()
	a.drain(grace)
	elapsed := time.Since(start)

	if err := a.jobCtx.Err(); err == nil {
		t.Fatal("straggling job was not cancelled after the grace window elapsed")
	}
	if elapsed < grace {
		t.Errorf("drain returned after %s, before the %s grace elapsed", elapsed, grace)
	}
	if elapsed > grace+2*time.Second {
		t.Errorf("drain took %s, far past the %s grace", elapsed, grace)
	}
}

// ---------------------------------------------------------------------------
// Drain admission gate
// ---------------------------------------------------------------------------

// Once draining is set, a new job is refused outright (so the server requeues it)
// and never reaches the active set.
func TestStartJobRejectsWhileDraining(t *testing.T) {
	a := newTestAgent(t)
	a.draining.Store(true)

	var got []*runnerv1.JobAck
	send := func(m *runnerv1.RunnerMessage) error {
		if ack := m.GetJobAck(); ack != nil {
			got = append(got, ack)
		}
		return nil
	}

	a.startJob(context.Background(), &runnerv1.JobAssignment{
		JobId:   "job-1",
		LeaseId: "lease-1",
		Spec:    &runnerv1.JobSpec{Executor: "shell"},
	}, send)

	if len(got) != 1 {
		t.Fatalf("expected exactly one ack, got %d", len(got))
	}
	if got[0].GetResult() != runnerv1.JobAck_REJECTED || got[0].GetRejectReason() != "draining" {
		t.Fatalf("want REJECTED/draining, got %v/%q", got[0].GetResult(), got[0].GetRejectReason())
	}
	if n := a.activeCount(); n != 0 {
		t.Fatalf("a draining runner accepted new work; active job count = %d", n)
	}
}
