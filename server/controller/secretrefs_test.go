package controller

import (
	"reflect"
	"testing"

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
			got := secretRefsFor(tc.job, false)
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
	if got := secretRefsFor(job, true); got != nil {
		t.Errorf("isFork=true returned %v, want nil (defense-in-depth)", got)
	}
	// Sanity: the same job with isFork=false returns the refs as before.
	if got := secretRefsFor(job, false); len(got) != 2 {
		t.Errorf("isFork=false returned %v, want 2 refs", got)
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
	got := secretRefsFor(job, false)
	want := []string{"MY_KEY_ONE", "MY_KEY_TWO"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secretRefsFor() = %v, want %v (each step's env evaluated independently)", got, want)
	}
}
