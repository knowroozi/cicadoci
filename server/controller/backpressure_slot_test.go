package controller

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/webhook"
)

// CIC-106: a run holds its repo's backpressure slot from the webhook until it
// is terminal. These run against Postgres.

// withTracker swaps a fresh tracker into the fixture's controller.
func withTracker(fx rerunFixture, limits backpressure.Limits) *backpressure.Tracker {
	tk := backpressure.NewTracker(limits)
	fx.c.cfg.Backpressure = tk
	return tk
}

// The slot outlives HandlePush, blocks the next push at the cap, and comes
// back when the queue finishes the run and fires the run-terminal hook, the
// path main.go wires.
func TestRunHoldsSlotUntilRunTerminal(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)
	tk := withTracker(fx, backpressure.Limits{PerRepo: 1})
	q := queue.NewPostgres(fx.st.Pool())
	q.SetRunTerminalHook(func(runID uuid.UUID, _ string) { fx.c.ReleaseRun(runID) })

	if err := pushTo(t, fx, "sha-1"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if got := tk.Count(fx.repoID.String()); got != 1 {
		t.Fatalf("slots held after HandlePush returned = %d, want 1", got)
	}
	if err := pushTo(t, fx, "sha-2"); err == nil {
		t.Fatal("second push accepted while the first run holds the only slot")
	}

	// Finish the run's only job through the queue, as a runner would.
	run := fx.runs(t)[0]
	var jobID uuid.UUID
	if err := fx.st.Pool().QueryRow(context.Background(),
		`SELECT id FROM jobs WHERE run_id = $1`, run.id).Scan(&jobID); err != nil {
		t.Fatalf("find job: %v", err)
	}
	var lease uuid.UUID
	if err := fx.st.Pool().QueryRow(context.Background(), `
		UPDATE jobs SET status = 'running', lease_id = gen_random_uuid(),
		       lease_expires_at = now() + interval '1 hour', attempts = attempts + 1
		 WHERE id = $1 RETURNING lease_id`, jobID).Scan(&lease); err != nil {
		t.Fatalf("lease job: %v", err)
	}
	if err := q.Complete(context.Background(), queue.Completion{
		JobID: jobID, LeaseID: lease, State: queue.StateSucceeded,
	}); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	if got := tk.Count(fx.repoID.String()); got != 0 {
		t.Fatalf("slots held after the run finished = %d, want 0", got)
	}
	if err := pushTo(t, fx, "sha-3"); err != nil {
		t.Fatalf("push after the run finished: %v", err)
	}
}

// A run that gets no jobs never reaches the run-terminal hook, so its slot
// must come back, and its check be completed, before HandlePush returns.
func TestRunWithNoJobsReleasesSlot(t *testing.T) {
	fx := newRerunFixture(t, prWorkflow) // pull_request only: a push matches nothing
	tk := withTracker(fx, backpressure.Limits{PerRepo: 1})

	for i, sha := range []string{"sha-1", "sha-2", "sha-3"} {
		if err := pushTo(t, fx, sha); err != nil {
			t.Fatalf("push %d: %v (slot leaked by a zero-job run)", i+1, err)
		}
	}
	if got := tk.Count(fx.repoID.String()); got != 0 {
		t.Fatalf("slots held = %d, want 0", got)
	}
	for _, r := range fx.runs(t) {
		if r.jobs != 0 {
			t.Fatalf("run has %d jobs; the test needs zero-job runs", r.jobs)
		}
	}
	// finishIfNoJobs also completes each run's forge check (CIC-81).
	if n := len(fx.forge.completes); n != 3 {
		t.Fatalf("forge.Complete calls = %d, want 3 (one per zero-job run)", n)
	}
}

// A forge re-run takes and holds a slot like any other run.
func TestRerunHoldsSlot(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)
	tk := withTracker(fx, backpressure.Limits{PerRepo: 2})

	if err := pushTo(t, fx, "sha-1"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := fx.c.HandleCheckSuite(decode[webhook.CheckSuitePayload](t, `{"action": "rerequested",
		"check_suite": {"id": 1, "head_sha": "sha-1"}, "repository": {"full_name": "acme/widgets"}}`)); err != nil {
		t.Fatalf("HandleCheckSuite: %v", err)
	}
	if got := tk.Count(fx.repoID.String()); got != 2 {
		t.Fatalf("slots held = %d, want 2 (push run + re-run)", got)
	}
	for _, r := range fx.runs(t) {
		fx.c.ReleaseRun(r.id)
	}
	if got := tk.Count(fx.repoID.String()); got != 0 {
		t.Fatalf("slots held after both runs released = %d, want 0", got)
	}
}
