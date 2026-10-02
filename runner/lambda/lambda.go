// Package lambda provides an AWS Lambda adapter for CICaDo runners.
//
// This is a thin adapter that wraps the existing runner so it
// can be deployed to AWS Lambda or Step Functions. The key
// constraint: Lambda functions are ephemeral, so the runner
// must be idempotent, stateless, and bounded in execution
// time.
//
// MVP scope: a Lambda handler that:
// 1. Receives a job dispatch event (from Step Functions or SQS).
// 2. Connects to the gRPC queue, polls for a job.
// 3. Executes the job (with step-level logging to CloudWatch).
// 4. Returns completion status.
//
// The actual Step Functions wiring is in the Terraform module.
package lambda

import (
	"context"
	"log/slog"

	"cicadoci.com/cicado/runner/executor"
	"cicadoci.com/cicado/runner/journal"
)

// Handler is the Lambda entry point.
//
// In production this would use the AWS Lambda Go SDK to
// deserialize a CloudEvents-formatted input, extract the
// job, dispatch it, and serialize a response.
//
// For the MVP, we implement the core logic that would
// sit inside the Lambda handler.
type Handler struct {
	exec *executor.Executor
	j    journal.Journal
	log  *slog.Logger
}

// New creates a Lambda handler.
func New(exec *executor.Executor, j journal.Journal, log *slog.Logger) *Handler {
	return &Handler{
		exec: exec,
		j:    j,
		log:  log,
	}
}

// JobEvent is the input to a Lambda dispatch (from Step Functions or SQS).
type JobEvent struct {
	// RunID is the run this job belongs to.
	RunID string

	// JobID is the job to dispatch.
	JobID string

	// Attempt is the attempt number (1-indexed).
	Attempt int

	// LeaseToken is the queue's lease for this job.
	LeaseToken string
}

// JobResponse is the output of a Lambda dispatch.
type JobResponse struct {
	// Success indicates whether the job succeeded.
	Success bool

	// ExitCode is the job's exit code.
	ExitCode int32

	// FailureClass is the classified failure category.
	FailureClass string

	// StepLogs contains per-step log output (CloudWatch).
	StepLogs map[string]string

	// Message is a human-readable reason when Success is false.
	Message string
}

// NotImplementedMessage is the failure reason the stub Dispatch reports.
const NotImplementedMessage = "lambda executor is not implemented: the job was not run (CIC-137)"

// Dispatch is the core Lambda invocation logic.
//
// It accepts a JobEvent extracted from the CloudEvents / SQS /
// Step Functions input, executes the job, and returns a
// JobResponse. The caller handles timeout and retry.
//
// For the MVP this is a logging + journal stub that fails closed: it
// never runs the job, so it reports failure rather than success. The
// actual gRPC queue connect + job fetch + execute is CIC-137.
func (h *Handler) Dispatch(ctx context.Context, evt *JobEvent) (*JobResponse, error) {
	h.log.Info("lambda: dispatching job",
		"run_id", evt.RunID,
		"job_id", evt.JobID,
		"attempt", evt.Attempt,
		"lease", len(evt.LeaseToken), // log token length not value
	)

	// Record in journal for audit trail.
	h.j.Record(journal.Entry{
		JobID: evt.JobID,
		Lane:  journal.LaneStatus,
		Seq:   0,
		Msg:   evt,
	})

	// TODO(CIC-137): the full Lambda dispatch path (connect to the gRPC
	// queue, fetch the job, execute the steps, return a result) is not yet
	// wired; it needs a public RPC surface on the queue. Until then the stub
	// fails closed: reporting success here would mark every job as passed
	// without running anything.
	_ = h.exec
	_ = ctx
	h.log.Warn("lambda: executor not implemented; failing job",
		"run_id", evt.RunID,
		"job_id", evt.JobID,
	)
	return &JobResponse{
		Success:      false,
		ExitCode:     1,
		FailureClass: "infra",
		Message:      NotImplementedMessage,
	}, nil
}
