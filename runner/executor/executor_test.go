package executor

import "testing"

// TestInjectGitEnv tests that InjectGitEnv sets CICADO_GIT_CLONE_URL /
// CICADO_GIT_REF / CICADO_GIT_SHA on Job.Env when the corresponding string
// is non-empty, and preserves operator-supplied values.
func TestInjectGitEnv(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		cloneURL string
		ref      string
		sha      string
		want     map[string]string
	}{
		{
			name:     "all three set",
			env:      map[string]string{},
			cloneURL: "https://github.com/o/r.git",
			ref:      "refs/heads/main",
			sha:      "abc123",
			want: map[string]string{
				"CICADO_GIT_CLONE_URL": "https://github.com/o/r.git",
				"CICADO_GIT_REF":       "refs/heads/main",
				"CICADO_GIT_SHA":       "abc123",
			},
		},
		{
			name:     "operator-supplied clone URL preserved",
			env:      map[string]string{"CICADO_GIT_CLONE_URL": "git@ssh:4222/o/r.git"},
			cloneURL: "https://github.com/o/r.git",
			ref:      "refs/heads/main",
			sha:      "abc123",
			want: map[string]string{
				"CICADO_GIT_CLONE_URL": "git@ssh:4222/o/r.git", // preserved
				"CICADO_GIT_REF":       "refs/heads/main",
				"CICADO_GIT_SHA":       "abc123",
			},
		},
		{
			name: "all empty -- nothing injected",
			env:  map[string]string{},
			want: map[string]string{},
		},
		{
			name:     "nil env initialised",
			env:      nil,
			cloneURL: "https://github.com/o/r.git",
			ref:      "refs/heads/main",
			sha:      "abc123",
			want: map[string]string{
				"CICADO_GIT_CLONE_URL": "https://github.com/o/r.git",
				"CICADO_GIT_REF":       "refs/heads/main",
				"CICADO_GIT_SHA":       "abc123",
			},
		},
		{
			name: "partial -- only SHA",
			env:  map[string]string{},
			sha:  "abc123",
			want: map[string]string{
				"CICADO_GIT_SHA": "abc123",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var j Job
			j.Env = tc.env
			j.InjectGitEnv(tc.cloneURL, tc.ref, tc.sha)
			if len(j.Env) != len(tc.want) {
				t.Fatalf("env size: got %d, want %d (env=%v)", len(j.Env), len(tc.want), j.Env)
			}
			for k, v := range tc.want {
				if got, ok := j.Env[k]; !ok {
					t.Errorf("missing key %s after inject", k)
				} else if got != v {
					t.Errorf("key %s: got %q, want %q", k, got, v)
				}
			}
		})
	}
}
