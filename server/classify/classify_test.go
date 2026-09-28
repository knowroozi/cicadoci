package classify

import "testing"

func TestClassifyFlaky(t *testing.T) {
	r := Classify(Input{ExitCode: 0, IsRerun: true})
	if r.Classification != Flaky {
		t.Fatalf("expected Flaky, got %q", r.Classification)
	}
	if r.Confidence < 0.9 {
		t.Fatalf("expected high confidence, got %f", r.Confidence)
	}
}

func TestClassifyOOMExit137(t *testing.T) {
	r := Classify(Input{ExitCode: 137, StepName: "build"})
	if r.Classification != OOM {
		t.Fatalf("expected OOM, got %q", r.Classification)
	}
}

func TestClassifyOOMLogPattern(t *testing.T) {
	r := Classify(Input{
		ExitCode: 1,
		StepName: "test",
		LogTail:  []string{"go test -v", "out of memory", "killed process"},
	})
	if r.Classification != OOM {
		t.Fatalf("expected OOM, got %q", r.Classification)
	}
}

func TestClassifyTimeout(t *testing.T) {
	r := Classify(Input{ExitCode: 124, StepName: "deploy"})
	if r.Classification != Timeout {
		t.Fatalf("expected Timeout, got %q", r.Classification)
	}
}

func TestClassifyTimeoutLog(t *testing.T) {
	r := Classify(Input{
		ExitCode: 1,
		StepName: "test",
		LogTail:  []string{"deadline exceeded"},
	})
	if r.Classification != Timeout {
		t.Fatalf("expected Timeout, got %q", r.Classification)
	}
}

func TestClassifyAuth(t *testing.T) {
	r := Classify(Input{
		ExitCode: 1,
		StepName: "deploy",
		LogTail:  []string{"permission denied pushing image"},
	})
	if r.Classification != Auth {
		t.Fatalf("expected Auth, got %q", r.Classification)
	}
}

func TestClassifyDepResolution(t *testing.T) {
	r := Classify(Input{
		ExitCode: 1,
		StepName: "build",
		LogTail:  []string{"go: cannot find package", "go mod download failed"},
	})
	if r.Classification != DepResolution {
		t.Fatalf("expected DepResolution, got %q", r.Classification)
	}
}

func TestClassifyTestStep(t *testing.T) {
	r := Classify(Input{
		ExitCode: 1,
		StepName: "unit_test",
		LogTail:  []string{"FAIL: TestFoo"},
	})
	if r.Classification != Test {
		t.Fatalf("expected Test, got %q", r.Classification)
	}
}

func TestClassifyInfra(t *testing.T) {
	r := Classify(Input{
		ExitCode: 1,
		StepName: "build",
		LogTail:  []string{"dial tcp: connection refused"},
	})
	if r.Classification != Infra {
		t.Fatalf("expected Infra, got %q", r.Classification)
	}
}

func TestClassifyUserScript(t *testing.T) {
	r := Classify(Input{ExitCode: 1, StepName: "echo"})
	if r.Classification != UserScript {
		t.Fatalf("expected UserScript, got %q", r.Classification)
	}
}

func TestClassifyUnknownOnSuccess(t *testing.T) {
	r := Classify(Input{ExitCode: 0})
	if r.Classification != Unknown {
		t.Fatalf("expected Unknown, got %q", r.Classification)
	}
}

func TestClassifyReasonsPopulated(t *testing.T) {
	cases := []Input{
		{ExitCode: 0, IsRerun: true},
		{ExitCode: 137},
		{ExitCode: 124},
		{ExitCode: 1, StepName: "build", LogTail: []string{"permission denied"}},
		{ExitCode: 1, StepName: "build", LogTail: []string{"go: cannot find package"}},
		{ExitCode: 1, StepName: "test"},
		{ExitCode: 1, StepName: "build"},
		{ExitCode: 0},
	}
	for _, in := range cases {
		r := Classify(in)
		if r.Reason == "" {
			t.Errorf("Classify(%+v): empty Reason", in)
		}
	}
}

// TestSuccessWithMisleadingLogs verifies that a successful run (exit 0)
// with misleading log patterns (e.g. "out of memory" in an info message)
// is NOT misclassified.
func TestSuccessWithMisleadingLogs(t *testing.T) {
	tests := []struct {
		logs []string
	}{
		{logs: []string{"out of memory in test output", "PASS"}},
		{logs: []string{"timeout: 10s", "all tests passed"}},
		{logs: []string{"403 forbidden when accessing external API"}},
	}
	for _, tt := range tests {
		got := Classify(Input{ExitCode: 0, LogTail: tt.logs})
		if got.Classification != Unknown {
			t.Errorf("expected Unknown for successful run, got %s", got.Classification)
		}
	}
}

// TestCaseInsensitivePattern verifies that patterns like "FAIL" match
// case-insensitively (lowercase log text).
func TestCaseInsensitivePattern(t *testing.T) {
	got := Classify(Input{
		ExitCode: 1,
		StepName: "unit",
		LogTail:  []string{"--- FAIL: TestFoo (0.01s)"},
	})
	if got.Classification != Test {
		t.Errorf("expected Test, got %s", got.Classification)
	}
}

// TestClassifyInfraFailedConnection is a regression test: "connection failed"
// (containing the substring "fail") must not be misclassified as Test.
func TestClassifyInfraFailedConnection(t *testing.T) {
	got := Classify(Input{
		ExitCode: 1,
		StepName: "deploy",
		LogTail:  []string{"dial tcp 10.0.0.5:443: connection failed"},
	})
	if got.Classification != Infra {
		t.Fatalf("got %q, want Infra", got.Classification)
	}
}

// TestClassifyBuildFail is a regression test: "build failed" on a
// non-test step must not be misclassified as Test.
func TestClassifyBuildFail(t *testing.T) {
	got := Classify(Input{
		ExitCode: 1,
		StepName: "build",
		LogTail:  []string{"npm run build failed with exit code 1"},
	})
	if got.Classification != UserScript {
		t.Fatalf("got %q, want UserScript", got.Classification)
	}
}
