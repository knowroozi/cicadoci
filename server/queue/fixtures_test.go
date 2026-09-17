package queue_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedOrgRepoRun creates the tenancy rows a job needs to exist, and returns the
// run id to hang jobs off.
func seedOrgRepoRun(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var orgID, repoID, runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id`,
		"org-"+uuid.NewString()[:8]).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'cicadoci', $2) RETURNING id`,
		orgID, "repo-"+uuid.NewString()[:8]).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event, ref, sha, actor)
		 VALUES ($1, 1, 'push', 'refs/heads/main', 'deadbeef', 'tester')
		 RETURNING id`, repoID).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// seedRunner creates a runner with the given labels and returns its id.
func seedRunner(t *testing.T, pool *pgxpool.Pool, labels []string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var orgID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM orgs LIMIT 1`).Scan(&orgID); err != nil {
		if err := pool.QueryRow(ctx,
			`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id`,
			"org-"+uuid.NewString()[:8]).Scan(&orgID); err != nil {
			t.Fatalf("seed org for runner: %v", err)
		}
	}

	var runnerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO runners (org_id, name, labels, executors, status)
		 VALUES ($1, $2, $3, ARRAY['shell','docker'], 'online') RETURNING id`,
		orgID, "runner-"+uuid.NewString()[:8], labels).Scan(&runnerID); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	return runnerID
}

type jobOpt func(*jobSpec)

type jobSpec struct {
	status       string
	executor     string
	requiredTags []string
	priority     int
	steps        int
}

func withStatus(s string) jobOpt     { return func(j *jobSpec) { j.status = s } }
func withTags(tags ...string) jobOpt { return func(j *jobSpec) { j.requiredTags = tags } }
func withExecutor(e string) jobOpt   { return func(j *jobSpec) { j.executor = e } }
func withPriority(p int) jobOpt      { return func(j *jobSpec) { j.priority = p } }
func withSteps(n int) jobOpt         { return func(j *jobSpec) { j.steps = n } }

// seedJob inserts a job, queued by default, and returns its id.
func seedJob(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, opts ...jobOpt) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	spec := jobSpec{status: "queued", executor: "shell", requiredTags: []string{}, priority: 100}
	for _, o := range opts {
		o(&spec)
	}

	var jobID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO jobs (run_id, name, status, executor, required_tags, priority, env)
		 VALUES ($1, $2, $3, $4, $5, $6, '{"CI":"true"}'::jsonb) RETURNING id`,
		runID, "job-"+uuid.NewString()[:8], spec.status, spec.executor,
		spec.requiredTags, spec.priority).Scan(&jobID); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	for i := 0; i < spec.steps; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO steps (job_id, step_index, name, image, command, env)
			 VALUES ($1, $2, $3, 'alpine:3.20', ARRAY['echo', $4], '{}'::jsonb)`,
			jobID, i, fmt.Sprintf("step-%d", i), fmt.Sprintf("hello from step %d", i)); err != nil {
			t.Fatalf("seed step: %v", err)
		}
	}
	return jobID
}

// jobStatus reads a job's current status.
func jobStatus(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM jobs WHERE id = $1`, jobID).Scan(&status); err != nil {
		t.Fatalf("read job status: %v", err)
	}
	return status
}
