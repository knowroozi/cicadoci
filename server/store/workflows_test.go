package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/workflow"
)

// TestUpsertWorkflowRoundTrip proves the write path CIC-78 adds: a parsed
// workflow can be registered and read back by a StoreLoader. The upsert is
// idempotent on (repo_id, path): the second registration overwrites in place
// with a single row and the same id.
func TestUpsertWorkflowRoundTrip(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "core", "master")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}

	path := ".github/workflows/ci.yaml"
	def, _ := json.Marshal(&workflow.Workflow{Name: "ci", Jobs: map[string]*workflow.Job{
		"build": {RunsOn: []string{"shell"}},
	}})

	id, err := st.UpsertWorkflow(ctx, store.WorkflowInput{
		RepoID:     repoID,
		Name:       "ci",
		Path:       path,
		Source:     "name: ci\non: push: {}\njobs:\n  build:\n    runs_on: shell\n",
		Definition: def,
		ContentSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("upsert workflow: %v", err)
	}

	// Read it back the way a StoreLoader does: by the repo.
	// (ListWorkflowsForRepo is the read projection the loader uses.)
	got, err := st.ListWorkflowsForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list workflows = %d rows, want 1", len(got))
	}
	row0 := got[0]
	if row0.ID != id {
		t.Errorf("row id = %s, want %s", row0.ID, id)
	}
	if row0.Name != "ci" {
		t.Errorf("row name = %q, want ci", row0.Name)
	}
	if row0.Path != path {
		t.Errorf("row path = %q, want %q", row0.Path, path)
	}
	if row0.ContentSHA != "deadbeef" {
		t.Errorf("row content_sha = %q, want deadbeef", row0.ContentSHA)
	}
	var wf workflow.Workflow
	if err := json.Unmarshal(row0.Definition, &wf); err != nil {
		t.Fatalf("decode stored definition: %v", err)
	}
	if wf.Name != "ci" || wf.Jobs["build"] == nil {
		t.Errorf("stored definition did not round-trip: %+v", wf)
	}

	// Re-register the same path: one row, same id, newer fields. The upsert
	// must not add a second row.
	_, err = st.UpsertWorkflow(ctx, store.WorkflowInput{
		RepoID:     repoID,
		Name:       "ci-renamed",
		Path:       path,
		Source:     "v2",
		Definition: []byte(`{"name":"ci-renamed"}`),
		ContentSHA: "cafebabe",
	})
	if err != nil {
		t.Fatalf("upsert workflow (2nd time): %v", err)
	}
	got, err = st.ListWorkflowsForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("list workflows (2nd): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list workflows after upsert = %d rows, want 1 (idempotent)", len(got))
	}
	if got[0].ID != id {
		t.Errorf("row id after upsert = %s, want %s (same row)", got[0].ID, id)
	}
	if got[0].ContentSHA != "cafebabe" {
		t.Errorf("content_sha after upsert = %q, want cafebabe", got[0].ContentSHA)
	}
}

// TestUpsertWorkflowRequiresRepoAndPath guards the no-op guards: an empty repo
// or empty path must fail rather than write an unusable row.
func TestUpsertWorkflowRequiresRepoAndPath(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	if _, err := st.UpsertWorkflow(ctx, store.WorkflowInput{RepoID: uuid.Nil, Path: "x"}); err == nil {
		t.Error("upsert with empty repo id: expected error, got nil")
	}
	if _, err := st.UpsertWorkflow(ctx, store.WorkflowInput{RepoID: uuid.New(), Path: ""}); err == nil {
		t.Error("upsert with empty path: expected error, got nil")
	}
}

// TestDeleteWorkflowsNotIn proves the CIC-120 prune: rows whose path is not in
// the kept set are deleted, the delete is scoped to the given repo, and an
// empty kept set clears that repo only.
func TestDeleteWorkflowsNotIn(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoA, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "core", "main")
	if err != nil {
		t.Fatalf("repo a: %v", err)
	}
	repoB, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "other", "main")
	if err != nil {
		t.Fatalf("repo b: %v", err)
	}
	def, _ := json.Marshal(&workflow.Workflow{Name: "x"})
	put := func(repo uuid.UUID, path string) {
		t.Helper()
		if _, err := st.UpsertWorkflow(ctx, store.WorkflowInput{
			RepoID: repo, Name: path, Path: path, Definition: def,
		}); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
	}
	paths := func(repo uuid.UUID) []string {
		t.Helper()
		rows, err := st.ListWorkflowsForRepo(ctx, repo)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Path)
		}
		return out
	}

	put(repoA, ".github/workflows/ci.yaml")
	put(repoA, ".github/workflows/old-deploy.yml")
	put(repoB, ".github/workflows/old-deploy.yml") // same path, other repo

	n, err := st.DeleteWorkflowsNotIn(ctx, repoA, []string{".github/workflows/ci.yaml"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	if got := paths(repoA); len(got) != 1 || got[0] != ".github/workflows/ci.yaml" {
		t.Errorf("repo a paths = %v, want [ci.yaml]", got)
	}
	if got := paths(repoB); len(got) != 1 {
		t.Errorf("repo b paths = %v, want its row untouched", got)
	}

	// Empty kept set (workflows directory gone): clears repo a only.
	if n, err := st.DeleteWorkflowsNotIn(ctx, repoA, nil); err != nil || n != 1 {
		t.Errorf("delete all = (%d, %v), want (1, nil)", n, err)
	}
	if got := paths(repoA); len(got) != 0 {
		t.Errorf("repo a paths = %v, want none", got)
	}
	if got := paths(repoB); len(got) != 1 {
		t.Errorf("repo b paths = %v, want its row untouched", got)
	}

	if _, err := st.DeleteWorkflowsNotIn(ctx, uuid.Nil, nil); err == nil {
		t.Error("nil repo id: want error")
	}
}
