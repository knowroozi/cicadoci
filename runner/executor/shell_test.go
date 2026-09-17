package executor_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cicadoci.com/cicado/runner/executor"
)

func newPreparedShell(t *testing.T) *executor.Shell {
	t.Helper()
	sh := executor.NewShell()
	if err := sh.Prepare(context.Background(), executor.Job{
		ID:            "job-1",
		WorkspaceRoot: t.TempDir(),
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() { _ = sh.Cleanup(context.Background()) })
	return sh
}

// collectingSink gathers output, safely across the two pump goroutines.
type collectingSink struct {
	mu     sync.Mutex
	stdout strings.Builder
	stderr strings.Builder
}

func (c *collectingSink) Write(stream executor.Stream, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stream == executor.StreamStderr {
		c.stderr.Write(data)
		return
	}
	c.stdout.Write(data)
}

func (c *collectingSink) out() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdout.String()
}

func (c *collectingSink) err() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderr.String()
}

func TestShellRunsAndCapturesBothStreams(t *testing.T) {
	sh := newPreparedShell(t)
	sink := &collectingSink{}

	res, err := sh.RunStep(context.Background(), executor.Step{
		Index:   0,
		Name:    "hello",
		Command: []string{"sh", "-c", "echo hello from cicado; echo oops >&2"},
	}, sink)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if got := sink.out(); !strings.Contains(got, "hello from cicado") {
		t.Errorf("stdout = %q, want it to contain the echoed text", got)
	}
	if got := sink.err(); !strings.Contains(got, "oops") {
		t.Errorf("stderr = %q, want it to contain the error text", got)
	}
}

// A step that exits non-zero is a job failure, not an executor failure. The
// distinction drives failure classification, so it is worth pinning down.
func TestShellNonZeroExitIsNotAnError(t *testing.T) {
	sh := newPreparedShell(t)

	res, err := sh.RunStep(context.Background(), executor.Step{
		Command: []string{"sh", "-c", "exit 42"},
	}, &collectingSink{})
	if err != nil {
		t.Fatalf("a failing step returned an executor error: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", res.ExitCode)
	}
}

func TestShellTimeoutKillsStep(t *testing.T) {
	sh := newPreparedShell(t)

	start := time.Now()
	res, err := sh.RunStep(context.Background(), executor.Step{
		Command: []string{"sleep", "60"},
		Timeout: 500 * time.Millisecond,
	}, &collectingSink{})
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if !res.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if res.ExitCode == 0 {
		t.Error("a timed-out step reported exit code 0")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("timeout took %v; the step was not killed promptly", elapsed)
	}
}

// Cancelling the context must kill the step's whole process group, not just the
// direct child. A step that backgrounds a process and exits would otherwise
// leave it running on the host indefinitely.
func TestShellCancellationKillsProcessGroup(t *testing.T) {
	sh := newPreparedShell(t)
	ctx, cancel := context.WithCancel(context.Background())

	marker := sh.Workspace() + "/still-alive"
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sh.RunStep(ctx, executor.Step{
			// Background a child that would touch a file after we cancel.
			Command: []string{"sh", "-c", "(sleep 3; touch " + marker + ") & sleep 30"},
		}, &collectingSink{})
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("step did not exit within 20s of cancellation")
	}

	// Wait past when the backgrounded child would have fired.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a backgrounded grandchild survived cancellation; the process group was not killed")
	}
}

// Steps must not inherit the runner's environment: it holds the runner's server
// credential, and a job that can read it can impersonate the runner.
func TestShellDoesNotLeakRunnerEnvironment(t *testing.T) {
	t.Setenv("CICADO_RUNNER_CREDENTIAL", "cicado_rnr_super_secret_value")

	sh := newPreparedShell(t)
	sink := &collectingSink{}

	if _, err := sh.RunStep(context.Background(), executor.Step{
		Command: []string{"sh", "-c", "env"},
		Env:     map[string]string{"STEP_VAR": "visible"},
	}, sink); err != nil {
		t.Fatalf("run step: %v", err)
	}

	out := sink.out()
	if strings.Contains(out, "super_secret_value") {
		t.Error("the runner's credential was visible in the step environment")
	}
	if strings.Contains(out, "CICADO_RUNNER_CREDENTIAL") {
		t.Error("a CICADO_ runner variable leaked into the step environment")
	}
	if !strings.Contains(out, "STEP_VAR=visible") {
		t.Errorf("the step's own env var was not passed through; got %q", out)
	}
}

func TestShellCleanupRemovesWorkspace(t *testing.T) {
	sh := executor.NewShell()
	if err := sh.Prepare(context.Background(), executor.Job{WorkspaceRoot: t.TempDir()}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ws := sh.Workspace()
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("workspace not created: %v", err)
	}
	if err := sh.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Error("workspace still exists after cleanup")
	}
}
