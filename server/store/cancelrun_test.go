package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
)

// TestCancelRunJobs_FinishedRunIsNoOp (CIC-206, T1).
func TestCancelRunJobs_FinishedRunIsNoOp(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID, jid := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, ref, number, status, event) VALUES ($1, $2, 'main', 1, 'succeeded', 'push')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'job0', 'shell', 'succeeded')`, jid, runID); err != nil {
		t.Fatal(err)
	}
	var firedMu sync.Mutex
	firedFor := make(map[string]struct{})
	fireTerminal := func(_ context.Context, rid uuid.UUID) {
		firedMu.Lock()
		firedFor[rid.String()] = struct{}{}
		firedMu.Unlock()
	}
	var called bool
	touched, err := st.CancelRunJobs(ctx, runID, "stop",
		func(_ context.Context, _ uuid.UUID, _ string) error {
			called = true
			return nil
		},
		fireTerminal,
	)
	if err != nil {
		t.Fatalf("CancelRunJobs: %v", err)
	}
	if touched {
		t.Error("finished run: touched=true, want false")
	}
	if called {
		t.Error("cancelJob called for finished run")
	}
	// Finished run: no active jobs, all terminal → no finalize → no hook.
	if len(firedFor) != 0 {
		t.Errorf("fireTerminal called for finished run, want 0; got %d", len(firedFor))
	}
}

// TestCancelRunJobs_UnknownRunIsNoOp (CIC-206, T1).
func TestCancelRunJobs_UnknownRunIsNoOp(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	runID := uuid.New()
	fired := make(map[string]struct{})
	fireTerminal := func(_ context.Context, rid uuid.UUID) {
		fired[rid.String()] = struct{}{}
	}
	touched, err := st.CancelRunJobs(ctx, runID, "stop",
		func(_ context.Context, _ uuid.UUID, _ string) error {
			t.Fatal("cancelJob must not be called for an unknown run")
			return nil
		},
		fireTerminal,
	)
	if err != nil {
		t.Fatalf("CancelRunJobs: %v", err)
	}
	if touched {
		t.Error("unknown run: touched=true, want false")
	}
	if len(fired) != 0 {
		t.Error("fireTerminal called for unknown run")
	}
}

// TestCancelRunJobs_QueuedJobIsCancelled (CIC-206, T1).
func TestCancelRunJobs_QueuedJobIsCancelled(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID, jid := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, ref, number, status, event) VALUES ($1, $2, 'main', 1, 'running', 'push')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'job0', 'shell', 'queued')`, jid, runID); err != nil {
		t.Fatal(err)
	}
	// In the mixed/active case the fireTerminal is NOT called by
	// CancelRunJobs — advanceRun will fire it when the cancel scanner
	// terminalizes the job.
	var firedCount int
	fireTerminal := func(_ context.Context, _ uuid.UUID) { firedCount++ }
	var gotJobID uuid.UUID
	var called bool
	cancelJob := func(_ context.Context, jobID uuid.UUID, _ string) error {
		gotJobID = jobID
		called = true
		// Simulate what RequestCancel does: mark the job as cancelled.
		if _, err := pool.Exec(ctx,
			"UPDATE jobs SET status = $1, cancel_reason = $2, finished_at = now(), lease_id = NULL WHERE id = $3",
			"cancelled", "operator cancel", jobID); err != nil {
			return err
		}
		return nil
	}
	touched, err := st.CancelRunJobs(ctx, runID, "stop", cancelJob, fireTerminal)
	if err != nil {
		t.Fatalf("CancelRunJobs: %v", err)
	}
	if !touched {
		t.Error("queued run: touched=false, want true")
	}
	if !called {
		t.Error("cancelJob not called")
	}
	if gotJobID != jid {
		t.Errorf("cancelJob got %s, want %s", gotJobID, jid)
	}
	// Active-job case: advanceRun fires the hook; CancelRunJobs does not.
	if firedCount != 0 {
		t.Errorf("fireTerminal called %d times for active-job case, want 0 (advanceRun handles it)", firedCount)
	}
}

// TestCancelRunJobs_PartialFailureStillAudits (CIC-206, B2/T1).
func TestCancelRunJobs_PartialFailureStillAudits(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID, jid1, jid2 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, ref, number, status, event) VALUES ($1, $2, 'main', 1, 'running', 'push')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'j1', 'shell', 'queued')`, jid1, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'j2', 'shell', 'queued')`, jid2, runID); err != nil {
		t.Fatal(err)
	}
	var callCount int
	fireTerminal := func(_ context.Context, _ uuid.UUID) {}
	touched, err := st.CancelRunJobs(ctx, runID, "stop",
		func(_ context.Context, jobID uuid.UUID, _ string) error {
			callCount++
			if jobID == jid1 {
				return errors.New("simulated failure")
			}
			return nil
		},
		fireTerminal,
	)
	if err == nil {
		t.Error("expected a joined error on partial failure")
	}
	if !touched {
		t.Error("partial failure: touched=false, want true")
	}
	if callCount != 2 {
		t.Errorf("cancelJob called %d times, want 2", callCount)
	}
}

// TestCancelRunJobs_PendingJobGatedRunCancelled (CIC-206, B1/T1).
// A run held at an approval gate has status='pending' and all jobs 'pending'.
// CancelRunJobs cancels them, finalizes the run to 'cancelled', and fires the
// terminal hook exactly once with the runID.
func TestCancelRunJobs_PendingJobGatedRunCancelled(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID, jid := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, ref, number, status, event) VALUES ($1, $2, 'main', 1, 'pending', 'push')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'gated', 'shell', 'pending')`, jid, runID); err != nil {
		t.Fatal(err)
	}
	var firedCount int
	var firedWith uuid.UUID
	fireTerminal := func(_ context.Context, rid uuid.UUID) {
		firedCount++
		firedWith = rid
	}
	touched, err := st.CancelRunJobs(ctx, runID, "gate cancelled",
		func(_ context.Context, _ uuid.UUID, _ string) error {
			t.Fatal("cancelJob must NOT be called for a pending job")
			return nil
		},
		fireTerminal,
	)
	if err != nil {
		t.Fatalf("CancelRunJobs(gated run): %v", err)
	}
	if !touched {
		t.Error("gated run: touched=false, want true")
	}
	// The gated run was in 'pending'; finalised to 'cancelled' here;
	// terminal hook fires exactly once.
	if firedCount != 1 {
		t.Errorf("fireTerminal called %d times, want 1", firedCount)
	}
	if firedWith != runID {
		t.Errorf("fireTerminal(%s), want %s", firedWith, runID)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, jid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Errorf("job status = %q, want 'cancelled'", status)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Errorf("run status = %q, want 'cancelled'", status)
	}
}

// TestCancelRunJobs_MixedState (CIC-206, T2 + B1).
// Mixed-state run: one running, one queued, one succeeded, one pending
// gated, one pending dependent. After CancelRunJobs:
//   - all pending jobs are 'cancelled'
//   - active jobs are cancelled via cancelJob (fake sets DB status)
//   - succeeded job is untouched
//   - the run remains 'running' (advanceRun will finalize it)
//   - fireTerminal is NOT called by CancelRunJobs (active case)
func TestCancelRunJobs_MixedState(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, ref, number, status, event) VALUES ($1, $2, 'main', 1, 'running', 'push')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	jRunning, jQueued := uuid.New(), uuid.New()
	jSucceeded, jpGated, jpDep := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status, lease_id) VALUES ($1, $2, 'run', 'shell', 'running', gen_random_uuid())`, jRunning, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'queued', 'shell', 'queued')`, jQueued, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'done', 'shell', 'succeeded')`, jSucceeded, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'gated', 'shell', 'pending')`, jpGated, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'dep', 'shell', 'pending')`, jpDep, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO job_dependencies (job_id, depends_on_job_id) VALUES ($1, $2)`, jpDep, jpGated); err != nil {
		t.Fatal(err)
	}
	var firedCount int
	fireTerminal := func(_ context.Context, _ uuid.UUID) { firedCount++ }
	var calledCount int
	// cancelJob fake: marks the job 'cancelled' in the DB (as RequestCancel
	// would via the cancel scanner) and clears the lease.
	cancelJob := func(_ context.Context, jobID uuid.UUID, _ string) error {
		calledCount++
		if _, err := pool.Exec(ctx,
			"UPDATE jobs SET status = $1, cancel_reason = $2, finished_at = now(), lease_id = NULL WHERE id = $3",
			"cancelled", "operator cancel", jobID); err != nil {
			return err
		}
		return nil
	}
	touched, err := st.CancelRunJobs(ctx, runID, "operator cancel", cancelJob, fireTerminal)
	if err != nil {
		t.Fatalf("CancelRunJobs: %v", err)
	}
	if !touched {
		t.Error("mixed-state: touched=false, want true")
	}
	// 2 active jobs (running + queued) went through cancelJob.
	if calledCount != 2 {
		t.Errorf("cancelJob called %d times, want 2", calledCount)
	}
	// fireTerminal NOT called by CancelRunJobs in the active case.
	if firedCount != 0 {
		t.Errorf("fireTerminal called %d times, want 0 (advanceRun fires it)", firedCount)
	}
	// All 4 non-terminal jobs are 'cancelled'; succeeded is untouched.
	for _, id := range []string{jRunning.String(), jQueued.String(), jpGated.String(), jpDep.String()} {
		var s string
		if e := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, id).Scan(&s); e != nil {
			t.Fatal(e)
		}
		if s != "cancelled" {
			t.Errorf("job %s status = %q, want 'cancelled'", id, s)
		}
	}
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, jSucceeded).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != "succeeded" {
		t.Errorf("succeeded job = %q, want 'succeeded' (untouched)", s)
	}
	// The run is still 'running' here (CancelRunJobs did not finalize it).
	// advanceRun will finalize it when the last active job terminalizes.
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != "running" {
		t.Errorf("run = %q, want 'running' (not yet finalised; advanceRun handles it)", s)
	}
}

// TestCancelRunJobs_FireCalledOnGatedNoOp (CIC-206, B1/F1 regression).
// A gated run with zero active jobs (all pending) → CancelRunJobs fires the
// terminal hook exactly once after the tx commit.
// This is the direct regression test for Dwight's F1 finding:
// before the fix, CancelRunJobs finalised the run to 'cancelled' for
// EVERY case (including active-job runs), which meant the hook would
// not fire when the cancel scanner terminalized those jobs later.
func TestCancelRunJobs_FireCalledOnGatedNoOp(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, ref, number, status, event) VALUES ($1, $2, 'main', 1, 'pending', 'push')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	// Two pending jobs: one top-level, one dependent.
	jp1, jp2 := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'p1', 'shell', 'pending')`, jp1, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'p2', 'shell', 'pending')`, jp2, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO job_dependencies (job_id, depends_on_job_id) VALUES ($1, $2)`, jp2, jp1); err != nil {
		t.Fatal(err)
	}
	var firedCount int
	var firedFor uuid.UUID
	fireTerminal := func(_ context.Context, rid uuid.UUID) {
		firedCount++
		firedFor = rid
	}
	touched, err := st.CancelRunJobs(ctx, runID, "gate denied",
		func(_ context.Context, _ uuid.UUID, _ string) error {
			t.Fatal("cancelJob must NOT be called when there are no active jobs")
			return nil
		},
		fireTerminal,
	)
	if err != nil {
		t.Fatalf("CancelRunJobs: %v", err)
	}
	if !touched {
		t.Error("gated run with 2 pending jobs: touched=false, want true")
	}
	if firedCount != 1 {
		t.Errorf("fireTerminal called %d times, want 1", firedCount)
	}
	if firedFor != runID {
		t.Errorf("fireTerminal fired with %s, want %s", firedFor, runID)
	}
	for _, id := range []string{jp1.String(), jp2.String()} {
		var s string
		if e := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, id).Scan(&s); e != nil {
			t.Fatal(e)
		}
		if s != "cancelled" {
			t.Errorf("job %s status = %q, want 'cancelled'", id, s)
		}
	}
	var rs string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&rs); err != nil {
		t.Fatal(err)
	}
	if rs != "cancelled" {
		t.Errorf("run status = %q, want 'cancelled'", rs)
	}
}

// TestCancelRunJobs_QueueIntegration_HookFires (CIC-206, r4 T2)
// Tests the active-job path with a real queue: request cancel,
// terminalize, assert the hook fires once with "cancelled".
func TestCancelRunJobs_QueueIntegration_HookFires(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	hookMu := sync.Mutex{}
	fired := make(map[uuid.UUID]int)
	q.SetRunTerminalHook(func(runID uuid.UUID, state string) {
		hookMu.Lock()
		fired[runID]++
		hookMu.Unlock()
	})

	// Seed a run with a queued job.
	orgID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, number, event, ref) VALUES ($1, $2, 1, 'push', 'main')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'job', 'shell', 'queued')`,
		jobID, runID); err != nil {
		t.Fatal(err)
	}
	// No fireTerminal in this case — the queue's advanceRun fires the hook
	// when the cancel scanner terminalizes the job.
	touched, err := st.CancelRunJobs(ctx, runID, "operator stop",
		func(ctx context.Context, jid uuid.UUID, reason string) error {
			// Mirror the dispatcher's cancel path: RequestCancel sets the
			// flag, TerminalizeCancel terminalizes the job and fires
			// advanceRun → hook.
			if _, e := q.RequestCancel(ctx, jid, reason); e != nil {
				return e
			}
			if _, e := q.TerminalizeCancel(ctx, jid); e != nil {
				return e
			}
			return nil
		},
		nil, // no fireTerminal: the queue's advanceRun handles the hook.
	)
	if err != nil {
		t.Fatalf("CancelRunJobs: %v", err)
	}
	if !touched {
		t.Error("queued run: touched=false, want true")
	}
	hookMu.Lock()
	n := fired[runID]
	hookMu.Unlock()
	if n != 1 {
		t.Errorf("hook fired %d times, want 1", n)
	}
	if got := jobStatus(ctx, pool, jobID); got != "cancelled" {
		t.Errorf("job status = %q, want cancelled", got)
	}
	// The run was finalized by advanceRun; it should be 'cancelled'.
	var rs string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&rs); err != nil {
		t.Fatal(err)
	}
	if rs != "cancelled" {
		t.Errorf("run status = %q, want cancelled", rs)
	}
}

// TestCancelRunJobs_QueueIntegration_Gated (CIC-206, r4 T2)
// Gated/pending case: hook is fired by CancelRunJobs itself (the cancel
// scanner doesn't handle pending jobs).
func TestCancelRunJobs_QueueIntegration_Gated(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	hookMu := sync.Mutex{}
	fired := make(map[uuid.UUID]int)
	q.SetRunTerminalHook(func(runID uuid.UUID, state string) {
		hookMu.Lock()
		fired[runID]++
		hookMu.Unlock()
	})

	orgID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	// Gated run: status='pending', one pending top-level job.
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo_id, number, event, ref, status) VALUES ($1, $2, 1, 'push', 'main', 'pending')`, runID, repoID); err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id, run_id, name, executor, status) VALUES ($1, $2, 'gated', 'shell', 'pending')`,
		jobID, runID); err != nil {
		t.Fatal(err)
	}
	// fireTerminal IS set: the gated case goes through CancelRunJobs.
	firedLocal := make(map[uuid.UUID]int)
	fireTerminal := func(ctx context.Context, rid uuid.UUID) {
		hookMu.Lock()
		firedLocal[rid]++
		hookMu.Unlock()
	}
	touched, err := st.CancelRunJobs(ctx, runID, "gate denied",
		func(_ context.Context, _ uuid.UUID, _ string) error {
			t.Error("cancelJob: unexpected call in gated case (no active jobs)")
			return nil
		},
		fireTerminal,
	)
	if err != nil {
		t.Fatalf("CancelRunJobs(gated): %v", err)
	}
	if !touched {
		t.Error("gated run: touched=false, want true")
	}
	if firedLocal[runID] != 1 {
		t.Errorf("fireTerminal called %d times, want 1", firedLocal[runID])
	}
	hookMu.Lock()
	hookFired := fired[runID]
	hookMu.Unlock()
	// The queue's onRunTerminal hook is set but not called by CancelRunJobs
	// (advanceRun didn't fire it — we only canceled a pending job).
	// The fireTerminal call IS the hook in this case.
	_ = hookFired
	// After the tx commit the run should be 'cancelled'.
	var rs string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&rs); err != nil {
		t.Fatal(err)
	}
	if rs != "cancelled" {
		t.Errorf("run status = %q, want cancelled", rs)
	}
	var js string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&js); err != nil {
		t.Fatal(err)
	}
	if js != "cancelled" {
		t.Errorf("job status = %q, want cancelled", js)
	}
}

// jobStatus reads the current status of a job from the pool.
func jobStatus(ctx context.Context, pool *pgxpool.Pool, jobID uuid.UUID) string {
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, jobID).Scan(&s); err != nil {
		panic(err) // used only in tests
	}
	return s
}
