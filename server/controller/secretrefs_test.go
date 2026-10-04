package controller

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"cicadoci.com/cicado/internal/secretexpr"
	"cicadoci.com/cicado/server/workflow"
)

// TestSecretRefsFor extracts the distinct, sorted set of secret NAMES that a
// job references across its job-level and per-step env, and confirms that a job
// with references carries them while a plain job carries none. This is the
// server-side half of CIC-79: the runner then fetches VALUES for these names
// via the lease-scoped GetJobSecrets RPC.
func TestSecretRefsFor(t *testing.T) {
	cases := []struct {
		name string
		job  *workflow.Job
		want []string
	}{
		{
			name: "no references",
			job:  &workflow.Job{},
			want: nil,
		},
		{
			name: "job-level bare reference",
			job:  &workflow.Job{Env: map[string]string{"DEPLOY_ROLE": "secrets.PROD_DEPLOY_ROLE"}},
			want: []string{"PROD_DEPLOY_ROLE"},
		},
		{
			name: "step-level template reference",
			job:  &workflow.Job{Steps: []*workflow.Step{{Env: map[string]string{"API_KEY": "${{ secrets.GITHUB_TOKEN }}"}}}},
			want: []string{"GITHUB_TOKEN"},
		},
		{
			name: "deduplicated across job and steps",
			job: &workflow.Job{
				Env: map[string]string{"A": "secrets.SHARED"},
				Steps: []*workflow.Step{
					{Env: map[string]string{"X": "${{ secrets.SHARED }}"}}, // same name, different form
					{Env: map[string]string{"Y": "secrets.ONLY_STEP"}},
				},
			},
			want: []string{"ONLY_STEP", "SHARED"},
		},
		{
			name: "plain env values are ignored",
			job:  &workflow.Job{Env: map[string]string{"LOG_LEVEL": "info", "URL": "https://example.com"}},
			want: nil,
		},
		{
			name: "embedded fragment is not a reference",
			job:  &workflow.Job{Steps: []*workflow.Step{{Env: map[string]string{"PATH": "prefix secrets.SUFFIX suffix"}}}},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := secretRefsFor(tc.job, false, "")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("secretRefsFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSecretRefsFor_ForkJobReturnsNil is the defense-in-depth blocker fix
// (Dwight #91 blocker 1b): even if a fork workflow's env text contains a
// secret reference, secretRefsFor with isFork=true returns nil, so the
// runner never asks GetJobSecrets for a secret it is not authorized for.
func TestSecretRefsFor_ForkJobReturnsNil(t *testing.T) {
	// Job with a template-form ref and a bare-form ref; both should be ignored
	// for isFork=true.
	job := &workflow.Job{
		Env: map[string]string{"AUTH": "secrets.PROD_AUTH"},
		Steps: []*workflow.Step{
			{Env: map[string]string{"TOKEN": "${{ secrets.DEPLOY }}"}},
			{Env: map[string]string{"PLAIN": "not-a-ref"}},
		},
	}
	if got := secretRefsFor(job, true, "https://github.com/o/r.git"); got != nil {
		t.Errorf("isFork=true returned %v, want nil (defense-in-depth)", got)
	}
	// Sanity: the same job with isFork=false returns the refs as before.
	if got := secretRefsFor(job, false, "https://github.com/o/r.git"); len(got) != 3 {
		t.Errorf("isFork=false returned %v, want 3 refs (DEPLOY, PROD_AUTH, github_clone_token)", got)
	}
}

// TestSecretRefsFor_MultipleStepsSharedKey is the regression for blocker 2
// (Dwight #91): two steps that both use the same env key T but with different
// secret refs must not mask each other. The old code merged by key and only
// surfaced the last step's ref; the new per-step extraction surfaces both.
func TestSecretRefsFor_MultipleStepsSharedKey(t *testing.T) {
	job := &workflow.Job{
		Steps: []*workflow.Step{
			{Env: map[string]string{"T": "secrets.MY_KEY_ONE"}},
			{Env: map[string]string{"T": "secrets.MY_KEY_TWO"}},
		},
	}
	got := secretRefsFor(job, false, "https://github.com/o/r.git")
	want := []string{"MY_KEY_ONE", "MY_KEY_TWO", "github_clone_token"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secretRefsFor() = %v, want %v (each step's env evaluated independently)", got, want)
	}
}

// TestSecretRefsFor_GitCloneToken is the CIC-148 test: when gitCloneURL is
// non-empty and isFork is false, github_clone_token must appear in the
// secret_refs list.  This ensures the controller adds the per-repo GitHub
// token to the runner's secret-fetch scope for non-fork runs that have
// a git context.
func TestSecretRefsFor_GitCloneToken(t *testing.T) {
	job := &workflow.Job{
		Env: map[string]string{"API_KEY": "secrets.DEPLOY_TOKEN"},
		Steps: []*workflow.Step{
			{Env: map[string]string{"X": "value"}},
		},
	}
	// Non-fork with git context: github_clone_token IS present alongside user refs.
	got := secretRefsFor(job, false, "https://github.com/org/repo.git")
	want := []string{"DEPLOY_TOKEN", "github_clone_token"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secretRefsFor(false, gitCloneURL) = %v, want %v", got, want)
	}
}

// TestSecretRefsFor_ForkJobNoGitCloneToken is the CIC-148 fork-protection
// assertion: when isFork is true, github_clone_token is NEVER in the list,
// even when gitCloneURL is non-empty.  This is the mutation-proven no-token-
// for-fork case: removing the isFork guard in secretRefsFor makes this test
// FAIL.
func TestSecretRefsFor_ForkJobNoGitCloneToken(t *testing.T) {
	job := &workflow.Job{
		Env: map[string]string{"API_KEY": "secrets.DEPLOY_TOKEN"},
		Steps: []*workflow.Step{
			{Env: map[string]string{"X": "value"}},
		},
	}
	// Fork with git context: returns nil unconditionally.
	got := secretRefsFor(job, true, "https://github.com/org/repo.git")
	if got != nil {
		t.Errorf("secretRefsFor(true, gitCloneURL) = %v, want nil (no secrets for fork runs)", got)
	}
}

// TestCloneStepSpec_NonForkHasCredentialEnv is the CIC-148 assertion that a
// non-fork job with a git context gets a CICADO_GITHUB_TOKEN env var in its
// checkout step pointing at the github_clone_token secret.  This is resolved
// by the runner's fetchJobSecrets at runtime.
func TestCloneStepSpec_NonForkHasCredentialEnv(t *testing.T) {
	spec := cloneStepSpec("https://github.com/org/repo.git", "refs/heads/main", "abc123", false)
	// Non-fork: Env is set with GitHub token secret reference.
	if spec.Env == nil {
		t.Error("cloneStepSpec with isFork=false should have Env set")
		return
	}
	var envMap map[string]string
	if err := json.Unmarshal(spec.Env, &envMap); err != nil {
		t.Fatalf("spec.Env is not valid JSON: %v", err)
	}
	got, ok := envMap["CICADO_GITHUB_TOKEN"]
	if !ok || got == "" {
		t.Errorf("CICADO_GITHUB_TOKEN not found in spec.Env, got: %v", envMap)
		return
	}
	if !strings.Contains(got, "github_clone_token") {
		t.Errorf("CICADO_GITHUB_TOKEN = %q, want it to reference secrets.github_clone_token", got)
	}
}

// TestCloneStepSpec_ForkHasNoCredentialEnv is the CIC-148 assertion that a
// fork job gets NO credential env in its checkout step.  This is the
// security-critical case: fork PR runs must not have any token.
func TestCloneStepSpec_ForkHasNoCredentialEnv(t *testing.T) {
	spec := cloneStepSpec("https://github.com/org/repo.git", "refs/heads/main", "abc123", true)
	if spec.Env != nil {
		t.Errorf("cloneStepSpec with isFork=true should have no Env, got: %s", spec.Env)
	}
}

// TestBuildCloneScript_HasCredentialHelper is a minimal smoke test that the
// generated script contains the git credential.helper line that reads
// CICADO_GITHUB_TOKEN from the step's env.
func TestBuildCloneScript_HasCredentialHelper(t *testing.T) {
	script := buildCloneScript("https://github.com/org/repo.git", "refs/heads/main", "abc123")
	if !strings.Contains(script, "credential.helper") {
		t.Error("buildCloneScript does not contain 'credential.helper'")
	}
	if !strings.Contains(script, "CICADO_GITHUB_TOKEN") {
		t.Error("buildCloneScript does not reference CICADO_GITHUB_TOKEN")
	}
}

func TestCloneStepSpec_NonForkEnvResolvesViaSecretExpr(t *testing.T) {
	// B1 regression test (Dwight #123): the env map emitted by
	// cloneStepSpec must pass through secretexpr.Resolve such that
	// github_clone_token resolves to the delivered value in CICADO_GITHUB_TOKEN.
	spec := cloneStepSpec("https://github.com/o/r.git", "refs/heads/main", "deadbeef", false)
	if spec.Env == nil {
		t.Fatal("expected cloneStepSpec to emit non-nil Env for non-fork job")
	}
	var rawEnv map[string]string
	if err := json.Unmarshal(spec.Env, &rawEnv); err != nil {
		t.Fatalf("clone step Env is invalid JSON: %v\n%s", err, spec.Env)
	}
	const tok = "ghs_sentinel_clone_token"
	resolved := secretexpr.Resolve(rawEnv, map[string]string{"github_clone_token": tok})
	if got := resolved["CICADO_GITHUB_TOKEN"]; got != tok {
		t.Errorf("B1: CICADO_GITHUB_TOKEN = %q, want %q", got, tok)
	}
	// secretRefsFor appends github_clone_token for non-fork jobs with a GitContext
	// so the runner fetches it and resolves it into CICADO_GITHUB_TOKEN.
	j := &workflow.Job{
		Env: map[string]string{"MY_SECRET": "{{ secrets.my_app_api_key }}"},
	}
	refs := secretRefsFor(j, false, "https://github.com/o/r.git")
	var found bool
	for _, r := range refs {
		if r == githubCloneTokenSecretName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("secretRefsFor must append %s for non-fork job with GitContext", githubCloneTokenSecretName)
	}
}
