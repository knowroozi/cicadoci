// Command ua-staleness reports whether the /understand knowledge graph recorded in
// .ua/meta.json is current with the checked-out tree, and degrades *gracefully* instead
// of surfacing a raw git error when the recorded commit is unreachable.
//
// Background (CIC-86): the repo squash-merges every PR. A graph refreshed on a feature
// branch records that branch's HEAD; after the squash-merge deletes the branch ref, that
// commit is no longer reachable from a fresh clone of master, and the staleness check
// `git diff --name-only <graphCommit>..HEAD` dies with "fatal: bad revision". CLAUDE.md
// and docs/decisions/CIC-77-knowledge-graph-currency.md say the staleness signal is a
// "refresh the graph" warning, NOT a hard failure -- so a bad revision must be reported as
// a stale-with-reason result, never a bare git error.
//
// Outcomes (all exit 0 unless git itself is unusable):
//   - current:     recorded commit is reachable and the project diff is empty.
//   - stale:       recorded commit is reachable but project files changed since it.
//   - unreachable: .ua/meta.json records a commit this clone cannot see (a squash-merge
//     artifact). Treat as stale and run a full /understand; do not error out.
//   - no-graph:    no .ua/meta.json yet (fresh checkout). Run /understand once.
//
// Usage: ua-staleness [repoRoot] [--strict] [--help]
// (repoRoot defaults to `git rev-parse --show-toplevel`. Set CI=1 or --strict to exit
// non-zero on a stale/unreachable result.)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// meta is the subset of .ua/meta.json this check reads.
type meta struct {
	GitCommitHash string `json:"gitCommitHash"`
}

// Result classifies the graph relative to the current tree.
type Result struct {
	Status     string // "current" | "stale" | "unreachable" | "no-graph"
	Hash       string // recorded gitCommitHash ("" when none)
	Message    string // one-line human explanation
	StaleFiles []string
}

// metaPath returns the .ua/meta.json path under the repo root.
func metaPath(root string) string { return filepath.Join(root, ".ua", "meta.json") }

// git runs "git args..." with CWD at root and returns its stdout; err is non-nil on any
// non-zero exit. cmd.Dir (NOT `git -C`) is used deliberately: the staleness diff passes a
// "." pathspec, and on some git versions `git -C` resolves that pathspec against git's
// original working directory, which would silently break the .ua/ exclusion. A context
// bounds the call so a hung git cannot hang the check.
func git(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	return string(out), err
}

// commitReachable reports whether <hash> resolves to a commit in this checkout. The
// "not a commit" case (expected after a squash-merge of a feature branch) is returned as
// (false, nil); only an actual git/exec failure is an error.
func commitReachable(ctx context.Context, root, hash string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", hash+"^{commit}")
	cmd.Dir = root
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// git ran but the object is absent: a clean "unreachable" for this clone.
		return false, nil
	}
	return false, fmt.Errorf("git cat-file: %w", err)
}

// repoRoot resolves the git toplevel unless root is already given.
func repoRoot(ctx context.Context, root string) (string, error) {
	if root != "" {
		return root, nil
	}
	out, err := git(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Check is the pure, testable heart of the check: read the recorded commit, verify it is
// reachable, and diff it against HEAD for changes under the project tree (excluding the
// .ua/ artifacts the refresh itself commits). It never returns a raw git error for the
// "commit not reachable" case -- that becomes a clean `unreachable` result. Only genuine
// operational problems (git unusable, unreadable meta.json) return an error.
//
// The .ua/ exclusion is applied in Go after the diff, not as a `':!.ua'` git pathspec, so
// it is independent of git's -C/pathspec-resolution quirks (mirrors the "ignoring .ua/"
// intent in CLAUDE.md).
func Check(ctx context.Context, root string) (Result, error) {
	metaFile := metaPath(root)
	raw, err := os.ReadFile(metaFile)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{Status: "no-graph", Message: "no .ua/meta.json yet -- run a full /understand to create the baseline"}, nil
		}
		return Result{}, fmt.Errorf("read %s: %w", metaFile, err)
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Result{}, fmt.Errorf("parse %s: %w", metaFile, err)
	}
	hash := strings.TrimSpace(m.GitCommitHash)
	if hash == "" {
		return Result{Status: "no-graph", Message: ".ua/meta.json has no gitCommitHash -- run a full /understand"}, nil
	}

	// Graceful fallback: a squash-merged feature-branch commit is not reachable from a fresh
	// clone of master. Detect it up front so the diff never dies on "bad revision".
	if reachable, rerr := commitReachable(ctx, root, hash); rerr != nil {
		return Result{}, rerr
	} else if !reachable {
		return Result{
			Status:  "unreachable",
			Hash:    hash,
			Message: fmt.Sprintf("graph records %s but that commit is not reachable from this checkout (expected after a squash-merge of its feature branch); run a full /understand to refresh", short(hash)),
		}, nil
	}

	out, err := git(ctx, root, "diff", "--name-only", hash+"..HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("git diff %s..HEAD: %w", short(hash), err)
	}
	var stale []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		// Exclude the artifacts the refresh itself commits so they never re-flag staleness.
		if name == ".ua" || strings.HasPrefix(name, ".ua/") {
			continue
		}
		seen[name] = true
		stale = append(stale, name)
	}
	sort.Strings(stale)
	if len(stale) == 0 {
		return Result{Status: "current", Hash: hash, Message: fmt.Sprintf("graph (%s) is current with the tree", short(hash))}, nil
	}
	return Result{
		Status:     "stale",
		Hash:       hash,
		Message:    fmt.Sprintf("graph (%s) is stale: %d project file(s) changed since the recorded commit", short(hash), len(stale)),
		StaleFiles: stale,
	}, nil
}

// short renders a commit hash at a readable width.
func short(h string) string {
	if len(h) >= 7 {
		return h[:7]
	}
	return h
}

func main() {
	ctx := context.Background()
	rootArg := ""
	strict := os.Getenv("CI") == "1"
	for _, a := range os.Args[1:] {
		switch a {
		case "--strict":
			strict = true
		case "--help", "-h":
			usage()
			return
		default:
			rootArg = a
		}
	}

	root, err := repoRoot(ctx, rootArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ua-staleness: cannot resolve repo root: %v\n", err)
		os.Exit(2)
	}
	res, err := Check(ctx, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ua-staleness: %v\n", err)
		os.Exit(2)
	}
	if res.Status == "current" {
		fmt.Println(res.Message)
		return
	}
	// Warn, not fail, by default: stale/unreachable are "refresh the graph" signals.
	fmt.Fprintf(os.Stderr, "ua-staleness: %s\n", res.Message)
	for _, f := range res.StaleFiles {
		fmt.Fprintln(os.Stderr, "  changed: "+f)
	}
	if strict && (res.Status == "stale" || res.Status == "unreachable") {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, "ua-staleness [repoRoot] [--strict] [--help]\n\n"+
		"Reports whether the /understand graph in .ua/meta.json is current. Degrades gracefully\n"+
		"(no raw git error) when the recorded commit is unreachable. With --strict or CI=1 it\n"+
		"exits non-zero on a stale/unreachable result.\n")
}
