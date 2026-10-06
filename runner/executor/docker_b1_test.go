//go:build docker

// docker_b1_test.go — byte-exact test for CIC-210 B1.
//
// Requires a real Docker daemon. Run with:
//   go test -tags docker ./runner/executor/

package executor

import (
	"context"
	"testing"
)

// TestB1_ByteExactPemPrintenv runs a minimal alpine container whose
// only env var is a multi-line PEM certificate value including a
// trailing new-line. The B1 path passes it via --env K (key only,
// value in cmd.Env); the container prints it via printenv.
//
// printenv appends exactly one trailing "\n" after each variable's
// value, so the expected output is val + "\n".
func TestB1_ByteExactPemPrintenv(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\nAB\nCD\n-----END CERTIFICATE-----\n"
	want := pem + "\n" // printenv appends one trailing \n

	sink := &collectingSink{}
	ctx := context.Background()
	d := NewDocker()
	if err := d.Prepare(ctx, Job{WorkspaceRoot: t.TempDir()}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	step := Step{
		Index:          0,
		Name:           "pe-printenv",
		Image:          "alpine:3.19",
		Command:        []string{"printenv", "PEM_KEY"},
		Env:            map[string]string{"PEM_KEY": pem},
		NetworkEnabled: false,
	}
	res, err := d.RunStep(ctx, step, sink)
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", res.ExitCode, sink.err())
	}
	got := sink.out()
	if got != want {
		t.Fatalf("printenv output = %q (%d bytes), want %q (%d bytes)", got, len(got), want, len(want))
	}
	t.Logf("PEM printenv byte-exact: %d bytes (val + \\n)", len(got))
}

// TestB1_ByteExactCRLinPem verifies that \r\n in a multi-line value
// is preserved via printenv.
func TestB1_ByteExactCRLinPem(t *testing.T) {
	val := "BEGIN\r\nMID\r\nEND\n" // three \r\n, one trailing \n
	want := val + "\n"             // printenv adds one more \n

	sink := &collectingSink{}
	ctx := context.Background()
	d := NewDocker()
	if err := d.Prepare(ctx, Job{WorkspaceRoot: t.TempDir()}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	step := Step{
		Index:          0,
		Name:           "crlf-printenv",
		Image:          "alpine:3.19",
		Command:        []string{"printenv", "VAR"},
		Env:            map[string]string{"VAR": val},
		NetworkEnabled: false,
	}
	res, err := d.RunStep(ctx, step, sink)
	if err != nil {
		t.Fatalf("RunStep: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	got := sink.out()
	if got != want {
		t.Fatalf("printenv CRLF output = %q (%d bytes), want %q (%d bytes)", got, len(got), want, len(want))
	}
	t.Logf("CRLF printenv byte-exact: %d bytes", len(got))
}
