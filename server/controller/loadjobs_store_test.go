package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
	"cicadoci.com/cicado/server/workflow"
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
		Path:       ".cicado/workflows/ci.yaml",
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

	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "push", "refs/heads/master", "deadbeef", "", false,
		"https://github.com/org/repo.git", "refs/heads/master", false); err != nil {
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

// TestLoadAndCreateJobs_PersistsSteps verifies that multi-step job specs
// are persisted to the steps table and can be read back.
func TestLoadAndCreateJobs_PersistsSteps(t *testing.T) {
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

	src := []byte("name: ci\non:\n  push: {}\n" +
		"jobs:\n" +
		"  build:\n" +
		"    runs-on: [default]\n" +
		"    steps:\n" +
		"        - name: install\n" +
		"          run: apt-get install -y gcc\n" +
		"        - name: compile\n" +
		"          run: make build\n" +
		"        - name: test\n" +
		"          run: make test\n")
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
		Path:       ".cicado/workflows/ci.yaml",
		Source:     string(src),
		Definition: def,
		ContentSHA: "cishas",
	}); err != nil {
		t.Fatalf("upsert workflow: %v", err)
	}

	cfg := DefaultConfig()
	cfg.WorkflowLoader = NewStoreLoader(st, discardLogger)
	c := New(st, NoOpForge{}, cfg, discardLogger)

	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "push",
		Ref:           "refs/heads/master",
		Sha:           "cishas",
		Actor:         "42",
		TriggerSource: store.TriggerForge,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "push", "refs/heads/master", "cishas", "", false, "", "", false); err != nil {
		t.Fatalf("LoadAndCreateJobs: %v", err)
	}

	// Read back all steps for the run's job.
	rows, err := st.Pool().Query(ctx,
		`SELECT s.name FROM steps s JOIN jobs j ON j.id = s.job_id WHERE j.run_id = $1 ORDER BY s.step_index`,
		runID)
	if err != nil {
		t.Fatalf("query steps: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan step: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	want := []string{"install", "compile", "test"}
	if len(names) != len(want) {
		t.Fatalf("got %d steps, want %d: %v", len(names), len(want), names)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("step %d name: got %q, want %q", i, names[i], w)
		}
	}
}

// TestLoadAndCreateJobs_NilLoader_FinalisesRun verifies that when no
// WorkflowLoader is configured, LoadAndCreateJobs finalises the run as
// succeeded instead of leaving it pending forever (CIC-85 review blocker).
func TestLoadAndCreateJobs_NilLoader_FinalisesRun(t *testing.T) {
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
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "push",
		Ref:           "main",
		Sha:           "abc123",
		Actor:         "bot",
		TriggerSource: store.TriggerForge,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Controller with real store but nil loader.
	c := New(st, nil, Config{}, discardLogger)
	err = c.LoadAndCreateJobs(ctx, repoID, runID, "push", "main", "abc123", "", false, "", "", false)
	if err != nil {
		t.Fatalf("LoadAndCreateJobs: %v", err)
	}
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "succeeded" {
		t.Errorf("expected status \"succeeded\", got %q", run.Status)
	}
}

// TestFinalizeRunToFailed verifies the store method flips a pending run to
// 'failed' and is idempotent.
func TestFinalizeRunToFailed(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "core", "master")
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "push",
		Ref:           "refs/heads/main",
		Sha:           "abc123",
		Actor:         "42",
		TriggerSource: store.TriggerForge,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Flip to failed.
	moved, ferr := st.FinalizeRunToFailed(ctx, runID)
	if ferr != nil {
		t.Fatalf("FinalizeRunToFailed: %v", ferr)
	}
	if !moved {
		t.Fatalf("FinalizeRunToFailed should return moved=true for pending run")
	}

	// Verify.
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "failed" {
		t.Fatalf("status = %q, want failed", run.Status)
	}

	// Idempotent: second call should be a no-op and return moved=false.
	moved2, ferr := st.FinalizeRunToFailed(ctx, runID)
	if ferr != nil {
		t.Fatalf("second FinalizeRunToFailed: %v", ferr)
	}
	if moved2 {
		t.Fatalf("second FinalizeRunToFailed should return moved=false, got true")
	}
	// Status should still be 'failed'.
	run, err = st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun 2: %v", err)
	}
	if run.Status != "failed" {
		t.Fatalf("status = %q, want failed (idempotent)", run.Status)
	}
}

// TestLoadAndCreateJobs_MatchedButFailedFinalisesFailed verifies that when a
// workflow matches the event but its DAG cannot be built, the run is
// finalised to 'failed' not 'succeeded' (CIC-85 matched-but-failed).
func TestLoadAndCreateJobs_MatchedButFailedFinalisesFailed(t *testing.T) {
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

	// A workflow with push: {} (matches all branches) but a bad DAG
	// (unknown dependency). BuildDAG fails → no jobs created → failed.
	src := []byte("name: bad-dag\non:\n  push: {}\n" +
		"jobs:\n" +
		"  build:\n" +
		"    needs: nonexistent\n" +
		"    runs-on: [default]\n" +
		"    steps:\n" +
		"       - run: echo hello\n")

	wf, err := ParseWorkflowBytes(src, "bad-dag.yaml")
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
		Path:       ".cicado/workflows/bad-dag.yaml",
		Source:     string(src),
		Definition: def,
		ContentSHA: "bad-dag-sha",
	}); err != nil {
		t.Fatalf("upsert workflow: %v", err)
	}

	cfg := DefaultConfig()
	cfg.WorkflowLoader = NewStoreLoader(st, discardLogger)
	c := New(st, NoOpForge{}, cfg, discardLogger)

	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "push",
		Ref:           "refs/heads/master",
		Sha:           "bad-dag-sha",
		Actor:         "42",
		TriggerSource: store.TriggerForge,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// LoadAndCreateJobs matches push:{} to any event, then BuildDAG fails
	// because 'build' needs 'nonexistent'. Run finalises as 'failed'.
	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "push", "refs/heads/master", "bad-dag-sha", "", false, "", "", false); err != nil {
		t.Fatalf("LoadAndCreateJobs: %v", err)
	}

	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("run status = %q, want 'failed' (matched-but-failed run must be failed)", got.Status)
	}
}

// errLoader is a WorkflowLoader whose LoadAll always fails, standing in for a
// transient store or decode error.
type errLoader struct{}

func (errLoader) LoadAll(context.Context, uuid.UUID, string) ([]*workflow.Workflow, error) {
	return nil, errors.New("loader unavailable")
}

// TestHandlePush_LoaderErrorFinalisesFailed drives the full HandlePush path
// with a loader that errors.  The run must end 'failed' — a load error is a
// server fault, not a zero-match — rather than staying 'pending' forever
// (CIC-85).
func TestHandlePush_LoaderErrorFinalisesFailed(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.WorkflowLoader = errLoader{}
	c := New(st, NoOpForge{}, cfg, discardLogger)

	var p webhook.PushPayload
	p.Ref = "refs/heads/main"
	p.After = "abc123"
	p.Repository.FullName = "acme/core"
	if err := c.HandlePush(p); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	// ResolveRepo is idempotent, so this returns the repo HandlePush created.
	_, repoID, err := st.ResolveRepo(ctx, "acme", "core", cfg.DefaultBranch, "")
	if err != nil {
		t.Fatalf("resolve repo: %v", err)
	}
	var status string
	if err := st.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE repo_id = $1`, repoID).Scan(&status); err != nil {
		t.Fatalf("select run: %v", err)
	}
	if status != "failed" {
		t.Errorf("run status = %q, want \"failed\" (a loader error must not leave the run pending)", status)
	}
}

// TestLoadAndCreateJobs_ZeroMatchFinalisesSucceeded stores a workflow whose
// trigger does not match the event.  No job is created and the run must be
// finalised 'succeeded' rather than left pending (CIC-85 zero-match path).
func TestLoadAndCreateJobs_ZeroMatchFinalisesSucceeded(t *testing.T) {
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

	// Only pushes to 'release' trigger this workflow; the event is a push to 'master'.
	src := []byte("name: release-only\non:\n  push:\n    branches: [release]\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo build\"]\n")
	wf, err := ParseWorkflowBytes(src, "release.yaml")
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
		Path:       ".cicado/workflows/release.yaml",
		Source:     string(src),
		Definition: def,
		ContentSHA: "zero-match-sha",
	}); err != nil {
		t.Fatalf("upsert workflow: %v", err)
	}

	cfg := DefaultConfig()
	cfg.WorkflowLoader = NewStoreLoader(st, discardLogger)
	c := New(st, NoOpForge{}, cfg, discardLogger)

	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "push",
		Ref:           "master",
		Sha:           "zero-match-sha",
		Actor:         "42",
		TriggerSource: store.TriggerForge,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if err := c.LoadAndCreateJobs(ctx, repoID, runID, "push", "master", "zero-match-sha", "", false, "", "", false); err != nil {
		t.Fatalf("LoadAndCreateJobs: %v", err)
	}

	var count int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM jobs WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("jobs created = %d, want 0 (the workflow does not match a push to master)", count)
	}
	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "succeeded" {
		t.Errorf("run status = %q, want \"succeeded\" (zero-match run must not stay pending)", got.Status)
	}
}
