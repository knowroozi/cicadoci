package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// dockerBin is the container-runtime command RunStep shells out to. It is a var,
// not a literal, so a test can point it at a fake binary without needing a real
// Docker daemon on the host — the same reason Shell runs a step's own command
// (step.Command[0]) rather than a hard-coded program name.
var dockerBin = "docker"

// workspaceMountPoint is the in-container path a job's workspace is mounted at.
// Every step starts its own fresh container, but each of them mounts the same
// host workspace directory at this same path, so a file one step writes is
// visible to the next. That is what makes "a fresh container per step" safe:
// no runtime state carries across steps — only the mounted storage does.
const workspaceMountPoint = "/cicado/workspace"

// containerSeq is a process-wide atomic counter for unique container names.
// CIC-195 B4: per-instance counters collide across concurrent jobs.
var containerSeq int64

// Docker runs every step in its own Linux container on the runner's docker
// daemon.
//
// It is the inverse of Shell in the way that matters: Shell shares the host with
// whatever ran before it and so can poison the host for what runs next, which is
// why Shell is for trusted infrastructure only. Docker gives each job an
// isolated container instead, while a bind-mounted workspace keeps one job's
// steps talking to each other across the container boundary.
//
// Like Shell, a *Docker is scoped to exactly one job and assumes runner/job.go
// hands it a fresh value per assignment. Reusing one across concurrent jobs
// would let them clobber each other's workspace, exactly as Shell warns about.
//
// Network is disabled by default: a step runs under `--network none` unless its
// NetworkEnabled flag opts back in. This default-off posture is a primary
// isolation guarantee for untrusted job code running on a shared runner.
//
// CIC-32 / CIC-195: on timeout or cancel, the client process group is killed
// and killContainer (with a 5 s background timeout, S3) removes the named
// container by force so a killed client does not leave a `--rm` container
// running under the daemon.
//
// CIC-196 B1/C1: step.Env is passed via --env-file (a 0600 file in an OS temp
// dir, S2), not via -e KEY=VALUE flags, which were briefly visible in the process
// listing of the docker client. isValidEnvKey (C1/C1a) rejects non-ASCII and
// metacharacter keys so a workflow cannot redirect a runner-host variable into
// the container. A documented, deliberate hardening, not an oversight.
type Docker struct {
	workspace string
	jobID     string
	mu        sync.Mutex
}

// NewDocker builds a docker executor.
func NewDocker() *Docker { return &Docker{} }

// killContainer force-removes a container by name. S3: it uses
// context.WithTimeout(contextWithoutCancel(ctx), 5s) so that a caller
// cancel during the 5-second cleanup window (e.g. a job timeout or
// server shutdown) does not abort the teardown. CIC-195 B3.
func (d *Docker) killContainer(ctx context.Context, name string) {
	// S3: contextWithoutCancel so a caller cancel during the 5 s window
	// cannot abort the container teardown.
	ctx2, cancel := context.WithTimeout(contextWithoutCancel(ctx), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx2, dockerBin, "rm", "-f", name)
	//nolint:errcheck // best-effort cleanup
	_ = cmd.Run()
}

// contextWithoutCancel wraps context.WithoutCancel for Go 1.20.+
// so a caller cancel during the cleanup window cannot abort
// the container teardown. S3.
func contextWithoutCancel(parent context.Context) context.Context {
	return context.WithoutCancel(parent)
}

// containerName returns a globally unique container name using the
// process-wide atomic counter (CIC-195 B4), the PID, and the job ID.
func (d *Docker) containerName(jobID string) string {
	n := atomic.AddInt64(&containerSeq, 1)
	return fmt.Sprintf("cicado-%d-%s-%08x", os.Getpid(), jobID, uint64(n))
}

// Name implements Executor.
func (d *Docker) Name() string { return "docker" }

// Prepare creates the job's workspace on the host and records the job ID.
// It is this same directory, bind-mounted into every step's container at
// workspaceMountPoint, that carries artifacts from one step to the next.
// Mirrors Shell.Prepare, but kept independent on purpose so the two executors
// never depend on each other's internals — coupling them for a couple of
// lines would be a worse trade than the small duplication it avoids.
func (d *Docker) Prepare(_ context.Context, job Job) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.jobID = job.ID
	root := job.WorkspaceRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("docker: create workspace root: %w", err)
	}
	dir, err := os.MkdirTemp(root, "cicado-job-")
	if err != nil {
		return fmt.Errorf("docker: create workspace: %w", err)
	}
	d.workspace = dir
	return nil
}

// Workspace returns the prepared host workspace directory.
func (d *Docker) Workspace() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.workspace
}

// RunStep runs one step in a fresh container, streaming output to sink as it
// arrives. See the type doc for the fresh-per-step / shared-workspace model
// and the CIC-195 follow-up on killing the container on cancel.
// CIC-195 B4 + CIC-196 B1: step env via --env-file in an OS temp dir;
// the docker client's own env stays at os.Environ().
func (d *Docker) RunStep(ctx context.Context, step Step, sink OutputSink) (Result, error) {
	if len(step.Command) == 0 {
		return Result{}, errors.New("docker: step has no command")
	}
	if step.Image == "" {
		return Result{}, errors.New("docker: step has no image")
	}
	if d.Workspace() == "" {
		return Result{}, errors.New("docker: no prepared workspace to mount")
	}

	stepCtx := ctx
	var cancel context.CancelFunc
	if step.Timeout > 0 {
		stepCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}

	started := time.Now()
	d.mu.Lock()
	jobID := d.jobID
	d.mu.Unlock()

	cName := d.containerName(jobID)
	envFilePath, cancelEnvFile, err := writeEnvFile("", step.Env)
	if err != nil {
		return Result{}, fmt.Errorf("docker: env-file: %w", err)
	}
	defer cancelEnvFile()

	args := dockerRunArgs(d.Workspace(), step, cName, envFilePath)
	//nolint:gosec // Building the run from a workflow's own fields is this package's job.
	cmd := exec.CommandContext(stepCtx, dockerBin, args...)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("docker: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("docker: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("docker: start %q: %w", step.Image, err)
	}

	// Docker does not defer a kill on every return like Shell does. It relies on
	// the Cancel function to terminate the process group only when the step times
	// out or is cancelled, and on killContainer to tear the named container down
	// on every exit path, so a killed client never leaves a --rm container
	// running under the daemon.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stdout, StreamStdout, sink) }()
	go func() { defer wg.Done(); pump(stderr, StreamStderr, sink) }()
	wg.Wait()

	waitErr := cmd.Wait()
	duration := time.Since(started)

	// TimedOut means our own per-step deadline fired, not the job's deadline or a
	// caller's cancel — only those three cancel the wrapped stepCtx, and the two
	// outside causes are reported by the caller via ctx.Err(), not here.
	timedOut := step.Timeout > 0 && errors.Is(stepCtx.Err(), context.DeadlineExceeded)
	d.killContainer(ctx, cName)

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		return Result{ExitCode: 0, Duration: duration}, nil
	case errors.As(waitErr, &exitErr):
		// The step ran and failed. That is a job result, not an executor error.
		code := int32(exitErr.ExitCode())
		if code < 0 {
			// Killed by a signal (timeout/cancel): report the Unix convention of
			// 128+signal so callers can tell "killed" from "exited N".
			code = 128
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code += int32(ws.Signal())
			} else {
				code += int32(syscall.SIGKILL)
			}
		}
		return Result{ExitCode: code, TimedOut: timedOut, Duration: duration}, nil
	default:
		return Result{Duration: duration}, fmt.Errorf("docker: wait for %q: %w", step.Image, waitErr)
	}
}

// Cleanup removes the job's host workspace. The bind mounts into the (already
// gone) step containers detach on their own, so removing the directory is the
// whole cleanup. Mirrors Shell.Cleanup, including its guard against a mangled
// path.
func (d *Docker) Cleanup(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.workspace == "" {
		return nil
	}
	if !filepath.IsAbs(d.workspace) {
		return fmt.Errorf("docker: refusing to remove non-absolute workspace %q", d.workspace)
	}
	if err := os.RemoveAll(d.workspace); err != nil {
		return fmt.Errorf("docker: remove workspace: %w", err)
	}
	d.workspace = ""
	return nil
}

// writeEnvFile writes step.Env to a 0600 env file in a fresh 0700 dir under the
// OS temp dir — not the workspace — so it cannot be globbed or archived as an
// artifact (S2). Keys are validated by isValidEnvKey and values with a newline
// are rejected; both are defenses against a step steering a runner-host variable
// into the container. CIC-196 B1/C1. The workspace argument is accepted for
// call-site symmetry with the executor but is intentionally unused.
func writeEnvFile(_ string, env map[string]string) (string, func(), error) {
	if len(env) == 0 {
		return "", func() {}, nil
	}
	// S2: a 0600 file in a 0700 dir under the OS temp dir, outside the workspace.
	dir, err := os.MkdirTemp("", "cicado-env-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create env dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf []byte
	for _, k := range keys {
		if !isValidEnvKey(k) {
			cleanup()
			return "", func() {}, fmt.Errorf("invalid env key %q", k)
		}
		v := env[k]
		if containsNL(v) {
			cleanup()
			return "", func() {}, fmt.Errorf("env value for %q contains a newline", k)
		}
		buf = append(buf, []byte(k+"="+v+"\n")...)
	}
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write env file: %w", err)
	}
	return path, cleanup, nil
}

// containsNL returns true if s has a newline or carriage return.
func containsNL(s string) bool {
	for _, c := range s {
		if c == '\n' || c == '\r' {
			return true
		}
	}
	return false
}

// isValidEnvKey reports whether s is a valid POSIX environment variable name:
// it must start with an ASCII letter or underscore and contain only ASCII
// alphanumerics and underscores. This blocks newlines, "=", "#", and any
// non-ASCII character that could let docker --env-file read a runner-host
// variable into the untrusted container (CIC-196 C1/C1a).
//
// The name set is deliberately ASCII-only ([A-Za-z_][A-Za-z0-9_]*), not any
// unicode.IsLetter/IsDigit match: docker's env-file parser and the downstream
// host tools that consume the container env all assume ASCII names, so a
// multi-byte rune cannot be smuggled in even when it is a letter in some
// locale.
func isValidEnvKey(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, ch := range s {
		// Any rune above 0x7F is non-ASCII and therefore rejected.
		if ch > 0x7F {
			return false
		}
		b := byte(ch)
		isAlpha := (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
		isNum := b >= '0' && b <= '9'
		if i == 0 {
			// First character: a letter or an underscore, never a digit.
			if !isAlpha && b != '_' {
				return false
			}
		} else if !isAlpha && !isNum && b != '_' {
			// Later characters: alphanumeric or underscore.
			return false
		}
	}
	return true
}

// dockerRunArgs assembles the `docker run` argument slice for one step. It is a
// pure function of its inputs so the exact command a step produces — including
// whether egress is cut off — can be asserted with no daemon at all.
// CIC-196 B1: the step's env is passed via --env-file rather than as repeated
// -e KEY=VALUE flags that would be visible in the docker client's argv;
// containerName is globally unique (CIC-195 B4, atomic counter).
func dockerRunArgs(workspace string, step Step, containerName string, envFilePath string) []string {
	// --rm tears the container down when the step exits, so we never accumulate
	// them on the host.
	args := []string{"run", "--rm", "--name", containerName}

	// Egress is off unless the step opts in. Absence of the flag is the bridge
	// default; presence of `none` is the default-off posture the server sets.
	if !step.NetworkEnabled {
		args = append(args, "--network", "none")
	}

	// The workspace mount is what lets a fresh container per step share state:
	// the host workspace is mounted in and made the working directory so a file a
	// step writes is where the next one looks.
	args = append(args,
		"-v", workspace+":"+workspaceMountPoint,
		"-w", workspaceMountPoint,
	)

	// CIC-196 B1: env passes via --env-file — a 0600 host file the container
	// owns — instead of -e flags, but only when there is env to inject.
	if envFilePath != "" {
		args = append(args, "--env-file", envFilePath)
	}

	// --entrypoint "" disables the image's ENTRYPOINT so the step's command runs
	// directly -- exactly as the shell executor runs it -- instead of being
	// handed to the image's entrypoint as trailing arguments. Without this, an
	// image that sets an ENTRYPOINT (e.g. ENTRYPOINT ["python"]) would receive a
	// run: step's shell invocation as if it were an argument to that entrypoint,
	// and the step would never execute. This is the CIC-152 fix.
	args = append(args, "--entrypoint", "")

	// The image is the first bare token after the flags; the step's command
	// follows it, verbatim.
	args = append(args, step.Image)
	args = append(args, step.Command...)
	return args
}

// NewExecutor selects an Executor by name, the value carried in a job's
// `executor` field.
//
// An empty name means "shell", preserving the historical default so existing
// jobs run exactly as they did before this switch existed. An unknown name is an
// error, not a silent fallback: a job that names a driver this runner lacks
// should be rejected at the seam, never quietly run some other way.
func NewExecutor(name string) (Executor, error) {
	switch name {
	case "", "shell":
		return NewShell(), nil
	case "cicanscript":
		return NewCicanscript(), nil
	case "docker":
		return NewDocker(), nil
	default:
		return nil, fmt.Errorf("unknown executor %q", name)
	}
}
