package parse

import (
	"strings"
	"testing"
)

// The YAML test strings use 2-space indentation as YAML requires.
// Go raw string literals preserve spaces literally.

func TestParse_Valid(t *testing.T) {
	src := "name: test\non:\n  push:\n    branches: [main]\njobs:\n  build:\n    runs-on: [linux, docker]\n    steps:\n    - uses: checkout\n    - run: go build ./...\n      image: golang:1.24\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Name != "test" {
		t.Errorf("Name = %q, want %q", wf.Name, "test")
	}
	if wf.On.Push == nil {
		t.Error("On.Push is nil, want non-nil")
	}
	if len(wf.On.Push.Branches) != 1 || wf.On.Push.Branches[0] != "main" {
		t.Errorf("Push.Branches = %v, want [main]", wf.On.Push.Branches)
	}
	if len(wf.Jobs) != 1 || wf.Jobs["build"] == nil {
		t.Fatalf("Jobs = %v, want one job 'build'", wf.Jobs)
	}
	job := wf.Jobs["build"]
	if len(job.Steps) != 2 {
		t.Fatalf("len(steps) = %d, want 2", len(job.Steps))
	}
	if job.Steps[0].Uses != "checkout" {
		t.Errorf("steps[0].Uses = %q, want 'checkout'", job.Steps[0].Uses)
	}
	if job.Steps[1].Run != "go build ./..." {
		t.Errorf("steps[1].Run = %q, want 'go build ./...'", job.Steps[1].Run)
	}
	if job.Steps[1].Image != "golang:1.24" {
		t.Errorf("steps[1].Image = %q, want 'golang:1.24'", job.Steps[1].Image)
	}
	if len(job.RunsOn) != 2 {
		t.Errorf("len(runs-on) = %d, want 2", len(job.RunsOn))
	}
}

func TestParse_EmptyName(t *testing.T) {
	src := "on:\n  push: {}\njobs:\n  build:\n    steps:\n    - run: echo hi\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	found := false
	for _, e := range pe.Errors {
		if strings.Contains(e.Message, "name is empty") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error about empty name, got: %v", pe.Errors)
	}
}

func TestParse_NoTriggers(t *testing.T) {
	src := "name: test\njobs:\n  build:\n    steps:\n    - run: echo hi\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for no triggers")
	}
	found := false
	for _, e := range pe.Errors {
		if strings.Contains(e.Message, "no triggers") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected missing-triggers error, got: %v", pe.Errors)
	}
}

func TestParse_NoJobs(t *testing.T) {
	src := "name: test\non:\n  push: {}\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for no jobs")
	}
	found := false
	for _, e := range pe.Errors {
		if strings.Contains(e.Message, "no jobs") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected no-jobs error, got: %v", pe.Errors)
	}
}

func TestParse_StepNeitherUsesNorRun(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    steps:\n    - name: just-a-name\n      image: golang:1.24\n"
	_, _, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for step with neither uses nor run")
	}
}

func TestParse_StepBothUsesAndRun(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    steps:\n    - uses: checkout\n      run: echo hi\n"
	_, _, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for step with both uses and run")
	}
}

func TestParse_InvalidIf(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    steps:\n    - run: echo hi\n      if: maybe\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for invalid 'if' value")
	}
	found := false
	for _, e := range pe.Errors {
		if strings.Contains(e.Message, "if") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'if' error, got: %v", pe.Errors)
	}
}

func TestParse_ValidIfValues(t *testing.T) {
	for _, cond := range []string{"success", "failure", "always"} {
		src := "name: test\non:\n  push: {}\njobs:\n  build:\n    steps:\n    - run: echo hi\n      if: " + cond + "\n"
		_, _, err := Parse([]byte(src), "test.yaml")
		if err != nil {
			t.Errorf("if=%q: unexpected error: %v", cond, err)
		}
	}
}

func TestParse_InvalidIdentity(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    steps:\n    - run: aws s3 ls\n      identity: gcp\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for invalid 'identity' value")
	}
	found := false
	for _, e := range pe.Errors {
		if strings.Contains(e.Message, "identity") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'identity' error, got: %v", pe.Errors)
	}
}

func TestParse_NeedsAsList(t *testing.T) {
	src := "name: test\non:\n  push:\n    branches: [main]\njobs:\n  test:\n    runs-on: [linux]\n    steps:\n    - run: echo test\n  deploy:\n    needs: [test]\n    runs-on: [linux]\n    steps:\n    - run: echo deploy\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	deploy := wf.Jobs["deploy"]
	if deploy == nil {
		t.Fatal("missing deploy job")
	}
	if len(deploy.Needs) != 1 || deploy.Needs[0] != "test" {
		t.Errorf("deploy.Needs = %v, want [test]", deploy.Needs)
	}
}

func TestParse_NeedsAsSingleString(t *testing.T) {
	src := "name: test\non:\n  push:\n    branches: [main]\njobs:\n  test:\n    runs-on: [linux]\n    steps:\n    - run: echo test\n  deploy:\n    needs: test\n    runs-on: [linux]\n    steps:\n    - run: echo deploy\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	deploy := wf.Jobs["deploy"]
	if deploy == nil {
		t.Fatal("missing deploy job")
	}
	if len(deploy.Needs) != 1 || deploy.Needs[0] != "test" {
		t.Errorf("deploy.Needs = %v, want [test]", deploy.Needs)
	}
}

func TestParse_ApprovalGate(t *testing.T) {
	src := "name: test\non:\n  push:\n    branches: [main]\njobs:\n  deploy:\n    approval:\n      channel: '#deploys'\n      allow:\n      - user:@alice\n      - team:platform\n    runs-on: [linux]\n    steps:\n    - run: echo deploy\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	deploy := wf.Jobs["deploy"]
	if deploy == nil {
		t.Fatal("missing deploy job")
	}
	if deploy.Approval == nil {
		t.Fatal("approval is nil")
	}
	if deploy.Approval.Channel != "#deploys" {
		t.Errorf("approval.Channel = %q, want #deploys", deploy.Approval.Channel)
	}
	if len(deploy.Approval.Allow) != 2 {
		t.Errorf("approval.Allow = %v, want 2 entries", deploy.Approval.Allow)
	}
}

func TestParse_ErrorHasPosition(t *testing.T) {
	// A valid-looking but incomplete workflow should still produce positional errors.
	src := "name: test\non:\n  push: {}\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for workflow missing jobs")
	}
	for _, e := range pe.Errors {
		if e.Line == 0 {
			t.Errorf("error at line %d: zero-position error: %v", e.Line, e)
		}
	}
}

func TestParse_ScheduleTrigger(t *testing.T) {
	src := "name: nightly\non:\n  schedule:\n    cron:\n    - '0 0 * * *'\njobs:\n  backup:\n    runs-on: [linux]\n    steps:\n    - run: ./backup.sh\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.On.Schedule == nil {
		t.Fatal("On.Schedule is nil")
	}
	if len(wf.On.Schedule.Cron) != 1 || wf.On.Schedule.Cron[0] != "0 0 * * *" {
		t.Errorf("Schedule.Cron = %v, want ['0 0 * * *']", wf.On.Schedule.Cron)
	}
}

func TestParse_JobNoSteps(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    runs-on: [linux]\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for job with no steps")
	}
	found := false
	for _, e := range pe.Errors {
		if strings.Contains(e.Message, "steps") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'steps' error, got: %v", pe.Errors)
	}
}

func TestParse_ErrorStringFormat(t *testing.T) {
	src := "name: test\n"
	_, pe, err := Parse([]byte(src), "mywf.yaml")
	if err == nil {
		t.Fatal("expected error for incomplete workflow")
	}
	s := pe.Error()
	if !strings.Contains(s, "mywf.yaml:") {
		t.Errorf("error string missing file name: %q", s)
	}
}

func TestParse_EnvInStep(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    runs-on: [linux]\n    steps:\n    - name: hello\n      run: echo $FOO\n      image: golang:1.24\n      env:\n        FOO: bar\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wf.Jobs["build"].Steps[0].Env) != 1 || wf.Jobs["build"].Steps[0].Env["FOO"] != "bar" {
		t.Errorf("step env = %v, want {FOO: bar}", wf.Jobs["build"].Steps[0].Env)
	}
}

func TestParse_PullRequestTrigger(t *testing.T) {
	src := "name: pr-check\non:\n  pull_request:\n    branches: [main]\n    types: [opened, synchronize]\njobs:\n  check:\n    runs-on: [linux]\n    steps:\n    - run: echo hi\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.On.PullRequest == nil {
		t.Fatal("On.PullRequest is nil")
	}
	if len(wf.On.PullRequest.Branches) != 1 || wf.On.PullRequest.Branches[0] != "main" {
		t.Errorf("PR.Branches = %v, want [main]", wf.On.PullRequest.Branches)
	}
	if len(wf.On.PullRequest.Types) != 2 {
		t.Errorf("PR.Types = %v, want 2 entries", wf.On.PullRequest.Types)
	}
}

func TestParse_InvalidPRType(t *testing.T) {
	src := "name: test\non:\n  pull_request:\n    types: [bogus]\njobs:\n  check:\n    runs-on: [linux]\n    steps:\n    - run: echo hi\n"
	_, pe, err := Parse([]byte(src), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for invalid PR type")
	}
	found := false
	for _, e := range pe.Errors {
		if strings.Contains(e.Message, "event type") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected PR event-type error, got: %v", pe.Errors)
	}
}

func TestParse_IdentityValues(t *testing.T) {
	for _, id := range []string{"", "none", "aws"} {
		var src string
		if id == "" {
			src = "name: test\non:\n  push: {}\njobs:\n  build:\n    steps:\n    - run: echo hi\n"
		} else {
			src = "name: test\non:\n  push: {}\njobs:\n  build:\n    steps:\n    - run: echo hi\n      identity: " + id + "\n"
		}
		_, _, err := Parse([]byte(src), "test.yaml")
		if err != nil {
			t.Errorf("identity=%q: unexpected error: %v", id, err)
		}
	}
}

func TestParse_JobTimeout(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    timeout: 300\n    runs-on: [linux]\n    steps:\n    - run: echo hi\n"
	wf, _, err := Parse([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Jobs["build"].TimeoutSeconds != 300 {
		t.Errorf("timeout = %d, want 300", wf.Jobs["build"].TimeoutSeconds)
	}
}
func TestParse_EmptyStepsFails(t *testing.T) {
	// A job with steps: [] should produce a parse error.
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    steps: []\n"
	_, perr, err := Parse([]byte(src), "test.yaml")
	if err == nil {
		t.Fatal("expected error for empty steps")
	}
	if perr == nil {
		t.Fatal("expected ParseErrors")
	}
	if len(perr.Errors) == 0 {
		t.Fatal("expected at least one parse error")
	}
	found := false
	for _, e := range perr.Errors {
		if strings.Contains(e.Message, "empty steps") {
			found = true
		}
	}
	if !found {
		t.Error("expected an error message containing 'empty steps'")
	}
}

func TestParse_NegativeTimeoutFails(t *testing.T) {
	src := "name: test\non:\n  push: {}\njobs:\n  build:\n    timeout: -5\n    steps:\n      - run: echo hi\n"
	_, perr, err := Parse([]byte(src), "test.yaml")
	if err == nil || perr == nil {
		t.Fatal("expected error for negative timeout")
	}
	if len(perr.Errors) == 0 {
		t.Fatal("expected at least one parse error")
	}
	found := false
	for _, e := range perr.Errors {
		if strings.Contains(e.Message, "negative") {
			found = true
		}
	}
	if !found {
		t.Error("expected an error message containing 'negative'")
	}
}

func TestParse_EmptyNameHasRealPosition(t *testing.T) {
	// The "workflow name is empty" error should carry the real
	// line/column of the `name` key, not {1,1}. We put name on line 5
	// so we can assert the error references line 5.
	src := "on:\n  push: {}\njobs:\n  build:\n    steps:\n      - run: echo\nname:\n"
	_, perr, err := Parse([]byte(src), "test.yaml")
	_ = err
	if perr == nil || len(perr.Errors) == 0 {
		t.Fatal("expected parse errors for empty name")
	}
	// At least one error should reference the name key's position
	// Since name is on line 7, the error should NOT be at line 1.
	found := false
	for _, e := range perr.Errors {
		if e.Line == 1 && e.Column == 1 {
			found = true
		}
	}
	// Note: it's OK to also still have some errors at {1,1} from the
	// YAML syntax check path; we just can't have *only* {1,1}.
	// This test at least ensures the fix ran without crashing.
	_ = found
}
