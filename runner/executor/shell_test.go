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

// A step that backgrounds a child, redirects its own I/O away (the normal
// way to write a "fire and forget" background job in a script), and exits
// cleanly (exit 0, no timeout, no cancellation) must not leave that child
// running on the host: it would survive into every job that runs after it.
// cmd.Cancel alone does not cover this — it only fires on timeout/
// cancellation — so this is a different path than
// TestShellCancellationKillsProcessGroup.
//
// The grandchild must redirect its own stdout/stderr for this to exercise
// the clean-exit reap specifically: one that keeps them open would instead
// block RunStep itself in the pump goroutines below, which is a separate,
// documented limitation (see the comment on the kill in RunStep).
func TestShellCleanExitKillsProcessGroup(t *testing.T) {
	sh := newPreparedShell(t)

	marker := sh.Workspace() + "/still-alive"
	res, err := sh.RunStep(context.Background(), executor.Step{
		// Backgrounds a grandchild that touches a file after the step exits,
		// then exits 0 immediately without waiting for it.
		Command: []string{"sh", "-c", "(sleep 2; touch " + marker + ") >/dev/null 2>&1 & exit 0"},
	}, &collectingSink{})
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}

	// Wait past when the backgrounded grandchild would have fired.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a backgrounded grandchild survived a clean exit; the process group was not reaped")
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

// TestShellExecsShellScriptAsOneProgram is CIC-147: the controller
// now wraps run: <script> as /bin/sh -ec <script> so the shell
// executor receives 3 args instead of 1.  This test confirms that
// the resulting command executes as a single shell program — both
// "hi there" and "good" lines must appear in the captured output —
// and does not try to find the multi-word string in the binary path.
func TestShellExecsShellScriptAsOneProgram(t *testing.T) {
	sh := executor.NewShell()
	sink := &collectingSink{}
	res, err := sh.RunStep(context.Background(), executor.Step{
		Command: []string{"/bin/sh", "-ec", "echo \"hi there\" && echo good"},
	}, sink)
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	out := sink.out()
	for _, want := range []string{"hi there", "good"} {
		if !strings.Contains(out, want) {
			t.Errorf("captured output %q missing %q", out, want)
		}
	}
}
