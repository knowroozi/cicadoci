// Package queue is the job queue. It runs on Postgres, which is the only
// stateful dependency CICaDo has (ADR-002).
//
// The design in one paragraph: jobs live in the `jobs` table and the queue is a
// view over it, so enqueueing happens in the same transaction that creates the
// run — there is no dual write to get out of sync. Workers claim with
// FOR UPDATE SKIP LOCKED, which lets N dispatchers claim disjoint batches
// without blocking each other. A LISTEN/NOTIFY doorbell wakes idle dispatchers
// instead of making them poll. A sweep over expired leases recovers jobs whose
// runner died without reporting.
//
// The interface in this file is the seam that keeps a future NATS or SQS driver
// from being a rewrite. The migration threshold is documented in
// project-docs/02-architecture.md §3.
package queue

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Queue is the scheduling seam. Postgres is the only implementation in MVP.
type Queue interface {
	// Enqueue makes a pending job eligible for claiming.
	Enqueue(ctx context.Context, jobID uuid.UUID) error

	// EnqueueInTx enqueues a job within an already-open transaction rather than via
	// its own statement on the pool: creating a run and its jobs as one unit of work,
	// CIC-12's scheduler-driven path needs that flip to 'queued' inside the same
	// commit so it can never be stranded half-made.
	EnqueueInTx(ctx context.Context, tx pgx.Tx, jobID uuid.UUID) error

	// Claim atomically leases up to limit jobs to a runner. Jobs are returned
	// only if the runner's labels satisfy their required_tags. Returns an empty
	// slice when nothing is eligible; that is not an error.
	Claim(ctx context.Context, req ClaimRequest) ([]ClaimedJob, error)

	// RenewLease extends the leases held by a runner for the given jobs, which
	// is what a Heartbeat does. It returns the jobs whose lease was NOT renewed
	// because it is no longer valid — the runner must abandon those.
	RenewLease(ctx context.Context, runnerID uuid.UUID, jobIDs []uuid.UUID, ttl time.Duration) ([]uuid.UUID, error)

	// Complete records a terminal state against a live lease. A stale lease is
	// rejected rather than applied: that is the zombie guard.
	Complete(ctx context.Context, res Completion) error

	// Requeue returns a job to the queue with backoff, or dead-letters it if it
	// has exhausted max_attempts.
	Requeue(ctx context.Context, jobID uuid.UUID, reason string, backoff time.Duration) (RequeueOutcome, error)

	// SweepExpiredLeases requeues jobs whose lease expired without a terminal
	// status — the recovery path for a runner that died mid-job.
	SweepExpiredLeases(ctx context.Context, now time.Time) ([]SweptJob, error)

	// RequestCancel marks a job cancelled and records who asked and why. It is the
	// write side of a user-initiated cancellation: the dispatcher reads the flag
	// the row now carries and pushes a CancelJob down the runner's session, the
	// runner stops in-flight execution within its grace window and reports a
	// terminal CANCELLED. A job that had already reached a terminal state is
	// untouched and reported as not active; a second cancel of a still-in-flight
	// job is harmless. The claim query skips cancelled jobs so one cancelled while
	// unclaimed is never handed out.
	RequestCancel(ctx context.Context, jobID uuid.UUID, reason string) (CancelResult, error)
}

// ClaimRequest is one dispatcher's attempt to lease work for a runner.
type ClaimRequest struct {
	RunnerID uuid.UUID
	// Labels the runner carries. A job is eligible when its required_tags are a
	// subset of these.
	Labels []string
	// Executors the runner can actually run. A job requesting an executor the
	// runner lacks is not eligible, however well its tags match.
	Executors []string
	Limit     int
	// LeaseTTL should be at least 3x the heartbeat interval, so a single missed
	// heartbeat does not cost a runner its jobs.
	LeaseTTL time.Duration
}

// ClaimedJob is a job leased to a runner, with everything needed to build a
// JobAssignment. Note what is absent: secret values. The job carries secret
// NAMES; values are fetched separately against the live lease.
type ClaimedJob struct {
	JobID          uuid.UUID
	RunID          uuid.UUID
	LeaseID        uuid.UUID
	LeaseExpiresAt time.Time
	Name           string
	WorkflowName   string
	Executor       string
	Env            map[string]string
	SecretRefs     []string
	TimeoutSeconds int32
	Attempt        int32
	GitCloneURL    string
	GitRef         string
	GitSHA         string
	Steps          []Step
}

// Step is one step of a claimed job, resolved and ready to execute.
type Step struct {
	Index          int32
	Name           string
	Image          string
	Command        []string
	Env            map[string]string
	TimeoutSeconds int32
	NetworkEnabled bool
}

// TerminalState is the set of states a job can finish in.
type TerminalState string

const (
	StateSucceeded TerminalState = "succeeded"
	StateFailed    TerminalState = "failed"
	StateCancelled TerminalState = "cancelled"
)

// Completion is a terminal report from a runner, authorized by LeaseID.
type Completion struct {
	JobID       uuid.UUID
	LeaseID     uuid.UUID
	State       TerminalState
	ExitCode    int32
	ErrorClass  string
	ErrorDetail string
}

// RequeueOutcome says what happened to a requeued job.
type RequeueOutcome string

const (
	// OutcomeRequeued means the job is back in the queue for another attempt.
	OutcomeRequeued RequeueOutcome = "requeued"
	// OutcomeDeadLettered means it exhausted max_attempts and will not run again.
	OutcomeDeadLettered RequeueOutcome = "dead_lettered"
)

// SweptJob records one lease the sweeper reclaimed.
type SweptJob struct {
	JobID    uuid.UUID
	RunnerID uuid.UUID
	Outcome  RequeueOutcome
	Attempts int32
}

// CancelResult reports what RequestCancel did to a job so the caller (the
// dispatcher or an API route) knows which runner, if any, to notify.
type CancelResult struct {
	// Active tells whether the job was still in-flight when the request arrived,
	// i.e. whether anything downstream (a runner, or the queue itself for a job
	// not yet claimed) still has work to do. A terminal job is not active.
	Active bool
	// RunnerID is the runner that currently holds the job's lease, so a
	// CancelJob can be pushed to the one session that owns it. It is the zero
	// UUID when the job was still queued with no claimer (Active may still be
	// true), meaning there is nothing to push yet: the flag alone is enough and
	// the claim query skips cancelled jobs rather than handing them out. It is
	// also the zero UUID when the job was terminal (Active is false then).
	RunnerID uuid.UUID
}
