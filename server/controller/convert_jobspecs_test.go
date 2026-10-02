package controller

import (
	"testing"

	"cicadoci.com/cicado/server/workflow"
)

// TestConvertJobSpecs_IncludesSteps verifies that convertJobSpecs maps each
// workflow job's steps into store.JobSpec.Steps so that CreateJobSpecs
// persists them into the job_steps table.
func TestConvertJobSpecs_IncludesSteps(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {
				Steps: []*workflow.Step{
					{Name: "install", Run: "apt-get install -y gcc"},
					{Name: "compile", Run: "make build"},
					{Name: "test", Run: "make test"},
				},
			},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}

	specs := convertJobSpecs(wf, dag, "https://github.com/org/repo.git", "refs/heads/main", "abc123", false)
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	spec := specs[0]
	if len(spec.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(spec.Steps))
	}
	for i, want := range []struct{ name, cmd string }{
		{"install", "apt-get install -y gcc"},
		{"compile", "make build"},
		{"test", "make test"},
	} {
		if got := spec.Steps[i].Name; got != want.name {
			t.Errorf("step %d name: got %q, want %q", i, got, want.name)
		}
		gotCmd := spec.Steps[i].Command
		if len(gotCmd) != 1 || gotCmd[0] != want.cmd {
			t.Errorf("step %d command: got %v, want %q", i, gotCmd, want.cmd)
		}
	}
	if spec.GitCloneURL != "https://github.com/org/repo.git" {
		t.Errorf("GitCloneURL = %q, want %q", spec.GitCloneURL, "https://github.com/org/repo.git")
	}
	if spec.GitRef != "refs/heads/main" {
		t.Errorf("GitRef = %q, want %q", spec.GitRef, "refs/heads/main")
	}
	if spec.GitSha != "abc123" {
		t.Errorf("GitSha = %q, want %q", spec.GitSha, "abc123")
	}
	// store import kept for potential DB read-back tests in a future PR.
}
