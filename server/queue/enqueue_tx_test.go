package queue_test

import (
	"context"
	"fmt"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
)

// This file bundles CIC-12's queue-constraint checks, all proven end-to-end against a live
// Postgres by actual claim/sweep calls rather than by shape assertion alone:
//
// - The atomic-enqueue pair proves EnqueueInTx (this branch only) closes the gap left after
//   creating a job and enqueuing it as two separate statements could leave stranded work in
//   between -- commit (it lands, claimable just like out-of-band one) + rollback (nothing did).
//
// - The sweep-cap test proves SweepExpiredLeases' hard 500-per-call guardrail really is enforced,
//   that a first call drains exactly 500 and a second drains only whatever's left, never more.

func TestEnqueuedJobCommitsWithRunCreation(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runID := seedOrgRepoRun(t, st.Pool()) // run already committed; the job's creation is the piece under test here.

	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // harmless no-op if Commit succeeds below -- same idiom as devseed.go.

	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, "INSERT INTO jobs (run_id, name, executor) VALUES ($1, 'echo', 'shell') RETURNING id", runID).Scan(&jobID); err != nil {
		t.Fatalf("insert pending job: %v", err)
	}

	// The enqueue is part of the very same transaction as the insert above; a single Commit must do it all.
	if err := q.EnqueueInTx(ctx, tx, jobID); err != nil {
		t.Fatalf("enqueue in-tx: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// After that single commit the job must be visible AND claimable just like an out-of-band one would be --
	// proving nothing partially queued got left behind by doing the enqueue inside the transaction.
	if s := jobStatus(t, st.Pool(), jobID); s != "queued" {
		t.Errorf("job status = %q after commit; want queued", s)
	}

	runnerID := seedRunner(t, st.Pool(), []string{"linux"})
	jobs, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux"},
		Executors: []string{"shell"},
		Limit:     1,
		LeaseTTL:  time.Second,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(jobs) != 1 || jobs[0].JobID != jobID {
		t.Fatalf("claimed %d job(s); want exactly the one enqueued in-tx", len(jobs))
	}
}

// The other half of the atomic guarantee. Creating the run and its job must not commit "the row exists"
// on top of a transaction that never commits -- without EnqueueInTx flipping pending->queued inside that same tx,
// there used to be no such guarantee at all (a crash between two separate statements could leave an orphan).
func TestEnqueuedJobIsAbsentAfterRollback(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runID := seedOrgRepoRun(t, st.Pool()) // committed independently; only the job and its enqueue live in tx under test.

	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, "INSERT INTO jobs (run_id, name, executor) VALUES ($1, 'echo', 'shell') RETURNING id", runID).Scan(&jobID); err != nil {
		t.Fatalf("insert pending job: %v", err)
	}

	// Flip pending -> queued in-tx, then roll the whole thing back: both must vanish together.
	if err := q.EnqueueInTx(ctx, tx, jobID); err != nil {
		t.Fatalf("enqueue in-tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// After rollback the row must be gone entirely -- proven both directly (no such id exists anywhere after)
	// and via the queue's own claim view, which has to find no work for it.
	var status string
	err = st.Pool().QueryRow(ctx, "SELECT status FROM jobs WHERE id = $1", jobID).Scan(&status)
	var isNoRows bool
	if errors.Is(err, pgx.ErrNoRows) {
		isNoRows = true
	} else if err != nil {
		t.Fatalf("lookup rolled-back job: %v", err)
	}
	if !isNoRows {
		// The row survived rollback -- exactly the partial-enqueue failure this ticket exists to rule out.
		t.Errorf("rolled-back job still present with status %q; expected it not to exist at all after rollback", status)
	}

	jobs, err := q.Claim(ctx, queue.ClaimRequest{RunnerID: seedRunner(t, st.Pool(), []string{"linux"}), Labels: []string{"linux"}, Executors: []string{"shell"}, Limit: 1, LeaseTTL: time.Second})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("claimed %d job(s); a rolled-back enqueue must leave nothing to claim", len(jobs))
	}
}

// SweepExpiredLeases caps at 500 per call as a production guardrail, not a correctness limit:
// after sweeping 501 jobs in one go it must only recover the first 500 that pass, and a second
// sweep picks up exactly the remainder. Without this cap, a genuine fleet outage (hundreds of
// crashed runners at once) could take a single sweep call arbitrarily long inside one transaction.

// SweepExpiredLeases caps at 500 rows per call as a production guardrail against unbounded work in a
// single transaction, not as a correctness limit (a genuine fleet outage simply drains across multiple
// sweeps). This proves that cap really is enforced -- and cheaply so: rather than trying to build one
// complex set-generating SQL expression by hand, it just loops 501 individual inserts the same way
// production would enqueue many small jobs, keeping this assertion simple and readable.
func TestSweepCapsAtFiveHundredPerCall(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runID := seedOrgRepoRun(t, st.Pool()) // one run to hang all of these off; they are not related by needs.

	const want = 501
	// Seed more expired 'claimed' leases than the cap can recover in one pass, so a second call is needed.
	for i := 1; i <= int(want); i++ {
		if _, err := st.Pool().Exec(ctx, `
			INSERT INTO jobs (run_id, name, executor, status, attempts, max_attempts, lease_id, claimed_by, lease_expires_at)
			VALUES ($1, $2, 'shell', 'claimed', 0, 3, gen_random_uuid(), NULL, now() - interval '2 hours')`, runID, fmt.Sprintf("sweep-%d", i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	swept, err := q.SweepExpiredLeases(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(swept) != 500 {
		t.Fatalf("first sweep returned %d jobs; want exactly 500 (the per-call cap)", len(swept))
	}
	for _, s := range swept {
		if s.Outcome != queue.OutcomeRequeued {
			t.Errorf("sweep outcome = %q; want requeued", s.Outcome)
		}
	}

	remaining, err := q.SweepExpiredLeases(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("second sweep returned %d job(s); want exactly the 1 not recovered by the cap this pass", len(remaining))
	}

	last, err := q.SweepExpiredLeases(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	if len(last) != 0 {
		t.Errorf("third sweep returned %d job(s); want none; all were recovered across the first two sweeps combined", len(last))
	}
}
