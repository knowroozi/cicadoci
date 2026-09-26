package policy

import (
	"testing"

	"cicadoci.com/cicado/server/workflow"
)

func newTestWF() *workflow.Workflow {
	return &workflow.Workflow{
		Name: "test",
		Jobs: map[string]*workflow.Job{
			"build": {
				RunsOn: []string{"linux"},
				Env:    map[string]string{"FOO": "bar", "DB_KEY": "secrets.DATABASE_URL"},
				Steps: []*workflow.Step{
					{Name: "compile", Env: map[string]string{"SECRET": "secrets.MY_SECRET", "PUBLIC": "hello"}, Run: "make"},
				},
			},
		},
	}
}

// TestForkPolicy_StripSecrets verifies secret-like env vars are removed.
func TestForkPolicy_StripSecrets(t *testing.T) {
	pol := &ForkPolicy{ForkSafeTag: "fork-safe"}
	wf := newTestWF()
	pol.Apply(wf)

	job := wf.Jobs["build"]
	if _, ok := job.Env["FOO"]; !ok {
		t.Fatal("FOO should survive scrubbing")
	}
	if _, ok := job.Env["DB_KEY"]; ok {
		t.Fatal("DB_KEY should be removed")
	}
	step := job.Steps[0]
	if _, ok := step.Env["PUBLIC"]; !ok {
		t.Fatal("PUBLIC should survive")
	}
	if _, ok := step.Env["SECRET"]; ok {
		t.Fatal("SECRET should be removed")
	}
}

// TestForkPolicy_AddTag verifies the fork-safe tag is added.
func TestForkPolicy_AddTag(t *testing.T) {
	pol := &ForkPolicy{ForkSafeTag: "fork-safe"}
	wf := newTestWF()
	pol.Apply(wf)

	tags := wf.Jobs["build"].RunsOn
	if !containsStr(tags, "fork-safe") {
		t.Fatalf("fork-safe not in %v", tags)
	}
	if !containsStr(tags, "linux") {
		t.Fatal("linux lost")
	}
}

// TestForkPolicy_Idempotent verifies applying twice is the same as once.
func TestForkPolicy_Idempotent(t *testing.T) {
	pol := &ForkPolicy{ForkSafeTag: "fork-safe"}
	wf := &workflow.Workflow{
		Name: "test",
		Jobs: map[string]*workflow.Job{
			"job": {RunsOn: []string{}, Steps: []*workflow.Step{{Name: "s", Run: "echo hi"}}},
		},
	}
	pol.Apply(wf)
	pol.Apply(wf)
	if n := len(wf.Jobs["job"].RunsOn); n != 1 {
		t.Fatalf("got %d tags, want 1", n)
	}
}

// TestForkPolicy_NoTag verifies empty ForkSafeTag doesn't add a tag.
func TestForkPolicy_NoTag(t *testing.T) {
	pol := &ForkPolicy{}
	wf := &workflow.Workflow{
		Name: "test",
		Jobs: map[string]*workflow.Job{
			"job": {RunsOn: []string{"linux"}, Steps: []*workflow.Step{{Name: "s", Run: "echo"}}},
		},
	}
	pol.Apply(wf)
	if n := len(wf.Jobs["job"].RunsOn); n != 1 {
		t.Fatalf("got %d tags, want 1", n)
	}
}

// TestForkPolicy_MultipleJobs exercises the policy over multi-job workflows.
func TestForkPolicy_MultipleJobs(t *testing.T) {
	pol := &ForkPolicy{ForkSafeTag: "fork-safe"}
	wf := &workflow.Workflow{
		Name: "multi",
		Jobs: map[string]*workflow.Job{
			"build":  {RunsOn: []string{"linux"}, Steps: []*workflow.Step{{Name: "c", Run: "make"}}},
			"deploy": {RunsOn: []string{"linux"}, Steps: []*workflow.Step{{Name: "d", Run: "deploy"}}},
		},
	}
	pol.Apply(wf)
	for name, job := range wf.Jobs {
		if !containsStr(job.RunsOn, "fork-safe") {
			t.Fatalf("job %q missing fork-safe", name)
		}
	}
}

// TestForkPolicy_StripsDeployIdentity pins the "no deploy identities"
// half of the fork-PR invariant. A fork PR that sets step.identity
// to a cloud provider (e.g. "aws") must have it cleared.
func TestForkPolicy_StripsDeployIdentity(t *testing.T) {
	wf := &workflow.Workflow{
		Jobs: workflow.Jobs{
			"deploy": {
				RunsOn: []string{"linux"},
				Steps: []*workflow.Step{
					{Name: "build", Run: "echo hi", Identity: "aws"},
					{Name: "test", Run: "go test ./...", Identity: "aws"},
					{Name: "ok", Run: "true", Identity: ""},
				},
			},
		},
	}
	p := DefaultForkPolicy()
	_ = p.Apply(wf)
	for _, s := range wf.Jobs["deploy"].Steps {
		if s.Identity != "" {
			t.Errorf("step %q: identity = %q, want empty (fork PRs must not assume a cloud identity)", s.Name, s.Identity)
		}
	}
}
