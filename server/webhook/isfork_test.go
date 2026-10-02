package webhook

import (
	"encoding/json"
	"testing"
)

// ── IsFork() with real GitHub JSON fixtures ──────────────────────────
//
// Each JSON string is a subset of a real GitHub pull_request webhook
// payload. Fork detection uses ONLY the full_name comparison
// (head.repo.full_name vs repository.full_name) plus fail-closed
// on nil/empty.
//
// We do NOT use pull_request.fork or head.repo.fork because when the
// base repository is itself a GitHub fork, those fields report true
// even for same-repo PRs.

const (
	// A fork PR: head repo (alice/widgets) != base repo (cicadoci/widgets).
	forkPRJSON = `{"action":"opened","pull_request":{"number":42,"fork":true,"base":{"ref":"main"},"head":{"ref":"feature","sha":"abc","repo":{"full_name":"alice/widgets","fork":true}}},"repository":{"full_name":"cicadoci/widgets"}}`

	// Same-repo PR: head repo == base repo.
	sameRepoPRJSON = `{"action":"opened","pull_request":{"number":43,"fork":false,"base":{"ref":"main"},"head":{"ref":"feature","sha":"def","repo":{"full_name":"cicadoci/widgets","fork":false}}},"repository":{"full_name":"cicadoci/widgets"}}`

	// Deleted fork: head.repo is null.
	deletedForkJSON = `{"action":"opened","pull_request":{"number":44,"fork":false,"base":{"ref":"main"},"head":{"ref":"feature","sha":"111","repo":null}},"repository":{"full_name":"cicadoci/widgets"}}`

	// Branch collision: head repo differs from base repo by full_name.
	branchCollideJSON = `{"action":"opened","pull_request":{"number":45,"fork":false,"base":{"ref":"main"},"head":{"ref":"main","sha":"222","repo":{"full_name":"acme/widgets","fork":false}}},"repository":{"full_name":"cicadoci/widgets"}}`

	// Base repo is itself a GitHub fork, but the PR is a same-repo PR.
	// pull_request.fork and head.repo.fork are both true, but full_name
	// matches, so IsFork() must return false.
	forkRepoSameRepoPRJSON = `{"action":"opened","pull_request":{"number":46,"fork":true,"base":{"ref":"main"},"head":{"ref":"feature","sha":"555","repo":{"full_name":"alice/widgets","fork":true}}},"repository":{"full_name":"alice/widgets"}}`

	// Zero-value payload (fail-closed).
)

func TestIsFork_ForkPR(t *testing.T) {
	var p PullRequestPayload
	if err := json.Unmarshal([]byte(forkPRJSON), &p); err != nil {
		t.Fatalf("unmarshal forkPRJSON: %v", err)
	}
	if !p.IsFork() {
		t.Error("fork PR (alice/widgets != cicadoci/widgets): IsFork() should be true")
	}
}

func TestIsFork_SameRepoNotFork(t *testing.T) {
	var p PullRequestPayload
	if err := json.Unmarshal([]byte(sameRepoPRJSON), &p); err != nil {
		t.Fatalf("unmarshal sameRepoPRJSON: %v", err)
	}
	if p.IsFork() {
		t.Error("same-repo PR (cicadoci/widgets == cicadoci/widgets): IsFork() should be false")
	}
}

func TestIsFork_DeletedForkNullHead(t *testing.T) {
	var p PullRequestPayload
	if err := json.Unmarshal([]byte(deletedForkJSON), &p); err != nil {
		t.Fatalf("unmarshal deletedForkJSON: %v", err)
	}
	if !p.IsFork() {
		t.Error("null head.repo (deleted fork): IsFork() should be true (fail-closed)")
	}
}

func TestIsFork_BranchCollisionIsFork(t *testing.T) {
	var p PullRequestPayload
	if err := json.Unmarshal([]byte(branchCollideJSON), &p); err != nil {
		t.Fatalf("unmarshal branchCollideJSON: %v", err)
	}
	if !p.IsFork() {
		t.Error("branch in different repo (acme/widgets vs cicadoci/widgets): IsFork() should be true")
	}
}

// TestIsFork_ForkRepoSameRepoPR is the key test for the #98 regression:
// when the base repository is itself a fork, pull_request.fork and
// head.repo.fork are both true, but a same-repo PR must NOT be treated
// as a fork.  Only the full_name comparison is authoritative.
func TestIsFork_ForkRepoSameRepoPR(t *testing.T) {
	var p PullRequestPayload
	if err := json.Unmarshal([]byte(forkRepoSameRepoPRJSON), &p); err != nil {
		t.Fatalf("unmarshal forkRepoSameRepoPRJSON: %v", err)
	}
	if p.IsFork() {
		t.Error("same-repo PR on a forked repo: IsFork() should be false " +
			"(pull_request.fork=true and head.repo.fork=true are both irrelevant)")
	}
}

func TestIsFork_ZeroValueIsForkFailClosed(t *testing.T) {
	var p PullRequestPayload // zero value
	if !p.IsFork() {
		t.Error("zero-value payload: IsFork() should be true (fail-closed)")
	}
}

func TestIsFork_EmptyRepoFullNameFailClosed(t *testing.T) {
	// Malformed payload: repository.full_name is empty.
	// We cannot confirm the head ref, so fail-closed = treat as fork.
	var p PullRequestPayload
	if err := json.Unmarshal([]byte(`{"action":"opened","pull_request":{"number":1,"head":{"ref":"x","sha":"abc","repo":{"full_name":"other/widgets"}}},"repository":{"full_name":""}}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.IsFork() {
		t.Error("empty repository.full_name: IsFork() should be true (fail-closed)")
	}
}
