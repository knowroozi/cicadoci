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

// TestForkPolicy_StripsTemplateFormSecret is the regression for Dwight's #91
// security blocker 1: the fork policy used to only strip values that start
// exactly with "secrets." — a fork workflow could smuggle a real secret
// through with the template form ("${{ secrets.DEPLOY }}") or a leading
// space (" secrets.DEPLOY"). After the fix, scrubSecrets uses
// secretexpr.IsRef, which recognises both forms plus leading whitespace.
func TestForkPolicy_StripsTemplateFormSecret(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "test",
		Jobs: map[string]*workflow.Job{
			"build": {
				RunsOn: []string{"linux"},
				Env: map[string]string{
					"A": "secrets.DEPLOY",               // bare
					"B": "${{ secrets.DEPLOY }}",        // template
					"C": "  secrets.DEPLOY",             // leading whitespace
					"D": "NOT_A_SECRET",                 // not a ref
					"E": "prefix secrets.SUFFIX suffix", // embedded fragment
				},
				Steps: []*workflow.Step{
					{
						Name: "step",
						Env: map[string]string{
							"F": "${{ secrets.GITHUB_TOKEN }}", // template in step
							"Z": "plain-value",
						},
						Run: "echo",
					},
				},
			},
		},
	}
	p := DefaultForkPolicy()
	p.Apply(wf)

	// All A, B, C (job level) and F (step level) must be gone.
	for _, key := range []string{"A", "B", "C", "Z"} {
		if _, ok := wf.Jobs["build"].Env[key]; ok {
			t.Errorf("job Env key %q survived scrub, want removed", key)
		}
	}
	if _, ok := wf.Jobs["build"].Env["D"]; !ok {
		t.Error("D (non-secret) was incorrectly removed")
	}
	if _, ok := wf.Jobs["build"].Env["E"]; !ok {
		t.Error("E (embedded fragment, not a ref) was incorrectly removed")
	}
	s := wf.Jobs["build"].Steps[0]
	if _, ok := s.Env["F"]; ok {
		t.Error("step Env key F (template form secret) survived scrub")
	}
	if _, ok := s.Env["Z"]; !ok {
		t.Error("step Env key Z (non-secret) was incorrectly removed")
	}
}

// TestForkPolicy_StripEmbeddedSecrets verifies that an embedded secret
// reference (CIC-135), e.g. "Bearer ${{ secrets.T }}", is scrubbed from a
// fork-PR job so the untrusted fork executor never receives it. The value is
// built by string concatenation so this source file never keeps '$' adjacent to
// '{{', which the shell/expansion would otherwise expand.
func TestForkPolicy_StripEmbeddedSecrets(t *testing.T) {
	pol := &ForkPolicy{ForkSafeTag: "fork-safe"}
	wf := &workflow.Workflow{
		Name: "test",
		Jobs: map[string]*workflow.Job{
			"build": {
				RunsOn: []string{"linux"},
				Env: map[string]string{
					"AUTH":  "Bearer " + "${{ secrets.TOKEN }}",
					"URL":   "postgres://u:" + "${{ secrets.PW }}" + "@db:5432",
					"PLAIN": "not-a-reference",
				},
				Steps: []*workflow.Step{},
			},
		},
	}
	pol.Apply(wf)

	job := wf.Jobs["build"]
	if _, ok := job.Env["AUTH"]; ok {
		t.Fatal("AUTH embedded secret ref should be scrubbed")
	}
	if _, ok := job.Env["URL"]; ok {
		t.Fatal("URL embedded secret ref should be scrubbed")
	}
	if _, ok := job.Env["PLAIN"]; !ok {
		t.Fatal("PLAIN should survive scrubbing")
	}
}
