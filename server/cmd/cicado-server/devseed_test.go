package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/queue"
)

func devSeedLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDevSeedProducesClaimableJob exercises the dev-seed path (direct job
// insertion via seedDemoJob) and asserts that a runner carrying the demo
// labels can claim the 'hello' job. It covers the dev-seed path only.
//
// For the full webhook acceptance test (signed HandlePush -> job DAG ->
// claim), see TestHandlePushCreatesClaimableGreetJob in devseed_greet.go.
func TestDevSeedProducesClaimableJob(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	pool := st.Pool()
	ctx := context.Background()

	// Run the seeding path exactly as main.go calls it.
	if err := devSeed(ctx, st, q, devSeedLogger()); err != nil {
		t.Fatalf("devSeed: %v", err)
	}

	// A runner carrying the demo labels (devSeed installs the org, repo and an
	// 'echo hello' job tagged 'shell').
	var orgID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM orgs WHERE slug = 'cicadoci'`).Scan(&orgID); err != nil {
		t.Fatalf("find cicadoci org after seed: %v", err)
	}
	var runnerID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO runners (org_id, name, labels, executors, status)
		VALUES ($1, $2, ARRAY['linux','shell','dev'], ARRAY['shell'], 'online')
		RETURNING id`, orgID, "acc-runner-"+uuid.NewString()[:8]).Scan(&runnerID); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	// Claim: the doorbell fired at the seed COMMIT, so a fresh connection with
	// the right labels+executor must be able to pull the job.
	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"linux", "shell", "dev"},
		Executors: []string{"shell"},
		Limit:     10,
		LeaseTTL:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) < 1 {
		t.Fatalf("dev-seed produced a job but claim returned %d", len(claimed))
	}
	got := claimed[0]
	if got.LeaseID == uuid.Nil {
		t.Fatal("claim returned a nil lease; the lease is the execution authority")
	}
	if got.Name != "hello" {
		t.Errorf("claimed job name = %q, want the dev-seed 'hello' job", got.Name)
	}
}
