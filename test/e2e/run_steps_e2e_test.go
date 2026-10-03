package e2e

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/controller"
)

// TestYAMLControllerRunner is the CIC-147 acceptance test: a run: step goes
// YAML -> parse -> controller (convertSteps builds the steps, the same path
// webhook-created runs use) -> queue -> a real runner over gRPC, instead of
// argv-shaped commands hand-inserted into the steps table.  Before CIC-147 the
// whole script became argv[0] and the step failed as "executable not found".
func TestYAMLControllerRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database and processes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h := startServer(t, ctx)
	regToken := h.mintRegistrationToken(t, []string{"linux", "shell"})
	// Controller-created jobs use the cicanscript executor (jobs.go, CIC-132).
	h.startRunnerWithExecutors(t, ctx, regToken, "cicanscript")

	// Insert org/repo/run similar to seedJob.
	var orgID, repoID, runID uuid.UUID
	if err := h.store.Pool().QueryRow(context.Background(), `SELECT id FROM orgs LIMIT 1`).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := h.store.Pool().QueryRow(context.Background(), `INSERT INTO repos (org_id, owner, name) VALUES ($1, 'cicadoci', 'core') RETURNING id`, orgID).Scan(&repoID); err != nil {
		t.Fatalf("repo: %v", err)
	}
	if err := h.store.Pool().QueryRow(context.Background(), `INSERT INTO runs (repo_id, number, event, ref, sha, actor, status) VALUES ($1, 1, 'push', 'refs/heads/main', 'abc123', 'tester', 'running') RETURNING id`, repoID).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Prepare a simple workflow YAML.
	yaml := []byte(`name: test
on:
  push: {}
jobs:
  hello:
    runs-on: [linux]
    steps:
      - run: echo "a b" && echo c`)

	wf, err := controller.ParseWorkflowBytes(yaml, "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Register workflow with static loader.
	loader := controller.NewStaticLoader()
	loader.Register(repoID, wf)

	// Stub forge (no reporting needed).
	stub := &controller.NoOpForge{}
	// Actually controller.NoOpForge exists; use it.
	// Build controller.
	c := controller.New(h.store, stub, controller.Config{WorkflowLoader: loader}, nil)

	// Trigger LoadAndCreateJobs as a push event.
	if err := c.LoadAndCreateJobs(context.Background(), repoID, runID, "push", "main", "abc123", "", false, "", ""); err != nil {
		t.Fatalf("LoadAndCreateJobs: %v", err)
	}

	// Find the created job.
	var jobID uuid.UUID
	if err := h.store.Pool().QueryRow(context.Background(), `SELECT id FROM jobs WHERE run_id = $1 LIMIT 1`, runID).Scan(&jobID); err != nil {
		t.Fatalf("job: %v", err)
	}

	// Wait for job to succeed.
	status := awaitJobStatus(t, h.store.Pool(), jobID, 60*time.Second, "succeeded")
	if status != "succeeded" {
		t.Fatalf("job status %s, logs:\n%s", status, jobLogs(t, h.store.Pool(), jobID))
	}
	logs := jobLogs(t, h.store.Pool(), jobID)
	// Both commands of the && chain must have run, each printing its own
	// line: the quoted "a b" survives as one argument, then "c".
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if !slices.Contains(lines, "a b") || !slices.Contains(lines, "c") {
		t.Fatalf("logs missing the lines \"a b\" and \"c\": %q", logs)
	}
}
