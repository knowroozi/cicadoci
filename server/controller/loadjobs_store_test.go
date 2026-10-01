package controller

import (
	"context"
	"encoding/json"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/store"
)

// TestLoadAndCreateJobsViaStoreLoader is the end-to-end proof CIC-78 asks for:
// with a store-backed loader wired into the controller, a run produces a job DAG.
// It is the inverse of TestLoadAndCreateJobs_NilLoader: there the loader is nil so
// no jobs are created; here the loader reads a row the store wrote, so a job is.
//
// It is gated on a real Postgres; it skips when CICADO_TEST_DATABASE_URL is unset,
// so it does not break an offline test run.
func TestLoadAndCreateJobsViaStoreLoader(t *testing.T) {
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

	// Register a push-triggered workflow via the write path CIC-78 adds. The
	// JSON the loader decodes is produced by the same parse+marshal seam a
	// production producer uses, so the round-trip shape is authoritative.
	src := []byte("name: ci\non:\n  push: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo build\"]\n")
	wf, err := ParseWorkflowBytes(src, "ci.yaml")
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	def, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("marshal workflow: %v", err)
	}
	if _, err := st.UpsertWorkflow(ctx, store.WorkflowInput{
		RepoID:     repoID,
		Name:       wf.Name,
		Path:       ".github/workflows/ci.yaml",
		Source:     string(src),
		Definition: def,
		ContentSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("upsert workflow: %v", err)
	}

	// Wire the store loader into the controller, exactly as main.go now does.
	cfg := DefaultConfig()
	cfg.WorkflowLoader = NewStoreLoader(st, discardLogger)
	c := New(st, NoOpForge{}, cfg, discardLogger)

	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "push",
		Ref:           "refs/heads/master",
		Sha:           "deadbeef",
		Actor:         "42",
		TriggerSource: store.TriggerForge,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "push", "refs/heads/master", "deadbeef", "", false); err != nil {
		t.Fatalf("LoadAndCreateJobs: %v", err)
	}

	var count int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM jobs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("jobs created for run = %d, want 1 (the loader found the workflow and built its DAG)", count)
	}
}
