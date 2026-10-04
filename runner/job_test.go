package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
)

// captureTerminalStatus collects the terminal StatusUpdate that the job-executor
// loop sends when a run ends. This lets a test assert that context cancellation
// produces a CANCELLED terminal update rather than FAILED.
type captureTerminalStatus struct {
	mu       sync.Mutex
	terminal runnerv1.StatusUpdate_State
	seen     bool
}

func (c *captureTerminalStatus) Send(m *runnerv1.RunnerMessage) error {
	upd := m.GetStatusUpdate()
	if upd != nil && isTerminalState(upd.GetState()) {
		c.mu.Lock()
		c.terminal = upd.GetState()
		c.seen = true
		c.mu.Unlock()
	}
	return nil
}

func (c *captureTerminalStatus) terminalState() runnerv1.StatusUpdate_State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminal
}

func (c *captureTerminalStatus) sawTerminal() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen
}

// isTerminalState reports whether a status state is one the job-executor emits
// as a final StatusUpdate for a job.
func isTerminalState(s runnerv1.StatusUpdate_State) bool {
	switch s {
	case runnerv1.StatusUpdate_SUCCEEDED,
		runnerv1.StatusUpdate_FAILED,
		runnerv1.StatusUpdate_CANCELLED:
		return true
	}
	return false
}

// TestCancelPropagatesToRunningStep is the CIC-16 acceptance test for the
// runner side: when a running job's context is cancelled — as the dispatcher's
// CancelJob push does — the executor's RunStep returns, and the job-executor
// loop detects the cancellation and reports terminal CANCELLED, not FAILED.
//
// This is the contract that a server-side CANCELLED status requires: the runner
// must translate a context cancel into a CANCELLED terminal update.
func TestCancelPropagatesToRunningStep(t *testing.T) {
	ag := newTestAgent(t)

	// JobSpec with a long-running shell step that outlives the cancel.
	assignment := &runnerv1.JobAssignment{
		JobId:   "job-cancel",
		LeaseId: "lease-cancel",
		Spec: &runnerv1.JobSpec{
			Executor: "shell",
			Steps: []*runnerv1.StepSpec{
				{
					Index:   0,
					Name:    "long-running",
					Command: []string{"sh", "-c", "sleep 30"},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	sink := &captureTerminalStatus{}

	done := make(chan struct{})
	go func() {
		ag.executeJob(ctx, assignment, sink.Send)
		close(done)
	}()

	// Give the step a moment to start, then cancel the context.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good: executeJob returned after cancel.
	case <-time.After(10 * time.Second):
		t.Fatal("executeJob did not return within 10 s of cancel")
	}

	if !sink.sawTerminal() {
		t.Fatal("expected a terminal StatusUpdate, got none")
	}
	if got := sink.terminalState(); got != runnerv1.StatusUpdate_CANCELLED {
		t.Fatalf("terminal state = %v, want CANCELLED", got)
	}
}

// TestCancelDuringPrepareFails verifies that a job whose context is cancelled
// before any step starts cannot silently succeed. The job must report a
// terminal FAILED or CANCELLED, not SUCCEEDED.
func TestCancelDuringPrepareFails(t *testing.T) {
	ag := newTestAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately, before executeJob can start.

	assignment := &runnerv1.JobAssignment{
		JobId:   "job-cancel-prepare",
		LeaseId: "lease-cancel-prepare",
		Spec: &runnerv1.JobSpec{
			Executor: "shell",
			Steps: []*runnerv1.StepSpec{
				{
					Index:   0,
					Name:    "never-runs",
					Command: []string{"true"},
				},
			},
		},
	}

	sink := &captureTerminalStatus{}
	ag.executeJob(ctx, assignment, sink.Send)

	if got := sink.terminalState(); got == runnerv1.StatusUpdate_SUCCEEDED {
		t.Fatalf("job with cancelled context should not SUCCEED, got %v", got)
	}
}

// TestGitEnvInjectedInShellStep is the CIC-148 acceptance test on the runner
// side: a JobAssignment carrying a Git context must make its coordinates
// available as CICADO_GIT_CLONE_URL / CICADO_GIT_REF / CICADO_GIT_SHA in
// every step's environment. A shell step writes all three to a known file;
// the test reads the file and asserts the expected values.
func TestGitEnvInjectedInShellStep(t *testing.T) {
	ag := newTestAgent(t)

	outDir := t.TempDir()
	outfile := filepath.Join(outDir, "gitenv.txt")

	// A shell step that writes the three git env vars to a known file.
	// Each var is written with printf; append mode for the 2nd/3rd avoids
	// clobbering the first write.
	cmd := fmt.Sprintf(
		"printf \"CICADO_GIT_CLONE_URL=%%s\\n\" \"$CICADO_GIT_CLONE_URL\" > %s && "+
			"printf \"CICADO_GIT_REF=%%s\\n\" \"$CICADO_GIT_REF\" >> %s && "+
			"printf \"CICADO_GIT_SHA=%%s\\n\" \"$CICADO_GIT_SHA\" >> %s",
		outfile, outfile, outfile,
	)

	assignment := &runnerv1.JobAssignment{
		JobId:   "job-git-env",
		LeaseId: "lease-git-env",
		Spec: &runnerv1.JobSpec{
			Executor: "shell",
			Git: &runnerv1.GitContext{
				CloneUrl: "https://github.com/acme/widgets.git",
				Ref:      "refs/heads/main",
				Sha:      "deadbeefcafef00d",
			},
			Steps: []*runnerv1.StepSpec{
				{
					Index:   0,
					Name:    "capture-git-env",
					Command: []string{"sh", "-c", cmd},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sink := &captureTerminalStatus{}
	ag.executeJob(ctx, assignment, sink.Send)

	if !sink.sawTerminal() {
		t.Fatal("no terminal StatusUpdate received")
	}
	if got := sink.terminalState(); got != runnerv1.StatusUpdate_SUCCEEDED {
		t.Fatalf("terminal state = %v, want SUCCEEDED", got)
	}

	data, err := os.ReadFile(outfile)
	if err != nil {
		t.Fatalf("failed to read git-env capture file %s: %v", outfile, err)
	}
	want := "CICADO_GIT_CLONE_URL=https://github.com/acme/widgets.git\n" +
		"CICADO_GIT_REF=refs/heads/main\n" +
		"CICADO_GIT_SHA=deadbeefcafef00d\n"
	if got := string(data); got != want {
		t.Errorf("git env capture:\n got: %q\nwant: %q", got, want)
	}
}
