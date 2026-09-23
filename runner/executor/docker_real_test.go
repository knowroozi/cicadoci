//go:build docker

// This file proves the docker executor against a real container runtime. It is
// gated behind the "docker" build tag so the default `go test ./... -race` suite
// (and its "fail if any test skipped" CI guard) never depends on a daemon being
// present — CI's test job has no docker. Run it with:
//
//	go test -tags docker ./runner/executor/...
//
// It is a smoke test, not a substitute for the hermetic arg-level assertions in
// docker_test.go: those pin down that the executor emits `--network none` for a
// network-off step; what this adds is that a real runtime will actually run that
// line. The guarantee that egress is truly cut is docker's `--network none`, not
// a property of our code to re-prove here.
package executor

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDockerRealEgressOff(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; build the 'docker' tag on a host with a runtime to run this")
	}

	// Real daemon, real alpine. Restore so the package default is unchanged.
	orig := dockerBin
	dockerBin = "docker"
	t.Cleanup(func() { dockerBin = orig })

	d := NewDocker()
	if err := d.Prepare(context.Background(), Job{ID: "real"}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() { _ = d.Cleanup(context.Background()) })

	sink := &collectingSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A trivial command in a network-off container must run to completion.
	res, err := d.RunStep(ctx, Step{
		Index:          0,
		Name:           "real-egress-off",
		Image:          "alpine",
		Command:        []string{"sh", "-c", "echo ran-off-network"},
		NetworkEnabled: false,
	}, sink)
	if err != nil {
		t.Fatalf("run step: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if got := sink.out(); !strings.Contains(got, "ran-off-network") {
		t.Errorf("stdout = %q, want it to contain the step's echo", got)
	}
}
