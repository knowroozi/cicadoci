// Package executor runs job steps.
//
// The Executor interface is the portability seam the architecture calls out as
// one of the two decisions that are near-impossible to change later (the runner
// protocol being the other). Kubernetes, Firecracker, and gVisor drivers arrive
// post-MVP as implementations of this interface, so it needs to be expressive
// enough for them without leaking anything specific to the two drivers that
// exist now.
package executor

import (
	"context"
	"time"
)

// Executor prepares a workspace, runs steps in it, and cleans up.
//
// The lifecycle is Prepare, then RunStep per step, then Cleanup — with Cleanup
// guaranteed to run even when a step fails or the job is cancelled.
type Executor interface {
	// Name is the identifier used in a workflow's `executor` field.
	Name() string

	// Prepare sets up whatever a job needs before its first step: a workspace
	// directory, a volume, a pod. It must be safe to call once per job.
	Prepare(ctx context.Context, job Job) error

	// RunStep executes one step and returns its result. Output is delivered to
	// sink as it is produced, not buffered to the end: a job that hangs must
	// still show what it printed before it hung.
	//
	// A non-nil error means the step could not be run (infrastructure). A step
	// that ran and exited non-zero returns a Result with that exit code and a
	// nil error — that is a job failure, not an executor failure, and the
	// distinction drives failure classification.
	RunStep(ctx context.Context, step Step, sink OutputSink) (Result, error)

	// Cleanup releases everything Prepare acquired.
	Cleanup(ctx context.Context) error
}

// Job is the execution context for one job.
type Job struct {
	ID    string
	RunID string
	// WorkspaceRoot is where the executor should place this job's workspace.
	WorkspaceRoot string
	Env           map[string]string
	// Secrets are held in memory for the life of the job and never written to
	// disk. They are registered with the redaction masker before any step runs.
	Secrets map[string]string
}

// Step is one step to execute.
type Step struct {
	Index          int32
	Name           string
	Image          string
	Command        []string
	Env            map[string]string
	Timeout        time.Duration
	NetworkEnabled bool
}

// Stream identifies which output stream a line came from.
type Stream int

const (
	// StreamStdout is standard output.
	StreamStdout Stream = iota
	// StreamStderr is standard error.
	StreamStderr
)

// OutputSink receives step output as it is produced.
//
// Implementations must be safe for concurrent use: stdout and stderr are
// drained by separate goroutines.
type OutputSink interface {
	Write(stream Stream, data []byte)
}

// OutputSinkFunc adapts a function to OutputSink.
type OutputSinkFunc func(stream Stream, data []byte)

// Write implements OutputSink.
func (f OutputSinkFunc) Write(stream Stream, data []byte) { f(stream, data) }

// Result is the outcome of a step that actually ran.
type Result struct {
	ExitCode int32
	// TimedOut records that the step was killed for exceeding its timeout,
	// which is a different failure class from a non-zero exit.
	TimedOut bool
	Duration time.Duration
}
