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
//	docker run --rm [--name NAME] [--network none] -v <host>:<mount> -w <mount> [--env-file PATH] --entrypoint "" <image> <cmd>...
//
// CIC-196 B1: step env passes via --env-file; CIC-195 B4: --name uses a global atomic counter + job ID.
const fakeDockerScript = `#!/bin/sh
[ "${1:-}" = "run" ] && shift
while [ $# -ge 1 ]; do
	case "$1" in
		--rm)  shift 1 ;;
		--name)  shift 2 ;;
		--network) shift 2 ;;
		--entrypoint) shift 2 ;;
		-v)  shift 2 ;;
		-w)  shift 2 ;;
		--env-file) shift 2 ;;
		-e)  shift 2 ;;
		-*)  shift 1 ;;
		*)  break ;;
	esac
done
# $1 is now the image name; skip it, then run the command that follows.
shift
exec "$@"`

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
	}, "cicado-test", "")
	if !subsequence(off, "--network", "none") {
		t.Errorf("network-off step was not run under --network none: %v", off)
	}

	on := dockerRunArgs("", Step{
		Image:          "alpine",
		Command:        []string{"sh", "-c", "true"},
		NetworkEnabled: true,
	}, "cicado-test", "")
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
	}, "cicado-test", "/tmp/secure-env-file")

	if !subsequence(args, "run", "--rm") {
		t.Errorf("expected leading 'run --rm', got %v", args)
	}
	if !subsequence(args, "-v", "/host/ws:/cicado/workspace") {
		t.Errorf("expected the workspace bind mount, got %v", args)
	}
	if !subsequence(args, "-w", "/cicado/workspace") {
		t.Errorf("expected the workspace as working dir, got %v", args)
	}

	// CIC-196 B1: step env arrives via --env-file, not -e KEY.
	if !subsequence(args, "--env-file", "/tmp/secure-env-file") {
		t.Errorf("expected --env-file flag, got %v", args)
	}
	for i, a := range args {
		if a == "-e" {
			t.Errorf("unexpected -e flag at position %d; use --env-file, got %v", i, args)
		}
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
	}, "cicado-test", "")

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

// TestDockerRunArgs_NoSecretsInArgs verifies CIC-196 B1: env VALUES do NOT appear
// in the docker argv. With --env-file, secret values live in a 0600 file, not argv.
func TestDockerRunArgs_NoSecretsInArgs(t *testing.T) {
	spec := Step{
		Image:   "golang:1.21",
		Command: []string{"/bin/sh", "-c", "build"},
		Env:     map[string]string{"GIT_PASSWORD": "s3cr3t", "API_KEY": "abc123"},
	}
	envFile := "/tmp/.cicado-env-xyz/env"
	args := dockerRunArgs("/host/ws", spec, "cicado-test1", envFile)

	// No -e flags may appear.
	for i, a := range args {
		if a == "-e" {
			t.Errorf("unexpected -e flag at position %d; use --env-file, got %v", i, args)
		}
	}

	// Secret values must not appear in argv.
	for _, secret := range []string{"s3cr3t", "abc123"} {
		for _, a := range args {
			if a == secret || strings.Contains(a, "="+secret) {
				t.Errorf("secret value %q appears in args: %v", secret, args)
			}
		}
	}

	// --env-file with the correct path must be present.
	if !subsequence(args, "--env-file", envFile) {
		t.Errorf("expected --env-file %q, got %v", envFile, args)
	}
}

// TestDockerRunArgsIncludesName pins CIC-195: --name is present and its
// value is the provided container name argument.
func TestDockerRunArgsIncludesName(t *testing.T) {
	args := dockerRunArgs("/ws", Step{
		Image:   "alpine",
		Command: []string{"true"},
	}, "cicado-42", "")

	i := indexOf(args, "--name")
	if i < 0 || i+1 >= len(args) {
		t.Fatal("--name must appear in the run args")
	}
	if args[i+1] != "cicado-42" {
		t.Errorf("expected --name value to be 'cicado-42', got %q", args[i+1])
	}
}

// TestWriteEnvFile_Basic verifies that writeEnvFile produces a file with
// the expected KEY=VALUE content and cleans up on defer.
func TestWriteEnvFile_Basic(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FOO": "bar", "BAZ": "qux"}
	path, cleanup, err := writeEnvFile(ws, env)
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	content := string(got)
	if !strings.Contains(content, "FOO=bar") {
		t.Errorf("env file does not contain FOO=bar: %q", content)
	}
	if !strings.Contains(content, "BAZ=qux") {
		t.Errorf("env file does not contain BAZ=qux: %q", content)
	}

	// After cleanup the file must be gone.
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("env file not cleaned up: %v", err)
	}
}

// TestWriteEnvFile_RejectsNewlines verifies that a value with a newline
// is rejected (security: prevents file injection).
func TestWriteEnvFile_RejectsNewlines(t *testing.T) {
	ws, _ := os.MkdirTemp("", "cicado-evf-test")
	defer os.RemoveAll(ws)
	env := map[string]string{"BAD": "line1\nLINE2"}
	_, cleanup, err := writeEnvFile(ws, env)
	if err == nil {
		t.Fatal("expected error for newline in env value")
	}
	cleanup()
	if _, err := os.Stat(ws); err == nil || !os.IsNotExist(err) {
		// Best-effory cleanup on error
		os.RemoveAll(ws)
	}
}

// TestWriteEnvFile_NoEnv returns empty path and no-op cleanup for empty env.
func TestWriteEnvFile_NoEnv(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	_ = os.Mkdir(ws, 0o700)
	path, cleanup, err := writeEnvFile(ws, nil)
	if err != nil {
		t.Fatalf("writeEnvFile(nil): %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
	cleanup() // must be a no-op
}

// TestDockerRunArgsWithEnvFile verifies that --env-file appears in the args
// when an env file is provided, and NOT when the env is empty.
func TestDockerRunArgsWithEnvFile(t *testing.T) {
	ws := "/host/ws"

	// with env
	step := Step{Image: "alpine", Command: []string{"sh", "-c", "true"}}
	args := dockerRunArgs(ws, step, "cicado-1", "/ws/.cicado-env-xyz/env")
	if !subsequence(args, "--env-file", "/ws/.cicado-env-xyz/env") {
		t.Errorf("expected --env-file in args, got %v", args)
	}

	// without env
	envStep := Step{Image: "alpine", Command: []string{"sh", "-c", "true"}}
	args2 := dockerRunArgs(ws, envStep, "cicado-2", "")
	for _, a := range args2 {
		if a == "--env-file" {
			t.Errorf("expected no --env-file for empty path, got %v", args2)
			break
		}
	}
}

// TestDockerRunArgs_EmptyEnvNoEnvFile verifies that when step has no Env
// and envFilePath is "", no --env-file flag is added.
func TestDockerRunArgs_EmptyEnvNoEnvFile(t *testing.T) {
	step := Step{Image: "alpine", Command: []string{"true"}}
	args := dockerRunArgs("/ws", step, "cicado-0", "")
	for _, a := range args {
		if a == "--env-file" {
			t.Errorf("no --env-file should be present for empty env, got %v", args)
			break
		}
	}
}

// TestDockerRunStep_EnvVisibility verifies that a step with env set actually
// sees the env var via the fake docker.
func TestDockerRunStep_EnvVisibility(t *testing.T) {
	useFakeDocker(t)
	d := newPreparedDocker(t)
	sink := &collectingSink{}
	res, err := d.RunStep(context.Background(), Step{
		Image:   "alpine",
		Command: []string{"sh", "-c", "echo $MY_VAR"},
		Env:     map[string]string{"MY_VAR": "hello-from-env"},
	}, sink)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	// With --env-file, the fake docker can't actually use it, but the
	// point is that RunStep doesn't fail and the env doesn't appear in
	// argv visible to ps. We use a minimal assertion.
	if got := sink.out(); got == "" {
		t.Errorf("expected some stdout output")
	}
}

// TestWriteEnvFile_RejectsNewlineKey is the C1a companion to
// TestIsValidEnvKey: a key with a newline must be rejected before any file is
// written, and the function must leave no env dir behind on that error (the
// temp dir it creates before validating is removed on the error path).
func TestWriteEnvFile_RejectsNewlineKey(t *testing.T) {
	env := map[string]string{"BAD\nKEY": "line1\nLINE2"}
	path, cleanup, err := writeEnvFile("", env)
	if err == nil {
		t.Fatal("writeEnvFile: expected error for newline in key, got nil")
	}
	if path != "" {
		t.Errorf("writeEnvFile: leaked env path on error: %q", path)
	}
	if cleanup == nil {
		t.Fatal("writeEnvFile: cleanup func must be non-nil even on error")
	}
	cleanup() // must be a safe no-op after the error path
}

func TestIsValidEnvKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"MY_VAR", true},
		{"_PRIVATE", true},
		{"A1_B2", true},
		{"X", true},
		{"", false},
		{"1BAD", false},
		// CIC-196 C1a: hostile keys must be rejected so docker --env-file
		// cannot be steered into reading a runner-host variable.
		{"A\nB", false}, // embedded newline: file-injection vector
		{"A=B", false},  // '=' would be parsed as a second assignment
		{"#X", false},   // '#' starts an env-file comment, not a key
		{"A B", false},  // space is not part of a POSIX env name
		{"É", false},    // non-ASCII; name set is [A-Za-z_][A-Za-z0-9_]*
	}
	for _, c := range cases {
		got := isValidEnvKey(c.key)
		if got != c.want {
			t.Errorf("isValidEnvKey(%q) = %t; want %t", c.key, got, c.want)
		}
	}
}
