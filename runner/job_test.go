package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

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

// B3c: runner-level executeJob tests for CIC-148 B2 optional-token behaviour.
//
// When a job's step env references ${{ secrets.github_clone_token }} but
// the secrets store returns nothing (absent or whitespace-only), the
// runner must NOT fail the step. resolveOptionalSecrets blanks
// CICADO_GITHUB_TOKEN to "", so the clone script's [ -n "$CICADO_GITHUB_TOKEN" ]
// guard takes the else branch (anonymous fetch).
//
// TestGithubCloneTokenNoSecretJobSucceeds:
//   - Step env: CICADO_GITHUB_TOKEN = ${{ secrets.github_clone_token }}
//   - secrets = {} (no client configured; newTestAgent uses nil)
//   - Step command: sh -c 'test -z "$CICADO_GITHUB_TOKEN" && echo ok || echo FAIL'
//   - Job MUST succeed.
//   - Capture the output to verify the assertion ran.
//
// TestGithubCloneTokenWhitespaceJobSucceeds:
//   - Step env: CICADO_GITHUB_TOKEN = ${{ secrets.github_clone_token }}
//   - secrets fake returns github_clone_token = "    " (whitespace)
//   - Same step command; job MUST succeed.
//   - Captures output.
//
// These tests exercise the full path: executeJob -> fetchJobSecrets ->
// resolveOptionalSecrets -> stepEnv -> runner exec.  M1 (delete
// runner/job.go:203 resolveOptionalSecrets call): the test would
// fail because CICADO_GITHUB_TOKEN would be the literal template text
// (non-empty), so `test -z` would FAIL.
func TestGithubCloneTokenNoSecretJobSucceeds(t *testing.T) {
	ag := newTestAgent(t)
	// newTestAgent uses nil secrets; fetchJobSecrets returns empty map.
	// github_clone_token is absent.

	outDir := t.TempDir()
	outfile := filepath.Join(outDir, "tokcheck.txt")

	assignment := &runnerv1.JobAssignment{
		JobId:   "job-ghct-no-token",
		LeaseId: "lease-ghct-no-token",
		Spec: &runnerv1.JobSpec{
			Executor:   "shell",
			SecretRefs: []string{"github_clone_token"},
			Steps: []*runnerv1.StepSpec{
				{
					Index: 0,
					Name:  "check-ghct",
					Command: []string{"sh", "-c",
						"test -z \"$CICADO_GITHUB_TOKEN\" && echo ok > " + outfile + " || echo FAIL >> " + outfile},
					Env: map[string]string{
						"CICADO_GITHUB_TOKEN": "${{ secrets.github_clone_token }}",
					},
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
		t.Fatalf("expected SUCCEEDED, got %v — resolveOptionalSecrets B2 not applied", got)
	}
	data, err := os.ReadFile(outfile)
	if err != nil {
		t.Fatalf("could not read output %s: %v", outfile, err)
	}
	if string(data) != "ok\n" {
		t.Errorf("expected 'ok\\n', got %q", string(data))
	}
}

// fakeSecretsWhitespace returns a github_clone_token value that is
// whitespace-only.  This exercises the B2 normalisation.
type fakeSecretsWhitespace struct{}

func (f *fakeSecretsWhitespace) GetJobSecrets(_ context.Context, _ *runnerv1.GetJobSecretsRequest, _ ...grpc.CallOption) (*runnerv1.GetJobSecretsResponse, error) {
	return &runnerv1.GetJobSecretsResponse{Secrets: map[string]string{"github_clone_token": "   \t\n "}}, nil
}

func TestGithubCloneTokenWhitespaceJobSucceeds(t *testing.T) {
	ag := newTestAgent(t)
	ag.secrets = &fakeSecretsWhitespace{}

	outDir := t.TempDir()
	outfile := filepath.Join(outDir, "wscheck.txt")

	assignment := &runnerv1.JobAssignment{
		JobId:   "job-ghct-ws",
		LeaseId: "lease-ghct-ws",
		Spec: &runnerv1.JobSpec{
			Executor:   "shell",
			SecretRefs: []string{"github_clone_token"},
			Steps: []*runnerv1.StepSpec{
				{
					Index: 0,
					Name:  "check-ws",
					Command: []string{"sh", "-c",
						"test -z \"$CICADO_GITHUB_TOKEN\" && echo ok > " + outfile + " || echo FAIL >> " + outfile},
					Env: map[string]string{
						"CICADO_GITHUB_TOKEN": "${{ secrets.github_clone_token }}",
					},
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
		t.Fatalf("expected SUCCEEDED for whitespace token, got %v", got)
	}
	data, err := os.ReadFile(outfile)
	if err != nil {
		t.Fatalf("could not read output %s: %v", outfile, err)
	}
	if string(data) != "ok\n" {
		t.Errorf("for whitespace-only token, expected 'ok\\n', got %q", string(data))
	}
}

// CIC-118: the runner enforces a step's `if:` conditional against the outcomes of
// the steps that ran before it. A "success"-gated step is skipped when a prior
// step failed, an "always"-gated step still runs, and the job reports a single
// terminal FAILED reflecting the prior failure.
//
// Structured events travel the LogStream (not the RunnerMessage sink this test
// drives), so enforcement is asserted behaviourally: the skipped step neither is
// marked RUNNING nor runs its command; the always-gated step does run.
func TestStepIfConditionsEnforcedAtRunner(t *testing.T) {
	ag := newTestAgent(t)
	dir := t.TempDir()
	skippedFile := filepath.Join(dir, "success-step-ran")
	alwaysFile := filepath.Join(dir, "always-step-ran")

	assignment := &runnerv1.JobAssignment{
		JobId:   "job-if",
		LeaseId: "lease-if",
		Spec: &runnerv1.JobSpec{
			Executor: "shell",
			Steps: []*runnerv1.StepSpec{
				// step 0 fails unconditionally.
				{Index: 0, Name: "failing", Command: []string{"/bin/sh", "-c", "exit 1"}},
				// step 1 runs only on success: it must be skipped after the failure.
				{
					Index:       1,
					Name:        "on-success",
					IfCondition: "success",
					Command:     []string{"/bin/sh", "-c", fmt.Sprintf("echo ran > %q", skippedFile)},
				},
				// step 2 runs regardless: it must run after the failure.
				{
					Index:       2,
					Name:        "on-always",
					IfCondition: "always",
					Command:     []string{"/bin/sh", "-c", fmt.Sprintf("echo ran > %q", alwaysFile)},
				},
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &ifCapture{}
	ag.executeJob(ctx, assignment, sink.Send)

	if got := sink.terminalState(); got != runnerv1.StatusUpdate_FAILED {
		t.Fatalf("terminal state = %v, want FAILED", got)
	}
	if n := sink.terminalCount(); n != 1 {
		t.Fatalf("got %d terminal updates, want exactly 1", n)
	}
	// The "success"-gated step must have been skipped: it neither runs nor writes.
	if _, err := os.Stat(skippedFile); err == nil {
		t.Error(`"success"-gated step ran despite a prior failure`)
	}
	if sink.ranStep(1) {
		t.Error(`"success"-gated step was marked RUNNING despite being skipped`)
	}
	// The "always"-gated step must have run.
	if _, err := os.Stat(alwaysFile); err != nil {
		t.Errorf(`"always"-gated step did not run after a failure: %v`, err)
	}
	if !sink.ranStep(2) {
		t.Error(`"always"-gated step was not marked RUNNING`)
	}
}

// ifCapture records the terminal status update (state + count and the step_index
// of every RUNNING_STEP update, so a conditional-execution test can assert which
// steps actually started.
type ifCapture struct {
	mu           sync.Mutex
	terminal     runnerv1.StatusUpdate_State
	terminalN    int
	runningSteps []uint32
}

func (c *ifCapture) Send(m *runnerv1.RunnerMessage) error {
	upd := m.GetStatusUpdate()
	if upd == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if isTerminalState(upd.GetState()) {
		c.terminal = upd.GetState()
		c.terminalN++
	} else if upd.GetState() == runnerv1.StatusUpdate_RUNNING_STEP {
		c.runningSteps = append(c.runningSteps, upd.GetStepIndex())
	}
	return nil
}

func (c *ifCapture) terminalState() runnerv1.StatusUpdate_State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminal
}

func (c *ifCapture) terminalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminalN
}

func (c *ifCapture) ranStep(idx uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.runningSteps {
		if s == idx {
			return true
		}
	}
	return false
}
