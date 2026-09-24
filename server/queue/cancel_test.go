package queue_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
)

// TestRequestCancelQueuedJob reports Active=true with a zero RunnerID when the
// job has not yet been claimed. The dispatcher will terminalize it; no session
// exists to push a CancelJob to.
func TestRequestCancelQueuedJob(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID, withExecutor("shell"))

	res, err := q.RequestCancel(ctx, jobID, "user request")
	if err != nil {
		t.Fatalf("RequestCancel(queued job): %v", err)
	}
	if !res.Active {
		t.Fatalf("request cancel of queued job: Active=false, want true")
	}
	if res.RunnerID != uuid.Nil {
		t.Fatalf("request cancel of queued job: RunnerID=%s, want nil", res.RunnerID)
	}
}

// TestRequestCancelClaimedJob reports Active=true with the owning runner's ID so
// the dispatcher can push a CancelJob to the right session.
func TestRequestCancelClaimedJob(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	runnerID := seedRunner(t, st.Pool(), []string{"linux", "shell"})
	jobID := seedJob(t, st.Pool(), runID, withExecutor("shell"))

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux", "shell"},
		Executors: []string{"shell"},
		Limit:     10,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("Claim returned %d jobs, want 1", len(claimed))
	}

	res, err := q.RequestCancel(ctx, jobID, "user request")
	if err != nil {
		t.Fatalf("RequestCancel(claimed job): %v", err)
	}
	if !res.Active {
		t.Fatalf("request cancel of claimed job: Active=false, want true")
	}
	if res.RunnerID != runnerID {
		t.Fatalf("request cancel of claimed job: RunnerID=%s, want %s", res.RunnerID, runnerID)
	}
}

// TestRequestCancelTerminalJob is a no-op: a job that has already reached a
// terminal status is not marked cancelled and reports Active=false.
func TestRequestCancelTerminalJob(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID, withStatus("succeeded"))

	res, err := q.RequestCancel(ctx, jobID, "user request")
	if err != nil {
		t.Fatalf("RequestCancel(succeeded job): %v", err)
	}
	if res.Active {
		t.Fatalf("request cancel of terminal job: Active=true, want false")
	}
}

// TestQueryCancelPendingReturnsQueuedJobs verifies that the cancel scan
// returns only rows that have a cancel flag and are not yet terminal.
func TestQueryCancelPendingReturnsQueuedJobs(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	queuedJob := seedJob(t, st.Pool(), runID, withExecutor("shell"))
	terminalJob := seedJob(t, st.Pool(), runID, withStatus("failed"))
	uncancelledJob := seedJob(t, st.Pool(), runID, withExecutor("shell"))

	if _, err := q.RequestCancel(ctx, queuedJob, "user request"); err != nil {
		t.Fatalf("RequestCancel(queued): %v", err)
	}
	// A terminal job: RequestCancel is a no-op, no flag gets written.
	if _, err := q.RequestCancel(ctx, terminalJob, "after finish"); err != nil {
		t.Fatalf("RequestCancel(terminal, no-op): %v", err)
	}
	_ = uncancelledJob

	rows, err := q.QueryCancelPending(ctx, 10)
	if err != nil {
		t.Fatalf("QueryCancelPending: %v", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id, owner uuid.UUID
		var reason string
		if err := rows.Scan(&id, &owner, &reason); err != nil {
			t.Fatalf("rows.Scan: %v", err)
		}
		ids = append(ids, id)
		_ = reason
	}
	if len(ids) != 1 || ids[0] != queuedJob {
		t.Fatalf("QueryCancelPending: got %v, want exactly [%s]", ids, queuedJob)
	}
}

// TestTerminalizeCancelOnQueuedJob verifies that TerminalizeCancel marks an
// unclaimed queued job as terminal and returns 1 affected row.
func TestTerminalizeCancelOnQueuedJob(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID, withExecutor("shell"))

	// RequestCancel sets the flag on a queued job.
	if _, err := q.RequestCancel(ctx, jobID, "user request"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	affected, err := q.TerminalizeCancel(ctx, jobID)
	if err != nil {
		t.Fatalf("TerminalizeCancel: %v", err)
	}
	if affected != 1 {
		t.Fatalf("TerminalizeCancel affected %d rows, want 1", affected)
	}
	if got := jobStatus(t, st.Pool(), jobID); got != "cancelled" {
		t.Fatalf("TerminalizeCancel status = %q, want %q", got, "cancelled")
	}
}

// TestTerminalizeCancelNoopWhenAlreadyClaimed verifies that TerminalizeCancel
// is a no-op when the job is claimed (race guard).
func TestTerminalizeCancelNoopWhenAlreadyClaimed(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	runnerID := seedRunner(t, st.Pool(), []string{"linux", "shell"})
	jobID := seedJob(t, st.Pool(), runID, withExecutor("shell"))

	// Claim the job so it has a valid lease and status='claimed'.
	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux", "shell"},
		Executors: []string{"shell"},
		Limit:     10,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	_ = claimed

	// Request cancel sets the flag; the job is claimed, not queued.
	if _, err := q.RequestCancel(ctx, jobID, "user request"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	// status='queued' guard means TerminalizeCancel skips it.
	affected, err := q.TerminalizeCancel(ctx, jobID)
	if err != nil {
		t.Fatalf("TerminalizeCancel: %v", err)
	}
	if affected != 0 {
		t.Fatalf("TerminalizeCancel claimed job: affected %d rows, want 0", affected)
	}
	if got := jobStatus(t, st.Pool(), jobID); got != "claimed" {
		t.Fatalf("TerminalizeCancel (claimed job) flipped status to %q, want 'claimed' unchanged", got)
	}
}

// TestClaimSkipsCancelledJob verifies that the claim SQL guard prevents
// handing out a job that has a cancel flag.
func TestClaimSkipsCancelledJob(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	runnerID := seedRunner(t, st.Pool(), []string{"linux", "shell"})
	jobID := seedJob(t, st.Pool(), runID, withExecutor("shell"))

	if _, err := q.RequestCancel(ctx, jobID, "pre-claim cancel"); err != nil {
		t.Fatalf("RequestCancel(queued job): %v", err)
	}

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux", "shell"},
		Executors: []string{"shell"},
		Limit:     10,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("Claim returned %d jobs for a cancelled job, want 0", len(claimed))
	}
}

// TestQueryCancelPendingIgnoresRunningJobWithFlag verifies that running/claimed
// jobs with a cancel flag appear in the cancel scan, so the dispatcher can push
// a CancelJob to the owning session.
func TestQueryCancelPendingIgnoresRunningJobWithFlag(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	runnerID := seedRunner(t, st.Pool(), []string{"linux", "shell"})
	jobID := seedJob(t, st.Pool(), runID, withExecutor("shell"))

	// Claim the job so it has a valid lease and status='claimed'.
	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux", "shell"},
		Executors: []string{"shell"},
		Limit:     10,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	_ = claimed

	if _, err := q.RequestCancel(ctx, jobID, "user request"); err != nil {
		t.Fatalf("RequestCancel(claimed job): %v", err)
	}

	rows, err := q.QueryCancelPending(ctx, 10)
	if err != nil {
		t.Fatalf("QueryCancelPending: %v", err)
	}
	defer rows.Close()

	var gotIDs []uuid.UUID
	for rows.Next() {
		var id, owner uuid.UUID
		var reason string
		if err := rows.Scan(&id, &owner, &reason); err != nil {
			t.Fatalf("rows.Scan: %v", err)
		}
		gotIDs = append(gotIDs, id)
		_ = reason
	}
	if len(gotIDs) != 1 || gotIDs[0] != jobID {
		t.Fatalf("QueryCancelPending: got %v, want exactly [%s]", gotIDs, jobID)
	}
}

// TestCancelChannelIsDefined is a compile-time sentinel.
func TestCancelChannelIsDefined(t *testing.T) {
	if queue.CancelChannel == "" {
		t.Fatal("CancelChannel is empty")
	}
}

// TestCicanscriptExecutorAccepted verifies that migration 0002 widens the CHECK
// constraint so a job can be persisted with executor='cicanscript'.  Before
// this migration, NewExecutor("cicanscript") was unreachable end-to-end: the
// SQL INSERT would fail and no job could carry executor='cicanscript'.
func TestCicanscriptExecutorAccepted(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()

	// seedJob with executor='cicanscript' succeeds only if the CHECK
	// constraint has been widened by migration 0002.
	runID := seedOrgRepoRun(t, st.Pool())
	seedJob(t, st.Pool(), runID, withExecutor("cicanscript"))
	// Reaching the end of this test means the INSERT/seedJob succeeded.
}

// Ensure pgx is imported (the row iterators use pgx.ErrNoRows).
var _ = pgx.ErrNoRows
