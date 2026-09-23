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
// A known M1 limitation, deferred to CIC-16: on timeout or cancellation we kill
// the `docker run` client (via exec.CommandContext), but we do not yet track the
// container id to tear the container down with it. A killed client can leave its
// `--rm` container running under the daemon. CIC-16 adds that path: cancel the
// step and destroy its container. Today we only prevent the leak on the happy and
// non-zero paths where the client exits cleanly.
//
// Env is passed as repeated `-e KEY=VALUE` flags. Those values are briefly
// visible in the process listing while the daemon forks — and this is a *wider*
// surface than Shell's buildEnv, which is a common mistake to equate them:
// Shell's cmd.Env lives in the child's environment block, readable via
// /proc/<pid>/environ and gated to the owning UID/root, whereas
// `docker run -e KEY=VALUE` puts the value in the argv of the `docker` client
// process, world-readable via `ps` / /proc/<pid>/cmdline by *any* local user,
// no privilege needed — a materially wider blast radius, not the same one. That
// says nothing about what flows through *today*: StepSpec.env / JobSpec.env are
// "plaintext, non-secret env only" (runner.proto) and secret values arrive only
// via GetJobSecrets against the live lease, which is not wired into this
// executor yet, so no live secret reaches the `-e` path now. Flagging it here so
// whoever adds secret injection (CIC-16 or a follow-up) doesn't reuse `-e` for
// it by default — the right hardening is a 0600 host file on a mounted volume a
// container owns, not argv. A documented, deliberate M1 framing, not an
// oversight.
type Docker struct {
	workspace string
	mu        sync.Mutex
}

// NewDocker builds a docker executor.
func NewDocker() *Docker { return &Docker{} }

// Name implements Executor.
func (d *Docker) Name() string { return "docker" }

// Prepare creates the job's workspace on the host. It is this same directory,
// bind-mounted into every step's container at workspaceMountPoint, that carries
// artifacts from one step to the next. Mirrors Shell.Prepare, but kept
// independent on purpose so the two executors never depend on each other's
// internals — coupling them for a couple of lines would be a worse trade than
// the small duplication it avoids.
func (d *Docker) Prepare(_ context.Context, job Job) error {
	d.mu.Lock()
	defer d.mu.Unlock()

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
// arrives. See the type doc for the fresh-per-step / shared-workspace model and
// the CIC-16 follow-up on killing the container on cancel.
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

	//nolint:gosec // Building the container run from a workflow's own fields is this package's job.
	cmd := exec.CommandContext(stepCtx, dockerBin, dockerRunArgs(d.Workspace(), step)...)

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

	// Unlike Shell we do not kill a process group on the way out: the child is a
	// `docker run` client, a leaf process with no host children of its own to
	// leak. The container it started is the daemon's responsibility, and
	// destroying that on cancel is exactly what CIC-16 adds; see the type doc.
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

// dockerRunArgs assembles the `docker run` line for one step. It is a pure
// function of its inputs so the exact command a step produces — including whether
// egress is cut off — can be asserted with no daemon at all.
func dockerRunArgs(workspace string, step Step) []string {
	// --rm tears the container down when the step exits, so we never accumulate
	// them on the host.
	args := []string{"run", "--rm"}

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

	// Repeating `-e` per key rather than an env file keeps secret values off the
	// host filesystem (a mounted file would be a second, quieter place to leak
	// them). Keys are sorted purely so the command is deterministic and testable.
	for _, kv := range sortedEnvs(step.Env) {
		args = append(args, "-e", kv)
	}

	// The image is the first bare token after the flags; the step's command
	// follows it, verbatim.
	args = append(args, step.Image)
	args = append(args, step.Command...)
	return args
}

// sortedEnvs renders a step's environment as ordered -e arguments.
func sortedEnvs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
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
	case "docker":
		return NewDocker(), nil
	default:
		return nil, fmt.Errorf("unknown executor %q", name)
	}
}
