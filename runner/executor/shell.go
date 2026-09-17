package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Shell runs steps directly on the runner host.
//
// This executor is for trusted infrastructure jobs only, and that is not a
// disclaimer to soften. Steps share the host: its filesystem outside the
// workspace, its installed tooling, its network, and whatever a previous job
// left behind. A job can poison the host for every job that follows. The docker
// executor is the default for that reason; this one exists because some
// infrastructure work genuinely has to touch the host it runs on.
type Shell struct {
	workspace string
	mu        sync.Mutex
}

// NewShell builds a shell executor.
func NewShell() *Shell { return &Shell{} }

// Name implements Executor.
func (s *Shell) Name() string { return "shell" }

// Prepare creates the job's workspace directory.
func (s *Shell) Prepare(_ context.Context, job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	root := job.WorkspaceRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("shell: create workspace root: %w", err)
	}
	dir, err := os.MkdirTemp(root, "cicado-job-")
	if err != nil {
		return fmt.Errorf("shell: create workspace: %w", err)
	}
	s.workspace = dir
	return nil
}

// Workspace returns the prepared workspace directory.
func (s *Shell) Workspace() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspace
}

// RunStep executes one step in the job workspace.
func (s *Shell) RunStep(ctx context.Context, step Step, sink OutputSink) (Result, error) {
	if len(step.Command) == 0 {
		return Result{}, errors.New("shell: step has no command")
	}

	stepCtx := ctx
	var cancel context.CancelFunc
	if step.Timeout > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}

	started := time.Now()

	//nolint:gosec // Running the command a workflow specifies is this package's purpose.
	cmd := exec.CommandContext(stepCtx, step.Command[0], step.Command[1:]...)
	cmd.Dir = s.Workspace()
	cmd.Env = buildEnv(step.Env)

	// Put the child in its own process group so that killing it on timeout or
	// cancellation takes its children too. A step that spawns a background
	// process and exits would otherwise leave it running on the host forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("shell: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("shell: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("shell: start %q: %w", step.Command[0], err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stdout, StreamStdout, sink) }()
	go func() { defer wg.Done(); pump(stderr, StreamStderr, sink) }()
	wg.Wait()

	waitErr := cmd.Wait()
	duration := time.Since(started)

	timedOut := step.Timeout > 0 && errors.Is(stepCtx.Err(), context.DeadlineExceeded)

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		return Result{ExitCode: 0, Duration: duration}, nil
	case errors.As(waitErr, &exitErr):
		// The step ran and failed. That is a job result, not an executor error.
		code := int32(exitErr.ExitCode())
		if code < 0 {
			// Killed by a signal; report a shell-conventional 128+signal.
			code = 137
		}
		return Result{ExitCode: code, TimedOut: timedOut, Duration: duration}, nil
	default:
		return Result{Duration: duration}, fmt.Errorf("shell: wait for %q: %w", step.Command[0], waitErr)
	}
}

// Cleanup removes the workspace.
func (s *Shell) Cleanup(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspace == "" {
		return nil
	}
	// Guard against removing something unexpected if workspace was mangled.
	if !filepath.IsAbs(s.workspace) {
		return fmt.Errorf("shell: refusing to remove non-absolute workspace %q", s.workspace)
	}
	if err := os.RemoveAll(s.workspace); err != nil {
		return fmt.Errorf("shell: remove workspace: %w", err)
	}
	s.workspace = ""
	return nil
}

// pump forwards output to the sink as it arrives.
//
// It reads whatever is available rather than splitting on lines: a step that
// prints a prompt without a newline, or a progress bar using carriage returns,
// should still show up in a live tail instead of sitting in a buffer.
func pump(r io.Reader, stream Stream, sink OutputSink) {
	reader := bufio.NewReaderSize(r, 32*1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sink.Write(stream, chunk)
		}
		if err != nil {
			return
		}
	}
}

// buildEnv composes the child environment.
//
// The runner's own environment is deliberately NOT inherited. The runner process
// may hold its server credential and other configuration in env vars, and a job
// that can read them can impersonate the runner. Steps get a minimal base plus
// exactly what the job specified.
func buildEnv(stepEnv map[string]string) []string {
	base := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME": os.TempDir(),
		"CI":   "true",
	}
	for k, v := range stepEnv {
		base[k] = v
	}
	env := make([]string, 0, len(base))
	for k, v := range base {
		env = append(env, k+"="+v)
	}
	return env
}
