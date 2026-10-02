package queue_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
)

// CIC-103: DAG promotion, downstream cancellation and the run-terminal rollup.

// hookRecorder counts run-terminal hook calls per run.
type hookRecorder struct {
	mu    sync.Mutex
	calls map[uuid.UUID][]string
}

func recordHook(q *queue.Postgres) *hookRecorder {
	r := &hookRecorder{calls: map[uuid.UUID][]string{}}
	q.SetRunTerminalHook(func(runID uuid.UUID, state string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls[runID] = append(r.calls[runID], state)
	})
	return r
}

func (r *hookRecorder) get(runID uuid.UUID) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls[runID]...)
}

// dagJob seeds a job: queued when it has no needs, pending otherwise, with one
// job_dependencies edge per need, as store.CreateJobSpecs writes them.
func dagJob(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, needs ...uuid.UUID) uuid.UUID {
	t.Helper()
	status := "queued"
	if len(needs) > 0 {
		status = "pending"
	}
	id := seedJob(t, pool, runID, withStatus(status))
	for _, n := range needs {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO job_dependencies (job_id, depends_on_job_id) VALUES ($1, $2)`,
			id, n); err != nil {
			t.Fatalf("seed dependency: %v", err)
		}
	}
	return id
}

// leaseJob puts a job in 'running' under a fresh lease, as a claim would, and
// returns the lease.
func leaseJob(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) uuid.UUID {
	t.Helper()
	var lease uuid.UUID
	if err := pool.QueryRow(context.Background(), `
		UPDATE jobs
		   SET status = 'running', lease_id = gen_random_uuid(),
		       lease_expires_at = now() + interval '1 hour', attempts = attempts + 1
		 WHERE id = $1
		RETURNING lease_id`, jobID).Scan(&lease); err != nil {
		t.Fatalf("lease job: %v", err)
	}
	return lease
}

// finish completes a job through the queue's Complete, the runner's path.
func finish(t *testing.T, q *queue.Postgres, pool *pgxpool.Pool, jobID uuid.UUID, state queue.TerminalState) {
	t.Helper()
	lease := leaseJob(t, pool, jobID)
	if err := q.Complete(context.Background(), queue.Completion{
		JobID: jobID, LeaseID: lease, State: state,
	}); err != nil {
		t.Fatalf("complete %s: %v", jobID, err)
	}
}

func runStatus(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return s
}

func expectJobs(t *testing.T, pool *pgxpool.Pool, want map[uuid.UUID]string, names map[uuid.UUID]string) {
	t.Helper()
	for id, w := range want {
		if got := jobStatus(t, pool, id); got != w {
			t.Errorf("job %s status = %q, want %q", names[id], got, w)
		}
	}
}

func expectHook(t *testing.T, r *hookRecorder, runID uuid.UUID, want ...string) {
	t.Helper()
	got := r.get(runID)
	if len(got) != len(want) {
		t.Fatalf("hook calls for run = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hook calls for run = %v, want %v", got, want)
		}
	}
}

// A → B → C: each success unblocks exactly the next level, the finished job is
// never put back in the queue, and the last success finalises the run.
func TestDAGLinearChain(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)

	runID := seedOrgRepoRun(t, pool)
	a := dagJob(t, pool, runID)
	b := dagJob(t, pool, runID, a)
	c := dagJob(t, pool, runID, b)
	names := map[uuid.UUID]string{a: "A", b: "B", c: "C"}

	finish(t, q, pool, a, queue.StateSucceeded)
	expectJobs(t, pool, map[uuid.UUID]string{a: "succeeded", b: "queued", c: "pending"}, names)
	expectHook(t, hook, runID)

	finish(t, q, pool, b, queue.StateSucceeded)
	expectJobs(t, pool, map[uuid.UUID]string{a: "succeeded", b: "succeeded", c: "queued"}, names)
	if got := runStatus(t, pool, runID); got != "pending" {
		t.Errorf("run status mid-DAG = %q, want pending", got)
	}

	finish(t, q, pool, c, queue.StateSucceeded)
	if got := runStatus(t, pool, runID); got != "succeeded" {
		t.Errorf("run status = %q, want succeeded", got)
	}
	expectHook(t, hook, runID, "succeeded")
}

// A → {B, C} → D: the join D waits for both needs, then the run succeeds.
func TestDAGDiamondSucceeds(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)

	runID := seedOrgRepoRun(t, pool)
	a := dagJob(t, pool, runID)
	b := dagJob(t, pool, runID, a)
	c := dagJob(t, pool, runID, a)
	d := dagJob(t, pool, runID, b, c)
	names := map[uuid.UUID]string{a: "A", b: "B", c: "C", d: "D"}

	finish(t, q, pool, a, queue.StateSucceeded)
	expectJobs(t, pool, map[uuid.UUID]string{b: "queued", c: "queued", d: "pending"}, names)

	finish(t, q, pool, b, queue.StateSucceeded)
	expectJobs(t, pool, map[uuid.UUID]string{c: "queued", d: "pending"}, names)

	finish(t, q, pool, c, queue.StateSucceeded)
	expectJobs(t, pool, map[uuid.UUID]string{d: "queued"}, names)

	finish(t, q, pool, d, queue.StateSucceeded)
	if got := runStatus(t, pool, runID); got != "succeeded" {
		t.Errorf("run status = %q, want succeeded", got)
	}
	expectHook(t, hook, runID, "succeeded")
}

// A → B → C, A → D: A fails, so its children and grandchildren are cancelled,
// nothing is queued, and the run fails.
func TestDAGFailureCancelsGrandchildren(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)

	runID := seedOrgRepoRun(t, pool)
	a := dagJob(t, pool, runID)
	b := dagJob(t, pool, runID, a)
	c := dagJob(t, pool, runID, b)
	d := dagJob(t, pool, runID, a)
	names := map[uuid.UUID]string{a: "A", b: "B", c: "C", d: "D"}

	finish(t, q, pool, a, queue.StateFailed)
	expectJobs(t, pool, map[uuid.UUID]string{a: "failed", b: "cancelled", c: "cancelled", d: "cancelled"}, names)
	if got := runStatus(t, pool, runID); got != "failed" {
		t.Errorf("run status = %q, want failed", got)
	}
	expectHook(t, hook, runID, "failed")
}

// A → {B, C} → D: B succeeds, C fails. The join must not be promoted on B's
// success alone, and is cancelled once C fails.
func TestDAGJoinWithFailedNeedIsCancelled(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)

	runID := seedOrgRepoRun(t, pool)
	a := dagJob(t, pool, runID)
	b := dagJob(t, pool, runID, a)
	c := dagJob(t, pool, runID, a)
	d := dagJob(t, pool, runID, b, c)
	names := map[uuid.UUID]string{a: "A", b: "B", c: "C", d: "D"}

	finish(t, q, pool, a, queue.StateSucceeded)
	finish(t, q, pool, b, queue.StateSucceeded)
	expectJobs(t, pool, map[uuid.UUID]string{d: "pending"}, names)

	finish(t, q, pool, c, queue.StateFailed)
	expectJobs(t, pool, map[uuid.UUID]string{b: "succeeded", c: "failed", d: "cancelled"}, names)
	if got := runStatus(t, pool, runID); got != "failed" {
		t.Errorf("run status = %q, want failed", got)
	}
	expectHook(t, hook, runID, "failed")
}

// The sweeper path: a job whose lease expired with no attempts left is
// dead-lettered by Requeue, which must cascade and finalise like a failure.
func TestDAGDeadLetterCancelsDependents(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)
	ctx := context.Background()

	runID := seedOrgRepoRun(t, pool)
	a := dagJob(t, pool, runID)
	b := dagJob(t, pool, runID, a)
	c := dagJob(t, pool, runID, b)
	names := map[uuid.UUID]string{a: "A", b: "B", c: "C"}

	if _, err := pool.Exec(ctx, `UPDATE jobs SET max_attempts = 1 WHERE id = $1`, a); err != nil {
		t.Fatalf("set max_attempts: %v", err)
	}
	leaseJob(t, pool, a)
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, a); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	swept, err := q.SweepExpiredLeases(ctx, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].Outcome != queue.OutcomeDeadLettered {
		t.Fatalf("swept = %+v, want one dead-lettered job", swept)
	}
	expectJobs(t, pool, map[uuid.UUID]string{a: "dead_letter", b: "cancelled", c: "cancelled"}, names)
	if got := runStatus(t, pool, runID); got != "failed" {
		t.Errorf("run status = %q, want failed", got)
	}
	expectHook(t, hook, runID, "failed")
}

// The cancel-scan path: cancelling the only queued job finalises the run as
// cancelled.
func TestTerminalizeCancelFinalisesRun(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)

	runID := seedOrgRepoRun(t, pool)
	a := dagJob(t, pool, runID)
	b := dagJob(t, pool, runID, a)

	n, err := q.TerminalizeCancel(context.Background(), a)
	if err != nil || n != 1 {
		t.Fatalf("terminalize cancel = %d, %v; want 1, nil", n, err)
	}
	if got := jobStatus(t, pool, b); got != "cancelled" {
		t.Errorf("dependent status = %q, want cancelled", got)
	}
	if got := runStatus(t, pool, runID); got != "cancelled" {
		t.Errorf("run status = %q, want cancelled", got)
	}
	expectHook(t, hook, runID, "cancelled")
}

// Many jobs of one run finishing at the same moment: the run is finalised and
// the hook fires exactly once. Without the run-row lock, two READ COMMITTED
// transactions can each see the other's job as still running and nobody
// finalises; without the status guard, several do.
func TestRunTerminalHookFiresOnceUnderConcurrency(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)

	const runs, jobsPerRun = 5, 8
	for r := 0; r < runs; r++ {
		runID := seedOrgRepoRun(t, pool)
		type leased struct{ job, lease uuid.UUID }
		var ls []leased
		for i := 0; i < jobsPerRun; i++ {
			j := dagJob(t, pool, runID)
			ls = append(ls, leased{j, leaseJob(t, pool, j)})
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, l := range ls {
			wg.Add(1)
			go func(l leased) {
				defer wg.Done()
				<-start
				if err := q.Complete(context.Background(), queue.Completion{
					JobID: l.job, LeaseID: l.lease, State: queue.StateSucceeded,
				}); err != nil {
					t.Errorf("complete: %v", err)
				}
			}(l)
		}
		close(start)
		wg.Wait()

		if got := runStatus(t, pool, runID); got != "succeeded" {
			t.Errorf("run %d status = %q, want succeeded", r, got)
		}
		if got := hook.get(runID); len(got) != 1 {
			t.Errorf("run %d hook calls = %v, want exactly one", r, got)
		}
	}
}

// A → {B, C} → D with B and C finishing concurrently: whichever commits second
// must see the first and promote D.
func TestDAGConcurrentJoinPromotes(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	for r := 0; r < 10; r++ {
		runID := seedOrgRepoRun(t, pool)
		a := dagJob(t, pool, runID)
		b := dagJob(t, pool, runID, a)
		c := dagJob(t, pool, runID, a)
		d := dagJob(t, pool, runID, b, c)
		finish(t, q, pool, a, queue.StateSucceeded)

		lb, lc := leaseJob(t, pool, b), leaseJob(t, pool, c)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, l := range [][2]uuid.UUID{{b, lb}, {c, lc}} {
			wg.Add(1)
			go func(job, lease uuid.UUID) {
				defer wg.Done()
				<-start
				if err := q.Complete(context.Background(), queue.Completion{
					JobID: job, LeaseID: lease, State: queue.StateSucceeded,
				}); err != nil {
					t.Errorf("complete: %v", err)
				}
			}(l[0], l[1])
		}
		close(start)
		wg.Wait()

		if got := jobStatus(t, pool, d); got != "queued" {
			t.Fatalf("iteration %d: join status = %q, want queued", r, got)
		}
	}
}

// A run is finalised once. A job that finishes after its run was finalised
// (for example one created late by a second workflow) must neither rewrite the
// run's status nor fire the hook again.
func TestRunFinalisedOnlyOnce(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	hook := recordHook(q)

	runID := seedOrgRepoRun(t, pool)
	finish(t, q, pool, dagJob(t, pool, runID), queue.StateSucceeded)
	expectHook(t, hook, runID, "succeeded")

	finish(t, q, pool, dagJob(t, pool, runID), queue.StateFailed)
	if got := runStatus(t, pool, runID); got != "succeeded" {
		t.Errorf("run status = %q, want it to stay succeeded", got)
	}
	expectHook(t, hook, runID, "succeeded")
}

// A run moves from pending to running when its first job is claimed, and a
// later claim does not move it back or touch its start time.
func TestClaimStartsRun(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	q := queue.NewPostgres(pool)
	ctx := context.Background()

	runID := seedOrgRepoRun(t, pool)
	dagJob(t, pool, runID)
	dagJob(t, pool, runID)
	runnerID := seedRunner(t, pool, []string{})

	claim := func() {
		t.Helper()
		got, err := q.Claim(ctx, queue.ClaimRequest{
			RunnerID: runnerID, Executors: []string{"shell"}, Limit: 1, LeaseTTL: testLeaseTTL,
		})
		if err != nil || len(got) != 1 {
			t.Fatalf("claim = %d jobs, %v; want 1, nil", len(got), err)
		}
	}
	readRun := func() (string, time.Time) {
		t.Helper()
		var s string
		var started *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT status, started_at FROM runs WHERE id = $1`, runID).Scan(&s, &started); err != nil {
			t.Fatalf("read run: %v", err)
		}
		if started == nil {
			return s, time.Time{}
		}
		return s, *started
	}

	if s, _ := readRun(); s != "pending" {
		t.Fatalf("run status before claim = %q, want pending", s)
	}
	claim()
	s, first := readRun()
	if s != "running" || first.IsZero() {
		t.Fatalf("run after first claim = %q started %v, want running with a start time", s, first)
	}
	claim()
	if s, again := readRun(); s != "running" || !again.Equal(first) {
		t.Errorf("run after second claim = %q started %v, want running started %v", s, again, first)
	}
}
