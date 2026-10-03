package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildCloneScriptChecksOutNonDefaultRef runs the generated checkout
// script against a local two-branch repo and asserts it lands on a commit
// that is NOT on the default branch, for a branch ref, a PR-style ref, and a
// bare SHA.  A `git clone --depth 1` + `git checkout <ref>` script fails all
// three because the shallow clone is single-branch.
func TestBuildCloneScriptChecksOutNonDefaultRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(origin, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git(origin, "init", "-q", "-b", "main", ".")
	write("a.txt")
	git(origin, "add", "a.txt")
	git(origin, "commit", "-qm", "main")
	git(origin, "checkout", "-q", "-b", "feature")
	write("b.txt")
	git(origin, "add", "b.txt")
	git(origin, "commit", "-qm", "feature")
	featureSHA := git(origin, "rev-parse", "HEAD")
	git(origin, "update-ref", "refs/pull/7/head", featureSHA)
	git(origin, "checkout", "-q", "main")

	for _, tc := range []struct{ name, ref, sha string }{
		{"branch ref", "refs/heads/feature", ""},
		{"pr ref", "refs/pull/7/head", ""},
		{"sha", "refs/heads/feature", featureSHA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-ec", buildCloneScript("file://"+origin, tc.ref, tc.sha))
			cmd.Dir = work
			cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("checkout script failed: %v\n%s", err, out)
			}
			if got := git(work, "rev-parse", "HEAD"); got != featureSHA {
				t.Errorf("HEAD = %s, want %s", got, featureSHA)
			}
			if _, err := os.Stat(filepath.Join(work, "b.txt")); err != nil {
				t.Errorf("feature-only file missing: %v", err)
			}
		})
	}
}
