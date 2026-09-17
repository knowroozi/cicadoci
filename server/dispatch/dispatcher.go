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
}

// DefaultConfig returns sensible defaults.
func DefaultConfig(leaseTTL time.Duration) Config {
	return Config{
		PollInterval:       5 * time.Second,
		SweepInterval:      15 * time.Second,
		LeaseTTL:           leaseTTL,
		MaxBatch:           8,
		UndeliveredBackoff: 2 * time.Second,
	}
}

// Dispatcher assigns work to connected runners.
type Dispatcher struct {
	queue    *queue.Postgres
	sessions *runnersvc.SessionRegistry
	cfg      Config
	log      *slog.Logger

	wake chan struct{}
	mu   sync.Mutex
}

// New builds a dispatcher.
func New(q *queue.Postgres, sessions *runnersvc.SessionRegistry, cfg Config, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		queue:    q,
		sessions: sessions,
		cfg:      cfg,
		log:      log,
		// Buffered depth 1: the wake signal is level-triggered, not a queue.
		// Several notifications arriving while a pass is running should cause
		// exactly one more pass, not one per notification.
		wake: make(chan struct{}, 1),
	}
}

// Wake asks for a dispatch pass. It never blocks.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run drives the dispatch loop until the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	poll := time.NewTicker(d.cfg.PollInterval)
	defer poll.Stop()
	sweep := time.NewTicker(d.cfg.SweepInterval)
	defer sweep.Stop()

	d.dispatchOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.wake:
			d.dispatchOnce(ctx)
		case <-poll.C:
			d.dispatchOnce(ctx)
		case <-sweep.C:
			d.sweepOnce(ctx)
		}
	}
}

// dispatchOnce claims work for every connected runner with spare capacity.
//
// Each server instance dispatches only to the runners connected to it. Since
// the claim is atomic, two instances never hand out the same job, so the fleet
// scales horizontally without any coordination between instances.
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
			sess.TrackAssigned(job.JobID)
			if !sess.Send(runnersvc.AssignmentFor(job)) {
				// The runner went away, or its outbound buffer is full, between
				// the claim and the send. Return the job immediately rather
				// than making it wait out the lease TTL for the sweeper.
				sess.Release(job.JobID)
				if _, err := d.queue.Requeue(ctx, job.JobID, "runner unreachable at assignment", d.cfg.UndeliveredBackoff); err != nil {
					d.log.Error("requeue undelivered job", "job_id", job.JobID, "error", err)
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
