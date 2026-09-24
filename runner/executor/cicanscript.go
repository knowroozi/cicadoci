package executor

import (
	"context"
)

// Cicanscript is a step runner that delegates to the Shell executor.
//
// A cicanscript job is a sequence of shell commands. The executor holds a
// Shell instance and, for each step in the job's sequence, invokes
// Shell.RunStep. This keeps the process-group kill, timeout, and output
// plumbing of the shell executor without duplicating any of it.
//
// The cicanscript executor is the natural next step after the raw Shell
// executor: a cicanscript is "a shell job, but sequenced" — it runs
// multiple commands in order, stopping on the first non-zero exit. The
// Shell executor already iterates over Steps; the cicanscript executor
// is simply a named alias that makes the intent explicit at the
// JobSpec.Executor level.
//
// The Shell executor is the single implementation in the executor
// package that supports context cancellation, timeout, process-group
// kill, and output streaming correctly. Delegating to it directly
// means every cicanscript feature is inherited for free.
type Cicanscript struct {
	shell *Shell
}

// NewCicanscript builds a cicanscript executor with a fresh underlying
// shell executor.
func NewCicanscript() *Cicanscript {
	return &Cicanscript{shell: NewShell()}
}

// Name returns "cicanscript" so the dispatcher can log which executor
// handled a job. This is the executor name that appears in
// JobSpec.Executor, not the type name.
func (c *Cicanscript) Name() string { return "cicanscript" }

// Prepare delegates to the shell executor to create the per-job workspace.
func (c *Cicanscript) Prepare(ctx context.Context, job Job) error {
	return c.shell.Prepare(ctx, job)
}

// RunStep delegates to the shell executor to run a single shell command
// with per-step environment, timeout, and process-group handling.
func (c *Cicanscript) RunStep(ctx context.Context, step Step, sink OutputSink) (Result, error) {
	return c.shell.RunStep(ctx, step, sink)
}

// Cleanup delegates to the shell executor to remove the workspace.
func (c *Cicanscript) Cleanup(ctx context.Context) error {
	return c.shell.Cleanup(ctx)
}
