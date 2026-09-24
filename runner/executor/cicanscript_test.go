package executor_test

import (
	"context"
	"testing"
	"time"

	"cicadoci.com/cicado/runner/executor"
)

// discardSink is a no-op OutputSink that satisfies the executor's non-nil
// requirement without capturing output.
type discardSink struct{}

func (discardSink) Write(_ executor.Stream, _ []byte) {}

// TestCicanscriptExecutorDelegates verifies that NewExecutor("cicanscript")
// builds a fully-functional Executor: it runs a shell step successfully and
// cleans up its workspace.
//
// A cicanscript job is "a shell job, but sequenced." The executor wraps a
// Shell instance so every per-step feature (process-group kill, timeout,
// output capture, workspace isolation) is inherited for free.
func TestCicanscriptExecutorDelegates(t *testing.T) {
	exec, err := executor.NewExecutor("cicanscript")
	if err != nil {
		t.Fatalf("NewExecutor(cicanscript): %v", err)
	}

	job := executor.Job{
		ID:            "job-1",
		WorkspaceRoot: t.TempDir(),
	}

	if err = exec.Prepare(context.Background(), job); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	result, err := exec.RunStep(context.Background(), executor.Step{
		Index:   0,
		Name:    "hello",
		Command: []string{"sh", "-c", "echo cicanscript works"},
	}, discardSink{})
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode)
	}

	if err = exec.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

// TestCicanscriptExecutorCancellation verifies that when the outer context
// is cancelled, the cicanscript executor's RunStep returns (it does not
// block forever). This is the executor-level half of the CIC-16 cancel
// contract: the runner's job-executor loop detects ctx.Err() != nil after
// RunStep returns and reports terminal CANCELLED rather than FAILED.
func TestCicanscriptExecutorCancellation(t *testing.T) {
	exec, err := executor.NewExecutor("cicanscript")
	if err != nil {
		t.Fatalf("NewExecutor(cicanscript): %v", err)
	}

	job := executor.Job{
		ID:            "job-1",
		WorkspaceRoot: t.TempDir(),
	}
	if err = exec.Prepare(context.Background(), job); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer func() {
		_ = exec.Cleanup(context.Background())
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// A long-running shell step: cancelled mid-execution.
		_, _ = exec.RunStep(ctx, executor.Step{
			Command: []string{"sh", "-c", "sleep 30"},
		}, discardSink{})
		close(done)
	}()

	// Start the step, then cancel after a short delay.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good: the executor returned after cancel.
	case <-time.After(10 * time.Second):
		t.Fatal("executor did not return within 10 s of context cancellation")
	}
}

// TestCicanscriptExecutorBuilds verifies that "cicanscript" is a selectable
// executor name via NewExecutor, not just a string constant in job.go.
func TestCicanscriptExecutorBuilds(t *testing.T) {
	exec, err := executor.NewExecutor("cicanscript")
	if err != nil {
		t.Fatalf("NewExecutor(cicanscript): %v", err)
	}
	if exec == nil {
		t.Fatal("NewExecutor(cicanscript) returned nil")
	}
}
