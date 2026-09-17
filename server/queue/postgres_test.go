package queue_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
)

const testLeaseTTL = 30 * time.Second

func TestClaimLeasesEligibleJob(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID, withSteps(2))
	runnerID := seedRunner(t, st.Pool(), []string{"linux", "shell"})

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux", "shell"},
		Executors: []string{"shell"},
		Limit:     10,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(claimed))
	}

	got := claimed[0]
	if got.JobID != jobID {
		t.Errorf("claimed job %s, want %s", got.JobID, jobID)
	}
	if got.LeaseID == uuid.Nil {
		t.Error("claim returned a nil lease id; the lease is the execution authority")
	}
	if got.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", got.Attempt)
	}
	if len(got.Steps) != 2 {
		t.Errorf("loaded %d steps, want 2", len(got.Steps))
	}
	if got.Env["CI"] != "true" {
		t.Errorf("env = %v, want CI=true", got.Env)
	}
	if status := jobStatus(t, st.Pool(), jobID); status != "claimed" {
		t.Errorf("job status = %q, want claimed", status)
	}
}

// TestConcurrentClaimNeverDoubleAssigns is the load test CIC-7 calls for: it is
// the property the whole queue rests on. If SKIP LOCKED were dropped, or the
// claim split into a SELECT then a separate UPDATE, this test fails with two
// runners holding the same job — which in production means a deploy running
// twice.
func TestConcurrentClaimNeverDoubleAssigns(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	const (
		jobCount    = 200
		runnerCount = 12
		batchSize   = 7
	)

	runID := seedOrgRepoRun(t, st.Pool())
	want := make(map[uuid.UUID]bool, jobCount)
	for i := 0; i < jobCount; i++ {
		want[seedJob(t, st.Pool(), runID)] = true
	}

	runners := make([]uuid.UUID, runnerCount)
	for i := range runners {
		runners[i] = seedRunner(t, st.Pool(), []string{"linux", "shell"})
	}

	var (
		mu      sync.Mutex
		claimed = make(map[uuid.UUID]uuid.UUID) // job -> runner that got it
		leases  = make(map[uuid.UUID]uuid.UUID) // job -> lease
		dupes   []uuid.UUID
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)

	for i := 0; i < runnerCount; i++ {
		wg.Add(1)
		go func(runnerID uuid.UUID) {
			defer wg.Done()
			<-start // maximize the overlap; this is the whole point of the test
			for {
				batch, err := q.Claim(ctx, queue.ClaimRequest{
					RunnerID:  runnerID,
					Labels:    []string{"linux", "shell"},
					Executors: []string{"shell"},
					Limit:     batchSize,
					LeaseTTL:  testLeaseTTL,
				})
				if err != nil {
					t.Errorf("runner %s claim: %v", runnerID, err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, j := range batch {
					if prior, seen := claimed[j.JobID]; seen {
						dupes = append(dupes, j.JobID)
						t.Errorf("job %s claimed twice: by %s and %s", j.JobID, prior, runnerID)
					}
					claimed[j.JobID] = runnerID
					leases[j.JobID] = j.LeaseID
				}
				mu.Unlock()
			}
		}(runners[i])
	}

	close(start)
	wg.Wait()

	if len(dupes) > 0 {
		t.Fatalf("%d jobs were claimed more than once", len(dupes))
	}
	if len(claimed) != jobCount {
		t.Fatalf("claimed %d distinct jobs, want all %d", len(claimed), jobCount)
	}
	for jobID := range want {
		if _, ok := claimed[jobID]; !ok {
			t.Errorf("job %s was never claimed", jobID)
		}
	}

	// Every lease handed out must be distinct: the lease is what authorizes
	// execution, so two jobs sharing one would let a report for one job be
	// accepted against the other.
	seenLease := make(map[uuid.UUID]bool, len(leases))
	for jobID, lease := range leases {
		if lease == uuid.Nil {
			t.Errorf("job %s got a nil lease", jobID)
		}
		if seenLease[lease] {
			t.Errorf("lease %s was issued for more than one job", lease)
		}
		seenLease[lease] = true
	}

	var queuedLeft int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE status = 'queued'`).Scan(&queuedLeft); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if queuedLeft != 0 {
		t.Errorf("%d jobs left queued after the fleet drained the queue", queuedLeft)
	}
}

func TestClaimRespectsTagsAndExecutor(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	untagged := seedJob(t, st.Pool(), runID)
	gpuJob := seedJob(t, st.Pool(), runID, withTags("linux", "gpu"))
	dockerJob := seedJob(t, st.Pool(), runID, withExecutor("docker"))
	runnerID := seedRunner(t, st.Pool(), []string{"linux", "shell"})

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux", "shell"},
		Executors: []string{"shell"},
		Limit:     10,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].JobID != untagged {
		t.Fatalf("claimed %d jobs, want only the untagged shell job", len(claimed))
	}
	if s := jobStatus(t, st.Pool(), gpuJob); s != "queued" {
		t.Errorf("gpu job status = %q; a runner without the gpu label must not get it", s)
	}
	if s := jobStatus(t, st.Pool(), dockerJob); s != "queued" {
		t.Errorf("docker job status = %q; a shell-only runner must not get it", s)
	}
}

// A runner with no labels at all must still be able to claim jobs that require
// no tags. This guards the SQL detail that `required_tags <@ NULL` is NULL
// rather than true, which would silently starve such a runner.
func TestClaimWithNoLabels(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID)
	runnerID := seedRunner(t, st.Pool(), []string{})

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    nil,
		Executors: []string{"shell"},
		Limit:     5,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].JobID != jobID {
		t.Fatalf("an unlabelled runner claimed %d jobs, want the untagged job", len(claimed))
	}
}

func TestClaimOrdersByPriorityThenAge(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	seedJob(t, st.Pool(), runID, withPriority(100))
	urgent := seedJob(t, st.Pool(), runID, withPriority(1))
	runnerID := seedRunner(t, st.Pool(), []string{"linux"})

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux"},
		Executors: []string{"shell"},
		Limit:     1,
		LeaseTTL:  testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].JobID != urgent {
		t.Fatalf("claim did not take the highest-priority job first")
	}
}

func TestPendingJobsAreNotClaimable(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	pending := seedJob(t, st.Pool(), runID, withStatus("pending"))
	runnerID := seedRunner(t, st.Pool(), []string{"linux"})

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 10, LeaseTTL: testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d pending jobs; jobs blocked on `needs` must not be claimable", len(claimed))
	}

	if err := q.Enqueue(ctx, pending); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed, err = q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 10, LeaseTTL: testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("claim after enqueue: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d jobs after enqueue, want 1", len(claimed))
	}
}

// The zombie guard. A runner that was out of contact long enough to lose its
// lease must not be able to overwrite the result of whoever re-ran the job.
func TestCompleteRejectsStaleLease(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID)
	runnerID := seedRunner(t, st.Pool(), []string{"linux"})

	first, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 1, LeaseTTL: testLeaseTTL,
	})
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %v (got %d jobs)", err, len(first))
	}
	staleLease := first[0].LeaseID

	if _, err := q.Requeue(ctx, jobID, "runner went away", 0); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	second, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 1, LeaseTTL: testLeaseTTL,
	})
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim: %v (got %d jobs)", err, len(second))
	}
	if second[0].LeaseID == staleLease {
		t.Fatal("requeue reissued the same lease id; a re-run must be distinguishable from a zombie")
	}

	err = q.Complete(ctx, queue.Completion{
		JobID: jobID, LeaseID: staleLease, State: queue.StateSucceeded,
	})
	if !errors.Is(err, queue.ErrStaleLease) {
		t.Fatalf("completing against a stale lease returned %v, want ErrStaleLease", err)
	}
	if s := jobStatus(t, st.Pool(), jobID); s != "claimed" {
		t.Errorf("job status = %q; the stale report must not have applied", s)
	}

	if err := q.Complete(ctx, queue.Completion{
		JobID: jobID, LeaseID: second[0].LeaseID, State: queue.StateSucceeded,
	}); err != nil {
		t.Fatalf("completing against the live lease: %v", err)
	}
	if s := jobStatus(t, st.Pool(), jobID); s != "succeeded" {
		t.Errorf("job status = %q, want succeeded", s)
	}
}

func TestRenewLeaseReportsAbandonedJobs(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	keep := seedJob(t, st.Pool(), runID)
	lose := seedJob(t, st.Pool(), runID)
	runnerID := seedRunner(t, st.Pool(), []string{"linux"})

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 10, LeaseTTL: testLeaseTTL,
	})
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim: %v (got %d)", err, len(claimed))
	}

	// Simulate the server revoking one job while the runner was out of contact.
	if _, err := q.Requeue(ctx, lose, "swept", 0); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	abandon, err := q.RenewLease(ctx, runnerID, []uuid.UUID{keep, lose}, testLeaseTTL)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if len(abandon) != 1 || abandon[0] != lose {
		t.Fatalf("abandon = %v, want exactly [%s]", abandon, lose)
	}
}

// A runner that dies mid-job must cost one job's latency, not a job that hangs
// forever. The sweeper is what makes that true.
func TestSweepRequeuesExpiredLeases(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID)
	runnerID := seedRunner(t, st.Pool(), []string{"linux"})

	if _, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 1, LeaseTTL: time.Second,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Sweep as of a point past the lease expiry rather than sleeping for it.
	swept, err := q.SweepExpiredLeases(ctx, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].JobID != jobID {
		t.Fatalf("swept %v, want the one expired job", swept)
	}
	if swept[0].Outcome != queue.OutcomeRequeued {
		t.Errorf("outcome = %q, want requeued", swept[0].Outcome)
	}
	if s := jobStatus(t, st.Pool(), jobID); s != "queued" {
		t.Errorf("job status = %q, want queued", s)
	}

	var leaseID *uuid.UUID
	if err := st.Pool().QueryRow(ctx, `SELECT lease_id FROM jobs WHERE id = $1`, jobID).Scan(&leaseID); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if leaseID != nil {
		t.Error("a swept job still holds a lease; the old runner could still report against it")
	}
}

// A job that reliably kills its runner must eventually stop, rather than
// cycling through the fleet forever.
func TestRequeueDeadLettersAfterMaxAttempts(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID)
	runnerID := seedRunner(t, st.Pool(), []string{"linux"})

	var maxAttempts int
	if err := st.Pool().QueryRow(ctx, `SELECT max_attempts FROM jobs WHERE id = $1`, jobID).Scan(&maxAttempts); err != nil {
		t.Fatalf("read max_attempts: %v", err)
	}

	for i := 1; i <= maxAttempts; i++ {
		claimed, err := q.Claim(ctx, queue.ClaimRequest{
			RunnerID: runnerID, Labels: []string{"linux"},
			Executors: []string{"shell"}, Limit: 1, LeaseTTL: testLeaseTTL,
		})
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if len(claimed) != 1 {
			t.Fatalf("attempt %d claimed %d jobs, want 1", i, len(claimed))
		}
		outcome, err := q.Requeue(ctx, jobID, "runner exploded", 0)
		if err != nil {
			t.Fatalf("requeue %d: %v", i, err)
		}
		want := queue.OutcomeRequeued
		if i == maxAttempts {
			want = queue.OutcomeDeadLettered
		}
		if outcome != want {
			t.Fatalf("attempt %d: outcome = %q, want %q", i, outcome, want)
		}
	}

	if s := jobStatus(t, st.Pool(), jobID); s != "dead_letter" {
		t.Errorf("job status = %q, want dead_letter", s)
	}
}

// Backoff must actually hide the job: a requeue with a delay that has not
// elapsed should not be claimable yet.
func TestRequeueBackoffDelaysClaim(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	runID := seedOrgRepoRun(t, st.Pool())
	jobID := seedJob(t, st.Pool(), runID)
	runnerID := seedRunner(t, st.Pool(), []string{"linux"})

	if _, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 1, LeaseTTL: testLeaseTTL,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := q.Requeue(ctx, jobID, "transient", time.Hour); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID: runnerID, Labels: []string{"linux"},
		Executors: []string{"shell"}, Limit: 1, LeaseTTL: testLeaseTTL,
	})
	if err != nil {
		t.Fatalf("claim during backoff: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed %d jobs during backoff, want 0", len(claimed))
	}
}
