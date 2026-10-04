package controller

import (
	"slices"
	"strings"
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
	// CI-148: a "checkout" step is prepended, so we expect 4 total.
	if len(spec.Steps) != 4 {
		t.Fatalf("got %d steps, want 4 (1 checkout + 3 user)", len(spec.Steps))
	}
	// First step is the synthetic checkout.
	if spec.Steps[0].Name != "checkout" {
		t.Errorf("step 0 name = %q, want \"checkout\"", spec.Steps[0].Name)
	}
	// User steps are at indices 1, 2, 3.
	userSteps := spec.Steps[1:]
	if len(userSteps) != 3 {
		t.Fatalf("got %d user steps, want 3", len(userSteps))
	}
	for i, want := range []struct {
		name string
		cmd  string
	}{
		{"install", "apt-get install -y gcc"},
		{"compile", "make build"},
		{"test", "make test"},
	} {
		if got := userSteps[i].Name; got != want.name {
			t.Errorf("user step %d name: got %q, want %q", i, got, want.name)
		}
		gotCmd := userSteps[i].Command
		// CIC-147: run: scripts are wrapped as /bin/sh -ec <script>, so the
		// user step's command is a 3-element argv.
		wantCmd := []string{"/bin/sh", "-ec", want.cmd}
		if !slices.Equal(gotCmd, wantCmd) {
			t.Errorf("user step %d command: got %v, want %v", i, gotCmd, wantCmd)
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

// TestConvertJobSpecs_MultiWordRunWrapped is CIC-147: a multi-word
// run: string must not become a single argv entry that the exec
// package looks up as a binary; it must be wrapped as
// /bin/sh -ec <script> so the whole multi-word script runs as one
// shell program.
func TestConvertJobSpecs_MultiWordRunWrapped(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {
				Steps: []*workflow.Step{
					{Name: "greet", Run: "echo \"hi there\" && echo good"},
				},
			},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}
	specs := convertJobSpecs(wf, dag, "", "", "", false)
	if len(specs) != 1 || len(specs[0].Steps) != 1 {
		t.Fatalf("expected 1 spec with 1 step, got %d specs: %+v", len(specs), specs)
	}
	got := specs[0].Steps[0].Command
	want := []string{"/bin/sh", "-ec", "echo \"hi there\" && echo good"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// TestCloneStepPrependedWhenGitContextPresent is CIC-148: when a
// workflow job gets a non-empty gitCloneURL, a synthetic "checkout"
// step is pre-pended to the job's Steps list so the runner clones
// the repo into the workspace before the first user step runs.
func TestCloneStepPrependedWhenGitContextPresent(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {
				Steps: []*workflow.Step{
					{Name: "build", Run: "make build"},
				},
			},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}
	const (
		url = "https://github.com/org/repo.git"
		ref = "refs/heads/main"
		sha = "abc123def456"
	)
	specs := convertJobSpecs(wf, dag, url, ref, sha, false)
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	steps := specs[0].Steps
	// First step should be the synthetic checkout.
	if len(steps) < 2 {
		t.Fatalf("got %d steps, want >= 2 (checkout + build)", len(steps))
	}
	chk := steps[0]
	if chk.Name != "checkout" {
		t.Errorf("first step name = %q, want \"checkout\"", chk.Name)
	}
	if len(chk.Command) != 3 || chk.Command[0] != "/bin/sh" || chk.Command[1] != "-ec" {
		t.Errorf("first step command = %v, want [\"/bin/sh\",\"-ec\",script]", chk.Command)
	}
	// The script must fetch the event's SHA from the clone URL.
	script := chk.Command[2]
	for _, sub := range []string{"git init", url, "git fetch --depth 1 origin '" + sha + "'", "git checkout -q FETCH_HEAD"} {
		if !strings.Contains(script, sub) {
			t.Errorf("clone script missing %q: got %q", sub, script)
		}
	}
	// The original user step must be intact at index 1 with the wrapped form.
	if steps[1].Name != "build" {
		t.Errorf("step 1 name = %q, want \"build\"", steps[1].Name)
	}
	if len(steps[1].Command) != 3 {
		t.Errorf("step 1 command = %v, want 3-element argv", steps[1].Command)
	}
	if steps[1].Command[2] != "make build" {
		t.Errorf("step 1 script = %q, want %q", steps[1].Command[2], "make build")
	}
}

// TestNoCloneStepWhenGitURLIsEmpty verifies that when gitCloneURL is
// empty (a fork / no-git job) no synthetic checkout step is prepended.
func TestNoCloneStepWhenGitURLIsEmpty(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {
				Steps: []*workflow.Step{
					{Name: "build", Run: "make build"},
				},
			},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}
	specs := convertJobSpecs(wf, dag, "", "", "", false)
	if len(specs) != 1 || len(specs[0].Steps) != 1 {
		t.Fatalf("expected exactly 1 step (no checkout), got %d", len(specs[0].Steps))
	}
	if specs[0].Steps[0].Name != "build" {
		t.Errorf("first step name = %q, want \"build\" (no checkout)", specs[0].Steps[0].Name)
	}
}

// TestBuildCloneScriptPrefersSha verifies the script shape: init, add the
// remote, fetch exactly the SHA (preferred over the ref), check out FETCH_HEAD.
func TestBuildCloneScriptPrefersSha(t *testing.T) {
	got := buildCloneScript("https://github.com/o/r.git", "refs/heads/main", "deadbeef")
	// CIC-148 (N1+B2): conditional git -c credential helper for authenticated
	// fetch, unconditional anon fallback.
	// N1: git -c credential.helper (not git config) — doesn't persist to
	// .git/config for later repo-controlled steps.
	if !strings.Contains(got, "git -c credential.helper=") {
		t.Errorf("N1: expected git -c credential.helper in script:\n%s", got)
	}
	if strings.Contains(got, "git config credential.helper") {
		t.Errorf("N1: git config credential.helper should not appear (N1 fix):\n%s", got)
	}
	// B2: conditional on CICADO_GITHUB_TOKEN being non-empty
	if !strings.Contains(got, "CICADO_GITHUB_TOKEN") {
		t.Errorf("B2: expected CICADO_GITHUB_TOKEN guard in script:\n%s", got)
	}
	// SHA preferred over ref.
	if !strings.Contains(got, "deadbeef") {
		t.Errorf("sha not used in fetch: %s", got)
	}
	if strings.Contains(got, "refs/heads/main") {
		t.Errorf("unexpected ref in script: %s", got)
	}
	// Checkout FETCH_HEAD.
	if !strings.Contains(got, "git checkout") {
		t.Errorf("missing checkout: %s", got)
	}
}

// TestBuildCloneScriptFallbacks verifies the fetch target falls back to the
// ref when no SHA is set, and to HEAD when neither is.
func TestBuildCloneScriptFallbacks(t *testing.T) {
	for _, tc := range []struct{ ref, sha, want string }{
		{"refs/pull/7/head", "", "git fetch --depth 1 origin 'refs/pull/7/head'\n"},
		{"", "", "git fetch --depth 1 origin 'HEAD'\n"},
	} {
		got := buildCloneScript("https://github.com/o/r.git", tc.ref, tc.sha)
		if !strings.Contains(got, tc.want) {
			t.Errorf("ref=%q sha=%q: script missing %q:\n%s", tc.ref, tc.sha, tc.want, got)
		}
	}
}

// TestBuildCloneScriptQuotesArguments verifies a single quote in the URL
// cannot break out of its shell quoting.
func TestBuildCloneScriptQuotesArguments(t *testing.T) {
	got := buildCloneScript("https://x/a'; rm -rf /; '.git", "", "")
	if !strings.Contains(got, `'https://x/a'\''; rm -rf /; '\''.git'`) {
		t.Errorf("URL not safely quoted:\n%s", got)
	}
}

// TestConvertJobSpecs_NonForkInjectsGitCloneToken is the CIC-148 controller
// integration test: a non-fork job with a git context gets github_clone_token
// in its SecretRefs and the checkout step has a CICADO_GITHUB_TOKEN env var
// that the runner resolves via GetJobSecrets.
func TestConvertJobSpecs_NonForkInjectsGitCloneToken(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {
				Steps: []*workflow.Step{
					{Name: "test", Run: "go test ./..."},
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
	// The checkout step is prepended; total steps = 1 checkout + 1 user = 2.
	if len(spec.Steps) < 1 {
		t.Fatalf("no steps in spec")
	}
	checkout := spec.Steps[0]
	if checkout.Name != "checkout" {
		t.Fatalf("step 0 name = %q, want 'checkout'", checkout.Name)
	}
	// Non-fork: checkout step has CICADO_GITHUB_TOKEN in its env.
	if checkout.Env == nil {
		t.Error("non-fork checkout step should have Env with CICADO_GITHUB_TOKEN")
	}
	if !strings.Contains(string(checkout.Env), "github_clone_token") {
		t.Errorf("checkout.Env = %s, want it to reference github_clone_token", checkout.Env)
	}
	// github_clone_token must be in SecretRefs.
	var found bool
	for _, ref := range spec.SecretRefs {
		if ref == "github_clone_token" {
			found = true
		}
	}
	if !found {
		t.Errorf("SecretRefs = %v, want github_clone_token among them", spec.SecretRefs)
	}
}

// TestConvertJobSpecs_ForkRun_NoGithubCloneToken verifies the security-critical
// case: a fork PR run must NOT get github_clone_token in its SecretRefs or in
// TestConvertJobSpecs_ForkRun_GetsGithubCloneToken verifies B1: a fork PR run
// gets github_clone_token in SecretRefs for the contents:read minter but no
// other user secrets (CIC-27 defense-in-depth).
func TestConvertJobSpecs_ForkRun_GetsGithubCloneToken(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {
				Env: map[string]string{"MY_SECRET": "${{ secrets.my_app_api_key }}"},
				Steps: []*workflow.Step{
					{Name: "test", Run: "go test ./..."},
				},
			},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}
	specs := convertJobSpecs(wf, dag, "https://github.com/org/repo.git", "refs/heads/main", "abc123", true)
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	spec := specs[0]
	// B1: the checkout step IS present with CICADO_GITHUB_TOKEN in Env.
	checkout := spec.Steps[0]
	if checkout.Env == nil {
		t.Error("B1: fork PR checkout step should have Env with CICADO_GITHUB_TOKEN")
	}
	if !strings.Contains(string(checkout.Env), "github_clone_token") {
		t.Errorf("B1: checkout.Env = %s, want it to reference github_clone_token", checkout.Env)
	}
	// B1: github_clone_token must be in SecretRefs (for the minter).
	var found bool
	for _, ref := range spec.SecretRefs {
		if ref == "github_clone_token" {
			found = true
		}
	}
	if !found {
		t.Error("B1: SecretRefs must contain github_clone_token")
	}
	// B1 defense-in-depth: no other user secrets for fork runs.
	for _, ref := range spec.SecretRefs {
		if ref != "github_clone_token" {
			t.Errorf("B1: fork run SecretRefs contains unexpected secret %q", ref)
		}
	}
}
