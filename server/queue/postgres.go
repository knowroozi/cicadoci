package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotifyChannel is the LISTEN/NOTIFY channel the schema's trigger publishes to
// when a job becomes claimable.
const NotifyChannel = "cicado_job_queued"

// CancelChannel is the LISTEN/NOTIFY channel the dispatcher watches for cancel
// requests. It is a separate name from NotifyChannel so the two doorbells —
// job-queued and cancel-requested — can wake the single dispatch loop without
// one clogging the other. Both listeners share the same process-level dispatch
// loop via Dispatcher.Wake and Dispatcher.WakeCancel, but keeping them on
// separate channels means a burst of cancels can't starve a burst of queue
// notifications, and vice versa.
const CancelChannel = "cicado_job_cancel"

// ErrStaleLease is returned when a runner reports against a lease that is no
// longer the job's live lease — the job was requeued, reassigned, or cancelled
// while the runner was out of contact. The report is rejected, not applied.
var ErrStaleLease = errors.New("queue: stale lease")

// ErrNotRequeueable is returned when a job's status no longer matches any of
// the states Requeue is willing to act on. This is expected, not exceptional:
// under concurrent sweeps (or a sweep racing a runner's own completion
// report), two callers can both select the same expired lease before either
// one's Requeue commits — whichever runs second finds the job already moved
// on (e.g. dead-lettered by the first) and gets this instead of a match.
var ErrNotRequeueable = errors.New("queue: job is not requeueable")

// Postgres is the Postgres-backed Queue.
type Postgres struct {
	pool *pgxpool.Pool
	// onRunTerminal is read on every job completion and written once at
	// startup; atomic so the race detector agrees.
	onRunTerminal atomic.Pointer[RunTerminalHook]
}

// NewPostgres builds a Queue over an existing pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

var _ Queue = (*Postgres)(nil)

// enqueueSQL flips a 'pending' job to 'queued'. Kept as one shared constant rather
// than inlined separately into each call site so that Enqueue (pool-path) and the
// transaction-scoped EnqueueInTx below can only ever drift apart if someone edits
// this exact spot -- the whole point of CIC-12's atomic-enqueue change is that run
// creation and its jobs reaching 'queued' stay a single unit, so a second
// independently-worded copy of the same statement would quietly reintroduce a
// dual-write gap no review could easily catch by eye.
const enqueueSQL = `
UPDATE jobs
   SET status = 'queued'
 WHERE id = $1
   AND status = 'pending'`

// enqueuer is anything that can run one update. Both the connection pool and an
// open transaction satisfy it, which is what lets Enqueue and EnqueueInTx share a
// single body instead of carrying each its own copy.
type enqueuer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var (
	_ enqueuer = (*pgxpool.Pool)(nil)
	_ enqueuer = pgx.Tx(nil)
)

func enqueue(ctx context.Context, target enqueuer, jobID uuid.UUID) error {
	tag, err := target.Exec(ctx, enqueueSQL, jobID)
	if err != nil {
		return fmt.Errorf("queue: enqueue %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("queue: job %s is not pending", jobID)
	}
	return nil
}

// Enqueue makes a pending job claimable through the connection pool. The schema's
// AFTER UPDATE trigger fires the doorbell at COMMIT, so a dispatcher woken by the
// notification is guaranteed to see the row.
func (q *Postgres) Enqueue(ctx context.Context, jobID uuid.UUID) error {
	return enqueue(ctx, q.pool, jobID)
}

// EnqueueInTx makes a pending job claimable inside an already-open transaction,
// rather than via its own statement on the pool. This is the shape CIC-12's
// scheduler needs: creating a run and its jobs and enqueuing them are one unit of
// work, so they must commit together or not at all -- a job that exists as 'pending'
// but never made it to 'queued' would leave stranded work nothing wakes up for. The
// NOTIFY trigger still rings only at COMMIT (exactly as before), so this changes when
// the enqueue happens relative to run creation, not when anyone can observe or claim
// the job: both a standalone Enqueue and an in-transaction one become visible and
// ring that same doorbell at precisely their own commit moment.
func (q *Postgres) EnqueueInTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) error {
	return enqueue(ctx, tx, jobID)
}

// claimSQL is the heart of the queue.
//
// The inner SELECT takes row locks with SKIP LOCKED, so two dispatchers racing
// for the same batch step over each other's locked rows instead of serializing
// on them: each gets a disjoint set, neither blocks, and no job is ever handed
// to two runners. The outer UPDATE then mints a fresh lease_id, which is the
// authority the runner will quote on every subsequent message about this job.
//
// Ordering matches jobs_claim_idx exactly (priority, created_at) so the scan
// stops at LIMIT rather than sorting the whole queue.
//
// Eligibility has three parts:
//   - required_tags <@ labels: the runner carries every tag the job demands
//   - executor = ANY(executors): the runner can actually run it
//   - available_at <= now(): retry backoff has elapsed
const claimSQL = `
UPDATE jobs
   SET status           = 'claimed',
       claimed_by       = $1,
       claimed_at       = now(),
       lease_id         = gen_random_uuid(),
       lease_expires_at = now() + ($2::double precision * interval '1 second'),
       attempts         = attempts + 1
 WHERE id IN (
       SELECT id
         FROM jobs
        WHERE status = 'queued'
          AND available_at <= now()
          AND required_tags <@ $3::text[]
          AND executor = ANY($4::text[])
          AND cancel_requested_at IS NULL
        ORDER BY priority, created_at
          FOR UPDATE SKIP LOCKED
        LIMIT $5
 )
RETURNING id, run_id, lease_id, lease_expires_at, name, executor, env,
          secret_refs, timeout_seconds, attempts,
          coalesce(git_clone_url, ''), coalesce(git_ref, ''), coalesce(git_sha, '')`

// startRunsSQL moves the runs of freshly claimed jobs from pending to running
// (CIC-103). The run rows are locked in id order first so two claims that
// touch the same runs cannot deadlock on each other. The status guard leaves a
// run that is already running, or already finalised, alone.
const startRunsSQL = `
UPDATE runs
   SET status = 'running', started_at = coalesce(started_at, now())
 WHERE id IN (SELECT id FROM runs
               WHERE id = ANY($1::uuid[]) AND status = 'pending'
               ORDER BY id
                 FOR UPDATE)`

// Claim leases up to req.Limit eligible jobs to a runner. The claim and the
// pending -> running move of each claimed job's run commit together.
func (q *Postgres) Claim(ctx context.Context, req ClaimRequest) ([]ClaimedJob, error) {
	if req.Limit <= 0 {
		return nil, nil
	}
	if req.LeaseTTL <= 0 {
		return nil, errors.New("queue: claim requires a positive lease TTL")
	}
	// Normalize nil to empty: `required_tags <@ NULL` is NULL, not true, which
	// would silently make every job ineligible for a runner with no labels.
	labels := req.Labels
	if labels == nil {
		labels = []string{}
	}
	executors := req.Executors
	if executors == nil {
		executors = []string{}
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("queue: claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, claimSQL,
		req.RunnerID, req.LeaseTTL.Seconds(), labels, executors, req.Limit)
	if err != nil {
		return nil, fmt.Errorf("queue: claim: %w", err)
	}
	defer rows.Close()

	var claimed []ClaimedJob
	for rows.Next() {
		var j ClaimedJob
		var envJSON []byte
		if err := rows.Scan(
			&j.JobID, &j.RunID, &j.LeaseID, &j.LeaseExpiresAt, &j.Name, &j.Executor,
			&envJSON, &j.SecretRefs, &j.TimeoutSeconds, &j.Attempt,
			&j.GitCloneURL, &j.GitRef, &j.GitSHA,
		); err != nil {
			return nil, fmt.Errorf("queue: scan claimed job: %w", err)
		}
		if err := json.Unmarshal(envJSON, &j.Env); err != nil {
			return nil, fmt.Errorf("queue: decode env for job %s: %w", j.JobID, err)
		}
		claimed = append(claimed, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: claim rows: %w", err)
	}
	rows.Close()

	if len(claimed) > 0 {
		runIDs := make([]uuid.UUID, len(claimed))
		for i, j := range claimed {
			runIDs[i] = j.RunID
		}
		if _, err := tx.Exec(ctx, startRunsSQL, runIDs); err != nil {
			return nil, fmt.Errorf("queue: claim: start runs: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("queue: claim: commit: %w", err)
	}

	// Steps are fetched per job rather than joined into the claim, so the claim
	// query stays a single-row-per-job UPDATE holding locks for as little time
	// as possible. This is one extra round trip per claimed job (not per step —
	// loadSteps returns a job's whole step list in one query), so a full batch
	// is at most Limit+1 round trips. Acceptable at MVP scale (jobs table has
	// tens of steps at most); a future batch JOIN across all claimed job IDs
	// would trade that for one query if this ever shows up in a profile.
	for i := range claimed {
		steps, err := q.loadSteps(ctx, claimed[i].JobID)
		if err != nil {
			return nil, err
		}
		claimed[i].Steps = steps
	}
	return claimed, nil
}

func (q *Postgres) loadSteps(ctx context.Context, jobID uuid.UUID) ([]Step, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT step_index, name, coalesce(image, ''), command, env,
		       timeout_seconds, network_enabled, coalesce(if_condition, '')
		  FROM steps
		 WHERE job_id = $1
		 ORDER BY step_index`, jobID)
	if err != nil {
		return nil, fmt.Errorf("queue: load steps for %s: %w", jobID, err)
	}
	defer rows.Close()

	var steps []Step
	for rows.Next() {
		var s Step
		var envJSON []byte
		if err := rows.Scan(&s.Index, &s.Name, &s.Image, &s.Command, &envJSON,
			&s.TimeoutSeconds, &s.NetworkEnabled, &s.If); err != nil {
			return nil, fmt.Errorf("queue: scan step: %w", err)
		}
		if err := json.Unmarshal(envJSON, &s.Env); err != nil {
			return nil, fmt.Errorf("queue: decode step env: %w", err)
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: step rows: %w", err)
	}
	return steps, nil
}

// RenewLease extends leases for jobs a runner claims to still be executing, and
// reports back the ones it must abandon. A runner that was partitioned long
// enough for its lease to be swept will be told to drop those jobs rather than
// silently continuing to run work someone else now owns.
func (q *Postgres) RenewLease(ctx context.Context, runnerID uuid.UUID, jobIDs []uuid.UUID, ttl time.Duration) ([]uuid.UUID, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}
	if ttl <= 0 {
		return nil, errors.New("queue: renew requires a positive lease TTL")
	}

	rows, err := q.pool.Query(ctx, `
		UPDATE jobs
		   SET lease_expires_at = now() + ($3::double precision * interval '1 second')
		 WHERE id = ANY($2::uuid[])
		   AND claimed_by = $1
		   AND status IN ('claimed', 'running')
		RETURNING id`, runnerID, jobIDs, ttl.Seconds())
	if err != nil {
		return nil, fmt.Errorf("queue: renew leases: %w", err)
	}
	defer rows.Close()

	renewed := make(map[uuid.UUID]bool, len(jobIDs))
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("queue: scan renewed lease: %w", err)
		}
		renewed[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: renew rows: %w", err)
	}

	var abandon []uuid.UUID
	for _, id := range jobIDs {
		if !renewed[id] {
			abandon = append(abandon, id)
		}
	}
	return abandon, nil
}

// MarkRunning moves a claimed job to running. Like every other runner-driven
// transition it is gated on the live lease.
func (q *Postgres) MarkRunning(ctx context.Context, jobID, leaseID uuid.UUID) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		   SET status     = 'running',
		       started_at = coalesce(started_at, now())
		 WHERE id = $1 AND lease_id = $2 AND status IN ('claimed', 'running')`,
		jobID, leaseID)
	if err != nil {
		return fmt.Errorf("queue: mark running: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %s", ErrStaleLease, jobID)
	}
	return nil
}

// Complete records a terminal state. The lease_id predicate is the zombie
// guard: a runner that comes back from a long partition holding a lease the
// server has since revoked cannot overwrite the result of whoever re-ran the
// job in the meantime.
//
// The same transaction advances the job's DAG and, when this was the run's
// last job, finalises the run (see advanceRun).
func (q *Postgres) Complete(ctx context.Context, res Completion) error {
	switch res.State {
	case StateSucceeded, StateFailed, StateCancelled:
	default:
		return fmt.Errorf("queue: %q is not a terminal state", res.State)
	}

	return q.inTx(ctx, func(tx pgx.Tx) (runOutcome, error) {
		return complete(ctx, tx, res)
	})
}

func complete(ctx context.Context, tx pgx.Tx, res Completion) (runOutcome, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE jobs
		   SET status       = $3,
		       exit_code    = $4,
		       error_class  = nullif($5, ''),
		       error_detail = nullif($6, ''),
		       finished_at  = now(),
		       last_lease_id    = lease_id,
		       lease_id         = NULL,
		       lease_expires_at = NULL
		 WHERE id = $1 AND lease_id = $2 AND status IN ('claimed', 'running')`,
		res.JobID, res.LeaseID, string(res.State), res.ExitCode,
		res.ErrorClass, res.ErrorDetail)
	if err != nil {
		return runOutcome{}, fmt.Errorf("queue: complete job %s: %w", res.JobID, err)
	}
	if tag.RowsAffected() == 0 {
		return runOutcome{}, fmt.Errorf("%w: job %s", ErrStaleLease, res.JobID)
	}
	return advanceRun(ctx, tx, res.JobID)
}

// requeueSQL returns a job to the queue, or dead-letters it when it has no
// attempts left. Clearing lease_id is what makes any still-running runner's
// next report fall foul of the zombie guard.
const requeueSQL = `
UPDATE jobs
   SET status = CASE WHEN attempts >= max_attempts THEN 'dead_letter' ELSE 'queued' END,
       available_at = CASE WHEN attempts >= max_attempts
                           THEN available_at
                           ELSE now() + ($2::double precision * interval '1 second') END,
       error_detail = CASE WHEN attempts >= max_attempts THEN $3 ELSE error_detail END,
       error_class  = CASE WHEN attempts >= max_attempts THEN 'infra' ELSE error_class END,
       finished_at  = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
       last_lease_id    = coalesce(lease_id, last_lease_id),
       lease_id         = NULL,
       claimed_by       = NULL,
       claimed_at       = NULL,
       lease_expires_at = NULL
 WHERE id = $1 AND status IN ('claimed', 'running', 'queued')
RETURNING status, attempts`

// Requeue returns a job for another attempt, or dead-letters it. A dead letter
// is terminal, so it advances the DAG and may finalise the run, in the same
// transaction (see advanceRun). This covers the sweeper's lease-expiry path.
func (q *Postgres) Requeue(ctx context.Context, jobID uuid.UUID, reason string, backoff time.Duration) (RequeueOutcome, error) {
	var outcome RequeueOutcome
	err := q.inTx(ctx, func(tx pgx.Tx) (runOutcome, error) {
		var status string
		var attempts int32
		err := tx.QueryRow(ctx, requeueSQL, jobID, backoff.Seconds(), reason).
			Scan(&status, &attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return runOutcome{}, fmt.Errorf("%w: job %s", ErrNotRequeueable, jobID)
		}
		if err != nil {
			return runOutcome{}, fmt.Errorf("queue: requeue %s: %w", jobID, err)
		}
		if status != "dead_letter" {
			outcome = OutcomeRequeued
			return runOutcome{}, nil
		}
		outcome = OutcomeDeadLettered
		return advanceRun(ctx, tx, jobID)
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}

// SweepExpiredLeases is the orphan-recovery path: it finds jobs whose lease ran
// out without a terminal status — a runner that crashed, lost its network, or
// was terminated by an autoscaler — and returns them to the queue.
//
// This is what makes a lost runner cost one job's latency instead of a job that
// hangs forever.
//
// The 500-row cap per call is a production guardrail, not a correctness
// limit: a fleet outage affecting more than 500 jobs just takes multiple
// passes to fully drain. That only stays cheap if whoever calls this on a
// timer picks an interval well above 500 * lease TTL — otherwise consecutive
// sweeps overlap the same backlog instead of making progress through it.
func (q *Postgres) SweepExpiredLeases(ctx context.Context, now time.Time) ([]SweptJob, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, coalesce(claimed_by, '00000000-0000-0000-0000-000000000000'::uuid), attempts, max_attempts
		  FROM jobs
		 WHERE status IN ('claimed', 'running')
		   AND lease_expires_at IS NOT NULL
		   AND lease_expires_at < $1
		 ORDER BY lease_expires_at
		 LIMIT 500`, now)
	if err != nil {
		return nil, fmt.Errorf("queue: find expired leases: %w", err)
	}

	type expired struct {
		jobID    uuid.UUID
		runnerID uuid.UUID
		attempts int32
	}
	var found []expired
	for rows.Next() {
		var e expired
		var maxAttempts int32
		if err := rows.Scan(&e.jobID, &e.runnerID, &e.attempts, &maxAttempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("queue: scan expired lease: %w", err)
		}
		found = append(found, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: expired lease rows: %w", err)
	}

	var swept []SweptJob
	for _, e := range found {
		// Backoff grows with attempts so a job that reliably kills its runner
		// does not spin through the fleet taking runners down with it.
		backoff := time.Duration(1<<min(e.attempts, 6)) * time.Second
		outcome, err := q.Requeue(ctx, e.jobID, "lease expired without a terminal status", backoff)
		if errors.Is(err, ErrNotRequeueable) {
			// Another server instance's sweep pass (or this job finishing on
			// its own between the SELECT above and this Requeue) already moved
			// this job past the states Requeue acts on. That is success, not
			// failure — the recovery this job needed already happened — and
			// must not abort the rest of this batch, which is exactly what a
			// fleet outage needs swept promptly rather than one job at a time
			// across separate 15s ticks.
			continue
		}
		if err != nil {
			return nil, err
		}
		swept = append(swept, SweptJob{
			JobID:    e.jobID,
			RunnerID: e.runnerID,
			Outcome:  outcome,
			Attempts: e.attempts,
		})
	}
	return swept, nil
}

// ---------------------------------------------------------------------------
// Cancellation
//

// requestCancelSQL flips the durable cancel flag on a non-terminal row. The
// status guard makes the call idempotent against a finish that races the
// cancel: terminal rows are skipped so a cancel of an already-succeeded job is
// a no-op, not an overwrite.
//
// claimed_by returns NULL for a job that was never claimed; the Go caller
// converts that to uuid.Nil.
const requestCancelSQL = `
UPDATE jobs
   SET cancel_requested_at = now(),
       cancel_reason       = $2
 WHERE id = $1
   AND status IN ('queued', 'claimed', 'running')
RETURNING claimed_by
`

// RequestCancel marks a job cancelled and rings the cancel doorbell.
//
// It is a single atomic UPDATE ... RETURNING: no read-then-write window. If a
// concurrent terminal StatusUpdate landed first, the status guard causes
// RETURNING to return zero rows; this call then reports Active = false (a
// cancel of a finished job is a no-op). If the job was still in-flight, the
// flag is written and claimed_by returned, so the caller learns the owner in
// one round trip.
//
// The two halves of a cancellation are split across the system on purpose:
//   - the row flag (cancel_requested_at) is the durable, long-lived record. A
//     job cancelled while unclaimed has no session to reach, so the flag suffices:
//     the claim query skips cancelled jobs, and the dispatcher terminalizes it.
//   - the CancelJob pushed to the runner's live session is the fast path for
//     stopping execution in flight. The dispatcher sends it; it is the
//     dispatcher that owns push-or-retry, not this function.
//
// Idempotency: a repeat cancel refreshes the timestamp and reason with no
// side-effect on the runner (Runner's cancelJob is a per-context cancel).
func (q *Postgres) RequestCancel(ctx context.Context, jobID uuid.UUID, reason string) (CancelResult, error) {
	// claimed_by is NULL for an unclaimed (queued) job. pgx sets the
	// pointer to nil in that case; we detect it without a coalesce/zero-literal
	// in the RETURNING clause.
	var owner *uuid.UUID
	err := q.pool.QueryRow(ctx, requestCancelSQL, jobID, reason).Scan(&owner)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already terminal, or the job does not exist.
			return CancelResult{}, nil
		}
		return CancelResult{}, fmt.Errorf("queue: request cancel %s: %w", jobID, err)
	}

	runnerID := uuid.Nil
	if owner != nil {
		runnerID = *owner
	}

	// Ring the cancel doorbell so a dispatcher wakes and pushes the job.
	_, _ = q.pool.Exec(ctx, "SELECT pg_notify($1, $2)",
		[]byte(CancelChannel), mustMarshalCancel(jobID))

	return CancelResult{Active: true, RunnerID: runnerID}, nil
}

func mustMarshalCancel(jobID uuid.UUID) []byte {
	b, err := json.Marshal(QueuedNotification{JobID: jobID.String()})
	if err != nil {
		panic(fmt.Sprintf("queue: marshal cancel notification for %s: %v", jobID, err))
	}
	return b
}

// cancelScanSQL is the dispatcher's cancel-scan SELECT: one row per job that
// has a pending cancel and is not yet terminal.
//
// The status IN ('queued', 'claimed', 'running') guard mirrors the same guard
// in the Requeue SQL — a job that finished between the cancel request and this
// scan is out of scope, and the dispatcher ignores it.
const cancelScanSQL = `
SELECT id,
       coalesce(claimed_by, '00000000-0000-0000-0000-000000000000'::uuid),
       cancel_reason
  FROM jobs
 WHERE cancel_requested_at IS NOT NULL
   AND status IN ('queued', 'claimed', 'running')
 ORDER BY cancel_requested_at
 LIMIT $1
`

// terminalCancelSQL marks a queued-but-unclaimed job as terminal CANCELLED.
// Called by the dispatcher's cancel scan when it sees a cancel requested for a
// job that no runner owns — the job is in the queue and never started.
//
// The status = 'queued' guard makes it safe to call even after a runner has
// claimed and already finished the job: that terminal write is authoritative
// and this UPDATE matches zero rows.
const terminalCancelSQL = `
UPDATE jobs
   SET status       = 'cancelled',
       finished_at  = now()
 WHERE id = $1
   AND status = 'queued'
`

// QueryCancelPending returns one row per job that has a pending cancel and has
// not yet reached a terminal state. The dispatcher iterates these rows each tick.
func (q *Postgres) QueryCancelPending(ctx context.Context, limit int) (pgx.Rows, error) {
	return q.pool.Query(ctx, cancelScanSQL, int32(limit))
}

// TerminalizeCancel marks a queued-but-unclaimed cancelled job as terminal
// CANCELLED. The dispatcher calls this for each row returned by
// QueryCancelPending with owner == zero-uuid.
//
// The status = 'queued' guard means this is safe even if a race re-queued the
// job between the scan and this call.
//
// A cancelled job is terminal, so it advances the DAG and may finalise the
// run, in the same transaction (see advanceRun).
func (q *Postgres) TerminalizeCancel(ctx context.Context, jobID uuid.UUID) (int32, error) {
	var n int32
	err := q.inTx(ctx, func(tx pgx.Tx) (runOutcome, error) {
		tag, err := tx.Exec(ctx, terminalCancelSQL, jobID)
		if err != nil {
			return runOutcome{}, fmt.Errorf("queue: terminalize cancel %s: %w", jobID, err)
		}
		n = int32(tag.RowsAffected())
		if n == 0 {
			return runOutcome{}, nil
		}
		return advanceRun(ctx, tx, jobID)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// DAG advance and run rollup (CIC-103)
//

// RunTerminalHook is called once per run, after the transaction that moved
// the run's last job to a terminal state has committed. state is the run's
// final status: "succeeded", "failed" or "cancelled".
type RunTerminalHook func(runID uuid.UUID, state string)

// SetRunTerminalHook installs the hook that fires when a run finishes. A nil
// hook disables it. Every terminal job transition in this package (Complete,
// a Requeue that dead-letters, TerminalizeCancel) goes through advanceRun, so
// one hook here covers the runner path, the sweeper and the cancel scan alike.
func (q *Postgres) SetRunTerminalHook(h RunTerminalHook) {
	if h == nil {
		q.onRunTerminal.Store(nil)
		return
	}
	q.onRunTerminal.Store(&h)
}

// runOutcome is what advanceRun did to the job's run. state is empty unless
// this call is the one that finalised the run.
type runOutcome struct {
	runID uuid.UUID
	state string
}

// fire calls the run-terminal hook when o finalised a run. It must run only
// after the transaction that produced o has committed: a hook that reads the
// run (forge.Complete, a backpressure release) must see the final state.
func (q *Postgres) fire(o runOutcome) {
	if o.state == "" {
		return
	}
	if h := q.onRunTerminal.Load(); h != nil {
		(*h)(o.runID, o.state)
	}
}

// lockRunSQL takes the run row's lock for the rest of the transaction. Every
// terminal transition of a job takes it after writing the job row, so two jobs
// of one run finishing at the same moment are serialised here. Without it, two
// READ COMMITTED transactions could each see the other's job as still running:
// neither would promote a join job that needs both, and neither would finalise
// the run.
const lockRunSQL = `
SELECT j.run_id, j.status
  FROM jobs j
  JOIN runs r ON r.id = j.run_id
 WHERE j.id = $1
   FOR UPDATE OF r`

// promoteSQL queues every pending direct dependent of $1 whose needs have all
// succeeded. The finished job itself is never matched (it is not its own
// dependent, and the status guard excludes anything not pending). Grandchildren
// are not touched: they become eligible when their own needs finish. The
// jobs_queued_notify trigger rings the dispatcher for each promoted row.
const promoteSQL = `
UPDATE jobs d
   SET status = 'queued', available_at = now()
 WHERE d.status = 'pending'
   AND d.id IN (SELECT job_id FROM job_dependencies WHERE depends_on_job_id = $1)
   AND NOT EXISTS (
         SELECT 1
           FROM job_dependencies e
           JOIN jobs p ON p.id = e.depends_on_job_id
          WHERE e.job_id = d.id
            AND p.status <> 'succeeded')`

// cancelDescendantsSQL cancels every pending job downstream of $1, children
// and grandchildren alike: a job whose ancestor did not succeed can never have
// all its needs met, so leaving it pending would hang the run forever. UNION
// (not UNION ALL) keeps a diamond from visiting the join twice.
const cancelDescendantsSQL = `
WITH RECURSIVE descendants(id) AS (
    SELECT job_id FROM job_dependencies WHERE depends_on_job_id = $1
    UNION
    SELECT e.job_id
      FROM job_dependencies e
      JOIN descendants d ON e.depends_on_job_id = d.id
)
UPDATE jobs
   SET status        = 'cancelled',
       finished_at   = now(),
       cancel_reason = coalesce(cancel_reason, 'an upstream job did not succeed')
 WHERE id IN (SELECT id FROM descendants)
   AND status = 'pending'`

// finaliseRunSQL sets the run's final status once every job is terminal. The
// status guard makes it single-winner: only the first transaction to see the
// run complete gets a row back, and only that one fires the hook. A failed or
// dead-lettered job fails the run; otherwise any cancelled job cancels it.
const finaliseRunSQL = `
UPDATE runs r
   SET status      = s.state,
       started_at  = coalesce(r.started_at, now()),
       finished_at = now()
  FROM (SELECT bool_and(status IN ('succeeded', 'failed', 'cancelled', 'skipped', 'dead_letter')) AS done,
               CASE WHEN bool_or(status IN ('failed', 'dead_letter')) THEN 'failed'
                    WHEN bool_or(status = 'cancelled')                THEN 'cancelled'
                    ELSE 'succeeded' END AS state
          FROM jobs
         WHERE run_id = $1) s
 WHERE r.id = $1
   AND s.done
   AND r.status IN ('pending', 'running')
RETURNING r.status`

// advanceRun moves the DAG on after jobID reached a terminal state inside tx:
// on success it queues the dependents that are now unblocked, on any other
// outcome it cancels everything downstream, and when no job of the run is left
// running it finalises the run. It runs in the same transaction as the job's
// terminal write, so a crash cannot leave a finished job with a stalled DAG.
func advanceRun(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) (runOutcome, error) {
	var out runOutcome
	var jobState string
	if err := tx.QueryRow(ctx, lockRunSQL, jobID).Scan(&out.runID, &jobState); err != nil {
		return runOutcome{}, fmt.Errorf("queue: lock run of job %s: %w", jobID, err)
	}

	switch jobState {
	case string(StateSucceeded):
		if _, err := tx.Exec(ctx, promoteSQL, jobID); err != nil {
			return runOutcome{}, fmt.Errorf("queue: promote dependents of %s: %w", jobID, err)
		}
	case string(StateFailed), string(StateCancelled), "dead_letter":
		if _, err := tx.Exec(ctx, cancelDescendantsSQL, jobID); err != nil {
			return runOutcome{}, fmt.Errorf("queue: cancel dependents of %s: %w", jobID, err)
		}
	default:
		return runOutcome{}, fmt.Errorf("queue: advance run: job %s is %q, not terminal", jobID, jobState)
	}

	err := tx.QueryRow(ctx, finaliseRunSQL, out.runID).Scan(&out.state)
	if errors.Is(err, pgx.ErrNoRows) {
		return runOutcome{runID: out.runID}, nil
	}
	if err != nil {
		return runOutcome{}, fmt.Errorf("queue: finalise run %s: %w", out.runID, err)
	}
	return out, nil
}

// inTx runs fn in a transaction and commits it. fn reports whether a run was
// finalised; the hook fires only after the commit succeeds.
func (q *Postgres) inTx(ctx context.Context, fn func(pgx.Tx) (runOutcome, error)) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("queue: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out, err := fn(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("queue: commit: %w", err)
	}
	q.fire(out)
	return nil
}
