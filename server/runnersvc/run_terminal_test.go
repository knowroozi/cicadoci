package runnersvc_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/runnersvc"
)

// TestNewInstallsRunTerminalHook pins the CIC-103 plumbing: a hook set on
// Config before New must be the one the queue fires when a run's last job
// finishes, with the run's final status, exactly once.
func TestNewInstallsRunTerminalHook(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	ctx := context.Background()
	q := queue.NewPostgres(pool)

	var mu sync.Mutex
	var calls []string
	cfg := runnersvc.DefaultConfig()
	cfg.OnRunTerminal = func(runID uuid.UUID, state string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, runID.String()+":"+state)
	}
	runnersvc.New(pool, q, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	var orgID, repoID, runID, jobID, lease uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('hook', 'Hook') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'cicadoci', 'hook') RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event) VALUES ($1, 1, 'push') RETURNING id`,
		repoID).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO jobs (run_id, name, status, lease_id, lease_expires_at)
		VALUES ($1, 'only', 'running', gen_random_uuid(), now() + interval '1 hour')
		RETURNING id, lease_id`, runID).Scan(&jobID, &lease); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	if err := q.Complete(ctx, queue.Completion{
		JobID: jobID, LeaseID: lease, State: queue.StateFailed, ExitCode: 1,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := runID.String() + ":failed"
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("OnRunTerminal calls = %v, want exactly [%s]", calls, want)
	}
}
