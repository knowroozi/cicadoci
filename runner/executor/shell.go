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
//
// A *Shell is scoped to exactly one job — runner/job.go constructs a fresh one
// per job — and workspace/mu assume that. Sharing one across concurrent jobs
// (e.g. a future optimization that reuses executors across a runner's
// capacity) would let them clobber each other's workspace.
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
	// cmd.Cancel above only fires on timeout/cancellation, not on a clean
	// exit. Setpgid puts the step in its own process group but does not reap
	// it: a step that backgrounds a process (redirecting its own I/O away,
	// which is how a background job is normally written) and exits would
	// otherwise leave that process running on the host for every job that
	// runs after it — the poisoned-host failure mode this executor's own doc
	// comment warns about. Killing the group on every return path closes
	// that; it is a no-op once the group's leader and everything under it
	// are already gone.
	//
	// This does NOT reap a grandchild that keeps its own copy of stdout or
	// stderr (i.e. it did not redirect them): the pump goroutines below block
	// on read until every holder of the pipe's write end closes it, so
	// RunStep itself will not return — and this kill will not run — until
	// that grandchild exits on its own or the step's own timeout/cancel
	// fires. Reaping was deliberately not made to race ahead of that by
	// calling cmd.Wait() before the pumps drain: doing so once let cmd.Wait's
	// own pipe close race the pump reads and silently dropped a step's
	// entire captured output under -race (caught by
	// TestShellRunsAndCapturesBothStreams flaking under repeated runs) —
	// worse than the leak this is fixing.
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()

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
			// Killed by a signal, not a normal exit: report the shell
			// convention of 128+signal (137 for SIGKILL, but not every kill
			// here is a SIGKILL — a step can catch and re-raise its own).
			code = 128
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code += int32(ws.Signal())
			} else {
				code += int32(syscall.SIGKILL)
			}
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
//
// The 32KB size is a ceiling, not a threshold to wait for: bufio.Reader.Read
// does at most one Read on the underlying pipe to refill its buffer, and a
// pipe read returns as soon as any data is available — it does not block
// until the buffer is full. A step that prints one byte and goes quiet is
// pumped in well under a millisecond, not held until 32KB accumulates.
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
