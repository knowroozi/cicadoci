// Package dispatch claims queued jobs and pushes them to connected runners.
//
// The loop is woken by the queue's LISTEN/NOTIFY doorbell and also ticks on a
// timer. Both are needed: the doorbell makes the common case fast, and the tick
// makes every case correct, because a notification published while this process
// was reconnecting is simply lost — NOTIFY has no redelivery.
package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	uuid "github.com/google/uuid"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/runnersvc"
)

// Config tunes the dispatch loop.
type Config struct {
	// PollInterval is the safety net behind the doorbell.
	PollInterval time.Duration
	// SweepInterval is how often expired leases are reclaimed.
	SweepInterval time.Duration
	// LeaseTTL is how long a claim is good for without a heartbeat.
	LeaseTTL time.Duration
	// MaxBatch caps how many jobs are claimed for one runner at a time.
	MaxBatch int
	// UndeliveredBackoff delays a job requeued because the runner it was just
	// claimed for turned out to be unreachable. Zero would make it claimable
	// again immediately — fine for one bad send, but a runner that stays
	// unreachable (full outbound buffer, mid-disconnect) would then be
	// re-claimed and re-rejected on every dispatch pass with no delay between
	// attempts.
	UndeliveredBackoff time.Duration
	// MaxCancelBatch caps how many pending cancels are processed per pass. It
	// mirrors MaxBatch so a burst of cancel requests cannot produce a burst of
	// CancelJob pushes on one pass.
	MaxCancelBatch int
	// CancelGraceSeconds is the grace window a CancelJob tells the runner to
	// honour before it kills in-flight work. A larger value gives a job more
	// time to finish cleanly; a zero value means "cancel immediately."
	CancelGraceSeconds uint32

	// Backpressure, when non-nil, enforces a system-level concurrency cap
	// in dispatchOnce.  When Acquire returns false for a claimed job,
	// the job is requeued with UndeliveredBackoff (same as an
	// unreachable runner).  The cap key is job.RunID as a proxy for
	// org/repo; a future ticket that adds RepoID to ClaimedJob can
	// use the proper per-repo key.
	Backpressure *backpressure.Tracker
}

// DefaultConfig returns sensible defaults.
func DefaultConfig(leaseTTL time.Duration) Config {
	return Config{
		PollInterval:       5 * time.Second,
		SweepInterval:      15 * time.Second,
		LeaseTTL:           leaseTTL,
		MaxBatch:           8,
		UndeliveredBackoff: 2 * time.Second,
		MaxCancelBatch:     8,
		CancelGraceSeconds: 5,
		// Backpressure: nil by default; caller provides a tracker from
		// controller.DefaultConfig().Backpressure.
	}
}

// Dispatcher assigns work to connected runners.
type Dispatcher struct {
	queue    *queue.Postgres
	sessions *runnersvc.SessionRegistry
	cfg      Config
	log      *slog.Logger

	// wake and cancelWake are two independent level-triggered channels: one for
	// new job-queued notifications, one for new cancel requests. Both feed the
	// same dispatch loop so they do not compete for attention, and each one
	// being level-triggered means many simultaneous notifications cause exactly
	// one more pass, not one per notification.
	wake       chan struct{}
	cancelWake chan struct{}
	mu         sync.Mutex
}

// New builds a dispatcher.
func New(q *queue.Postgres, sessions *runnersvc.SessionRegistry, cfg Config, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		queue:      q,
		sessions:   sessions,
		cfg:        cfg,
		log:        log,
		wake:       make(chan struct{}, 1),
		cancelWake: make(chan struct{}, 1),
	}
}

// Wake asks for a dispatch pass. It never blocks.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// WakeCancel asks for a cancel pass. It never blocks and is level-triggered like
// Wake, so a flood of cancel requests causes exactly one more cancel pass, not
// one per request.
func (d *Dispatcher) WakeCancel() {
	select {
	case d.cancelWake <- struct{}{}:
	default:
	}
}

// Run drives the dispatch loop until the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	poll := time.NewTicker(d.cfg.PollInterval)
	defer poll.Stop()
	sweep := time.NewTicker(d.cfg.SweepInterval)
	defer sweep.Stop()

	// Run one pass of each kind on startup so jobs and cancels that arrived
	// before this process started are picked up immediately.
	d.dispatchOnce(ctx)
	d.dispatchCancelOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.wake:
			d.dispatchOnce(ctx)
		case <-d.cancelWake:
			d.dispatchCancelOnce(ctx)
		case <-poll.C:
			d.dispatchOnce(ctx)
			d.dispatchCancelOnce(ctx)
		case <-sweep.C:
			d.sweepOnce(ctx)
		}
	}
}

// dispatchCancelOnce handles one round of pending cancellations. There are two
// shapes a cancel can take, and the dispatcher handles them differently:
//
// Claimed, live session case. The job the cancel targets has a runner attached
// to it through a currently connected gRPC session. The cancel push is a
// best-effort send: if the outbound buffer is full the push is dropped, but the
// same row will be seen again on the next pass since the flag outlives the
// attempt. When the runner does receive the CancelJob it stops in-flight
// execution within GraceSeconds and sends a terminal CANCELLED StatusUpdate.
// The flag is intentionally not cleared: the terminal StatusUpdate transitions
// the job out of the scan window, and a redundant cancel of an already-finishing
// job is harmless because the runner's cancel logic is idempotent.
//
// Unclaimed case: a cancel arrives while the job is still in the queue, before
// a runner has claimed it. The claim query skips cancelled jobs, so the
// dispatcher is the only authority that can move such a job to a terminal state.
// It writes directly to the jobs table rather than sending a CancelJob to a
// session that does not exist.
func (d *Dispatcher) dispatchCancelOnce(ctx context.Context) {
	rows, err := d.queue.QueryCancelPending(ctx, d.cfg.MaxCancelBatch)
	if err != nil {
		d.log.Warn("cancel scan failed", "err", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var jobID, ownerID uuid.UUID
		var reason string
		if err := rows.Scan(&jobID, &ownerID, &reason); err != nil {
			d.log.Warn("cancel scan: row scan failed", "err", err)
			continue
		}

		// Unclaimed: terminalize in place.
		//
		// A job that was cancelled while it was still in the queue has no runner
		// to talk to. The claim query skips cancelled jobs, so the dispatcher is
		// the only authority that can move it to a terminal state. This is a
		// guarded UPDATE: if the runner that claimed it (the claim race lost the
		// cancel race) has finished between the scan and this write, the
		// terminal-cancel guard (status = 'queued') misses, RowsAffected is 0,
		// and nothing bad happens — no double-advance, no incorrect state.
		if ownerID == uuid.Nil {
			if _, err := d.queue.TerminalizeCancel(ctx, jobID); err != nil {
				d.log.Warn("terminal cancel of unclaimed job failed", "job", jobID, "err", err)
			}
			continue
		}

		sess, ok := d.sessions.Get(ownerID)
		if !ok {
			// The owner runner has disconnected but the job's lease has not yet
			// expired. The sweeper will reclaim it when the lease expires; the
			// cancel flag will ride along to the next claim, at which point
			// the cancel pass for the new owner will fire again. Do not clear
			// the flag: the job is not done yet, and clearing it would make
			// the next owner unaware that it should be cancelled.
			continue
		}

		// Best-effort push to the live session. If the buffer is full the push
		// is dropped, but the same flag still exists on the job, so the next
		// cancel pass (or the poll tick) will retry.
		if !sess.Send(&runnerv1.ServerMessage{
			Msg: &runnerv1.ServerMessage_CancelJob{
				CancelJob: &runnerv1.CancelJob{
					JobId:        jobID.String(),
					Reason:       reason,
					GraceSeconds: d.cfg.CancelGraceSeconds,
				},
			},
		}) {
			d.log.Warn("cancel push dropped (buffer full)", "job", jobID, "runner", ownerID)
		} else {
			d.log.Debug("cancel pushed to runner", "job", jobID, "runner", ownerID)
		}
	}
}

// dispatchOnce claims work for every connected runner with spare capacity.
//
// Each server instance dispatches only to the runners connected to it. Since
// the claim is atomic, two instances never hand out the same job, so the fleet
// scales horizontally without any coordination between instances.
//
// Backpressure (CIC-71g): when Config.Backpressure is non-nil, each claimed job
// is passed through Tracker.Acquire before it is assigned to a runner.  A rejected
// job is requeued with UndeliveredBackoff.  The cap key is job.RunID as a proxy
// for repoID; a future ticket (CIC-42) that adds RepoID to ClaimedJob can switch
// to the proper per-repo key.
func (d *Dispatcher) dispatchOnce(ctx context.Context) {
	// One pass at a time. Overlapping passes would not be incorrect — the claim
	// protects that — but they would spend database round trips racing to hand
	// work to the same runners.
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, sess := range d.sessions.Snapshot() {
		available := sess.Available()
		if available <= 0 {
			continue
		}
		if available > d.cfg.MaxBatch {
			available = d.cfg.MaxBatch
		}

		claimed, err := d.queue.Claim(ctx, queue.ClaimRequest{
			RunnerID:  sess.RunnerID,
			Labels:    sess.Labels,
			Executors: sess.Executors,
			Limit:     available,
			LeaseTTL:  d.cfg.LeaseTTL,
		})
		if err != nil {
			d.log.Error("claim jobs", "runner_id", sess.RunnerID, "error", err)
			continue
		}

		for _, job := range claimed {
			// Backpressure check: if the system is at capacity, requeue
			// this job and continue to the next claimed item.  The cap
			// key is job.RunID used as a proxy for repoID.
			if d.cfg.Backpressure != nil {
				acquired, err := d.cfg.Backpressure.Acquire(ctx, job.RunID.String(), "")
				if err != nil {
					// Fail-open: an acquire error must not stall a job.
					d.log.Warn("backpressure acquire failed",
						"job", job.JobID, "err", err)
				} else if !acquired {
					// Capacity reached: requeue and skip this job.
					sess.Release(job.JobID)
					if _, err := d.queue.Requeue(
						ctx, job.JobID,
						"backpressure limit reached", d.cfg.UndeliveredBackoff); err != nil {
						d.log.Error("backpressure requeue failed",
							"job", job.JobID, "err", err)
					}
					continue
				}
			}

			sess.TrackAssigned(job.JobID)
			if !sess.Send(runnersvc.AssignmentFor(job)) {
				// The runner went away, or its outbound buffer is full, between
				// the claim and the send. Return the job immediately rather
				// than making it wait out the lease TTL for the sweeper.
				sess.Release(job.JobID)
				// The backpressure slot was consumed by Acquire; free it
				// on the undelivered path so the counter does not leak.
				if d.cfg.Backpressure != nil {
					d.cfg.Backpressure.Release(job.RunID.String(), "")
				}
				if _, err := d.queue.Requeue(
					ctx, job.JobID,
					"runner unreachable at assignment",
					d.cfg.UndeliveredBackoff); err != nil {
					d.log.Error("requeue undelivered job",
						"job_id", job.JobID, "error", err)
				}
				continue
			}
			d.log.Info("job assigned",
				"job_id", job.JobID, "runner_id", sess.RunnerID, "attempt", job.Attempt)
		}
	}
}

func (d *Dispatcher) sweepOnce(ctx context.Context) {
	swept, err := d.queue.SweepExpiredLeases(ctx, time.Now())
	if err != nil {
		d.log.Error("sweep expired leases", "error", err)
		return
	}
	for _, s := range swept {
		d.log.Warn("reclaimed an expired lease",
			"job_id", s.JobID, "runner_id", s.RunnerID,
			"attempts", s.Attempts, "outcome", s.Outcome)
	}
	if len(swept) > 0 {
		// Requeued jobs are claimable now; do not wait for the next tick.
		d.Wake()
	}
}
