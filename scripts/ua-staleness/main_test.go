package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newRepo makes an isolated git repo in a temp dir with one initial commit and
// .ua/meta.json recorded against that commit. It returns the repo root.
func newRepo(t *testing.T, recordedMeta string) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	// Per-repo identity so commits work without touching the caller's global config.
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "CICaDo Test")
	run("config", "commit.gpgsign", "false")

	if err := os.MkdirAll(filepath.Join(root, ".ua"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".ua", "meta.json"), []byte(recordedMeta), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "initial")
	t.Cleanup(func() {})
	return root
}

func head(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return string(out[:40])
}

// commitMore stages a fresh project file and commits, returning the new project change.
func touchProject(t *testing.T, root string, changedFile string, content string) {
	t.Helper()
	f := filepath.Join(root, changedFile)
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "change "+changedFile)
}

func TestNoGraph(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ua"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), root)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != "no-graph" {
		t.Fatalf("status = %q, want no-graph", res.Status)
	}
}

func TestEmptyHashIsNoGraph(t *testing.T) {
	root := newRepo(t, `{"gitCommitHash":""}`)
	res, err := Check(context.Background(), root)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != "no-graph" {
		t.Fatalf("status = %q, want no-graph for empty hash", res.Status)
	}
}

// TestCurrent proves the .ua/ exclusion for real: it commits a change under .ua/ AFTER the
// recorded hash, so `git diff H..HEAD` genuinely lists `.ua/meta.json`, and Check must still
// report "current" because the filter drops .ua/ paths. The precondition assertion makes the
// test fail if the tree has no diff (which would make the .ua/ claim vacuous).
func TestCurrent(t *testing.T) {
	root := newRepo(t, `{}`)
	h0 := head(t, root)
	// touchProject writes .ua/meta.json (committing via git add -A). That commit is a
	// real tree change that git diff h0..HEAD will list as .ua/meta.json.
	touchProject(t, root, ".ua/meta.json", `{"gitCommitHash":"`+h0+`"}`)
	// Precondition: the diff really must list .ua/meta.json, else the assertion below
	// proves nothing about the filter. (A new untracked file would NOT show, so we must
	// use a committed change — which is what touchProject produces.)
	out, err := exec.CommandContext(context.Background(), "git", "-C", root, "diff", "--name-only", h0+"..HEAD").Output()
	if err != nil {
		t.Fatalf("git diff precondition: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != ".ua/meta.json" {
		t.Fatalf("precondition: git diff %s..HEAD = %q, want .ua/meta.json (test would be vacuous without it)", h0, got)
	}
	// With the .ua/ filter, a committed change under .ua/ alone must NOT flip to stale.
	res, err := Check(context.Background(), root)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != "current" {
		t.Fatalf("status = %q, want current (.ua/ change must be filtered out); stale=%v msg=%q", res.Status, res.StaleFiles, res.Message)
	}
}

// TestCurrentWithTopLevelChange is the positive control: after the same setup, a committed
// top-level change must flip to "stale" with the top-level path in StaleFiles and no .ua/
// path leaking through the filter. Pairs with TestCurrent so a filter that returned
// "current" for everything could not pass both.
func TestCurrentWithTopLevelChange(t *testing.T) {
	root := newRepo(t, `{}`)
	h0 := head(t, root)
	// Record h0 in .ua/meta.json (a .ua/ change the filter should drop).
	touchProject(t, root, ".ua/meta.json", `{"gitCommitHash":"`+h0+`"}`)
	// Now add and commit a top-level project change; it must surface as stale.
	touchProject(t, root, "top-level.txt", "a top-level change\n")
	res, err := Check(context.Background(), root)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != "stale" {
		t.Fatalf("status = %q, want stale (top-level change must surface); stale=%v msg=%q", res.Status, res.StaleFiles, res.Message)
	}
	sawTop := false
	for _, f := range res.StaleFiles {
		if f == ".ua/meta.json" || strings.HasPrefix(f, ".ua/") {
			t.Errorf("StaleFiles=%v must not include any .ua/ path (filter broken)", res.StaleFiles)
		}
		if f == "top-level.txt" {
			sawTop = true
		}
	}
	if !sawTop {
		t.Errorf("StaleFiles=%v must include top-level.txt (positive control failed)", res.StaleFiles)
	}
}

func TestStaleProjectChange(t *testing.T) {
	root := newRepo(t, `{}`)
	h := head(t, root)
	os.WriteFile(filepath.Join(root, ".ua", "meta.json"), []byte(`{"gitCommitHash":"`+h+`"}`), 0o644)
	touchProject(t, root, "changelog.txt", "changed since graph")
	res, err := Check(context.Background(), root)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != "stale" {
		t.Fatalf("status = %q, want stale", res.Status)
	}
	if !reflect.DeepEqual(res.StaleFiles, []string{"changelog.txt"}) {
		t.Fatalf("stale files = %v, want [changelog.txt]", res.StaleFiles)
	}
}

// TestUnreachableIsGraceful is the CIC-86 core: an unreachable recorded commit (a
// squash-merge artifact) must be reported as a clean `unreachable` result, never a raw
// "bad revision" git error.
func TestUnreachableIsGraceful(t *testing.T) {
	root := newRepo(t, `{}`)
	// Record a commit hash this repo does not contain (all-zero git object id).
	dead := "0000000000000000000000000000000000000000"
	if err := os.WriteFile(filepath.Join(root, ".ua", "meta.json"), []byte(`{"gitCommitHash":"`+dead+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), root)
	if err != nil {
		t.Fatalf("Check returned an error for an unreachable commit — that is the bug CIC-86 fixes: %v", err)
	}
	if res.Status != "unreachable" {
		t.Fatalf("status = %q, want unreachable", res.Status)
	}
	if res.Hash != dead {
		t.Fatalf("hash = %q, want %q", res.Hash, dead)
	}
}

func TestCheckTimeout(t *testing.T) {
	// A short deadline must not leak a hung git; the call should complete fast.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ua"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No git repo here; check still resolves "no-graph" without invoking git.
	res, err := Check(ctx, root)
	if err != nil {
		t.Fatalf("Check under no git repo: %v", err)
	}
	if res.Status != "no-graph" {
		t.Fatalf("status = %q, want no-graph", res.Status)
	}
}
