package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker is a stand-in for `docker run ...` that needs no daemon. It
// understands only the exact flag grammar dockerRunArgs emits, then execs the
// container's own command on the host, so a step's plumbing (output streaming,
// exit codes, timeout wiring) can be exercised with no runtime present. It is a
// test double, not a real runtime: it does not isolate, so on its own it cannot
// prove egress is cut off — that is docker_real_test.go's job when a daemon is
// available.
//
// Grammar it consumes (every value-taking flag precedes the image, which is the
// first bare token; the command follows the image):
//
//	docker run --rm [--network none] -v <host>:<mount> -w <mount> [-e K=V ...] --entrypoint "" <image> <cmd>...
const fakeDockerScript = `#!/bin/sh
[ "${1:-}" = run ] && shift
while [ $# -ge 1 ]; do
	case "$1" in
		--rm) shift 1 ;;
		--network) shift 2 ;;
		--entrypoint) shift 2 ;;
		-v) shift 2 ;;
		-w) shift 2 ;;
		-e) shift 2 ;;
		-*) shift 1 ;;
		*) break ;;
	esac
done
# $1 is now the image name; skip it, then run the command that follows.
shift
exec "$@"
`

// useFakeDocker points the package's docker binary at the script above for the
// duration of a test and restores whatever it found on cleanup.
func useFakeDocker(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(fakeDockerScript), 0o750); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	orig := dockerBin
	dockerBin = path
	t.Cleanup(func() { dockerBin = orig })
}

// newPreparedDocker prepares a docker executor with its own throwaway workspace.
func newPreparedDocker(t *testing.T) *Docker {
	t.Helper()
	d := NewDocker()
	if err := d.Prepare(context.Background(), Job{
		ID:            "job-1",
		WorkspaceRoot: t.TempDir(),
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() { _ = d.Cleanup(context.Background()) })
	return d
}

// collectingSink gathers output from both pump goroutines, safely. (A private
// copy of the same idea shell_test uses; the two live in different packages so
// they cannot share one definition.)
type collectingSink struct {
	mu     sync.Mutex
	stdout strings.Builder
	stderr strings.Builder
}

func (c *collectingSink) Write(stream Stream, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch stream {
	case StreamStderr:
		c.stderr.Write(data)
	case StreamStdout:
		c.stdout.Write(data)
	}
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

// TestDockerRunArgsNetworkOff is the egress guarantee at the assembly layer: a
// step that did not opt into network must be run under `--network none`, and one
// that did must not be. No daemon is required — the composition is what actually
// drives the isolation, so asserting it directly is the real check.
func TestDockerRunArgsNetworkOff(t *testing.T) {
	off := dockerRunArgs("", Step{
		Image:   "alpine",
		Command: []string{"sh", "-c", "true"},
		// NetworkEnabled is false: the default-off posture.
	})
	if !subsequence(off, "--network", "none") {
		t.Errorf("network-off step was not run under --network none: %v", off)
	}

	on := dockerRunArgs("", Step{
		Image:          "alpine",
		Command:        []string{"sh", "-c", "true"},
		NetworkEnabled: true,
	})
	if i := indexOf(on, "--network"); i >= 0 {
		t.Errorf("network-enabled step forced a --network mode it should not have: %v", on[i:min(i+2, len(on))])
	}
}

// TestDockerRunArgsShapes pins the rest of the line: --rm is always present
// (containers are torn down, never accumulated), the workspace is mounted and set
// as the working directory, env arrives as sorted -e args, and the image precedes
// the command.
func TestDockerRunArgsShapes(t *testing.T) {
	args := dockerRunArgs("/host/ws", Step{
		Image:   "alpine",
		Command: []string{"/bin/sh", "-c", "echo hi"},
		Env:     map[string]string{"ZED": "1", "ALPHA": "2"},
	})

	if !subsequence(args, "run", "--rm") {
		t.Errorf("expected leading 'run --rm', got %v", args)
	}
	if !subsequence(args, "-v", "/host/ws:/cicado/workspace") {
		t.Errorf("expected the workspace bind mount, got %v", args)
	}
	if !subsequence(args, "-w", "/cicado/workspace") {
		t.Errorf("expected the workspace as working dir, got %v", args)
	}

	// The two -e values, in sorted order: ALPHA before ZED.
	var envs []string
	for i, a := range args {
		if a == "-e" && i+1 < len(args) {
			envs = append(envs, args[i+1])
		}
	}
	if len(envs) != 2 || envs[0] != "ALPHA=2" || envs[1] != "ZED=1" {
		t.Errorf("expected sorted -e ALPHA=2 ZED=1, got %v", envs)
	}

	// The image precedes the command.
	if imgI, echoI := indexOf(args, "alpine"), indexOf(args, "/bin/sh"); imgI == -1 || echoI == -1 || imgI > echoI {
		t.Errorf("expected image before command; got %v", args)
	}
}

// TestDockerRunArgsDisablesImageEntrypoint pins the CIC-152 fix: the run line
// forces --entrypoint "" so a step's command runs directly instead of being
// handed to the image's ENTRYPOINT as trailing arguments. Without this, an
// image that sets an ENTRYPOINT (e.g. ["python"]) would receive a run: step's
// shell as an argument and the step would never execute.
func TestDockerRunArgsDisablesImageEntrypoint(t *testing.T) {
	args := dockerRunArgs("/host/ws", Step{
		Image:   "python", // a hypothetical image with ENTRYPOINT ["python"]
		Command: []string{"/bin/sh", "-c", "echo build"},
	})

	// --entrypoint must appear and its value must be the empty string, which is
	// what disables the image's entrypoint.
	eI := indexOf(args, "--entrypoint")
	if eI == -1 {
		t.Fatalf("expected --entrypoint in run args, got %v", args)
	}
	if eI+1 >= len(args) || args[eI+1] != "" {
		t.Fatalf("expected --entrypoint with an empty value; args after --entrypoint = %v", args[eI:])
	}

	// It must precede the image, and the command must still follow the image:
	// --rm ... --entrypoint "" <image> <cmd>...
	imgI := indexOf(args, "python")
	cmdI := indexOf(args, "echo build")
	if imgI == -1 || cmdI == -1 {
		t.Fatalf("expected image and command in args: %v", args)
	}
	if eI >= imgI || imgI >= cmdI {
		t.Errorf("expected --entrypoint before image before command; got --entrypoint@%d image@%d cmd@%d in %v", eI, imgI, cmdI, args)
	}
}

func TestDockerSelector(t *testing.T) {
	cases := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{name: "", want: "shell"},
		{name: "shell", want: "shell"},
		{name: "docker", want: "docker"},
		{name: "bogus", wantErr: true},
	}
	for _, tc := range cases {
		exec, err := NewExecutor(tc.name)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NewExecutor(%q) = nil error, want error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("NewExecutor(%q) error: %v", tc.name, err)
			continue
		}
		if got := exec.Name(); got != tc.want {
			t.Errorf("NewExecutor(%q).Name() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDockerRunsStep runs a step through the full plumbing with the fake runtime
// and checks both streams plus a zero exit.
func TestDockerRunsStep(t *testing.T) {
	useFakeDocker(t)
	d := newPreparedDocker(t)
	sink := &collectingSink{}

	// HOME is pointed at the workspace mount so a write lands where a later step
	// would look; the fake runtime honors the -w we pass but a real runtime does
	// not need it for this check.
	res, err := d.RunStep(context.Background(), Step{
		Index:   0,
		Name:    "hello",
		Image:   "alpine",
		Command: []string{"sh", "-c", "echo hello from cicado; echo oops >&2"},
	}, sink)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if got := sink.out(); !strings.Contains(got, "hello from cicado") {
		t.Errorf("stdout = %q, want it to contain the echo", got)
	}
	if got := sink.err(); !strings.Contains(got, "oops") {
		t.Errorf("stderr = %q, want it to contain the error text", got)
	}
}

// TestDockerNonZeroExitIsNotAnError: a step that exits non-zero is a job result,
// not an executor error that stops the run.
func TestDockerNonZeroExitIsNotAnError(t *testing.T) {
	useFakeDocker(t)
	d := newPreparedDocker(t)

	res, err := d.RunStep(context.Background(), Step{
		Image:   "alpine",
		Command: []string{"sh", "-c", "exit 42"},
	}, &collectingSink{})
	if err != nil {
		t.Fatalf("a failing step returned an executor error: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", res.ExitCode)
	}
}

// TestDockerTimeoutKillsStep: the per-step deadline stops a hung step promptly and
// flags it as a timeout, not a clean zero exit.
func TestDockerTimeoutKillsStep(t *testing.T) {
	useFakeDocker(t)
	d := newPreparedDocker(t)

	start := time.Now()
	res, err := d.RunStep(context.Background(), Step{
		Image:   "alpine",
		Command: []string{"sh", "-c", "sleep 30"},
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

// TestDockerRequiresImageAndCommand guards the two inputs a step must supply.
func TestDockerRequiresImageAndCommand(t *testing.T) {
	useFakeDocker(t)
	d := newPreparedDocker(t)

	if _, err := d.RunStep(context.Background(), Step{Image: "alpine"}, &collectingSink{}); err == nil {
		t.Error("expected an error for a step with no command")
	}
	if _, err := d.RunStep(context.Background(), Step{Command: []string{"true"}}, &collectingSink{}); err == nil {
		t.Error("expected an error for a step with no image")
	}
}

// TestDockerCleanupRemovesWorkspace confirms the host workspace is gone after
// cleanup.
func TestDockerCleanupRemovesWorkspace(t *testing.T) {
	d := NewDocker()
	if err := d.Prepare(context.Background(), Job{WorkspaceRoot: t.TempDir()}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ws := d.Workspace()
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("workspace not created: %v", err)
	}
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Error("workspace still exists after cleanup")
	}
}

// --- arg-assertion helpers, kept local to this test file ---

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// subsequence reports whether wants appear in args in order (not necessarily
// contiguously), so it tolerates other flags landing between the ones it checks.
func subsequence(args []string, wants ...string) bool {
	pos := 0
	for _, w := range wants {
		found := false
		for i := pos; i < len(args); i++ {
			if args[i] == w {
				pos = i + 1
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
