package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/controller"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/webhook"
)

// TestHandlePushCreatesClaimableGreetJob drives a GitHub push event through the
// controller's HandlePush — the same path a signed /api/v1/webhook/github
// request takes — and asserts that the demo workflow's "greet" job is created
// and claimable. It is the CIC-78 acceptance proof that the store-loader path
// produces jobs from pushed events, not just pre-seed.
//
// PG-gated: skips when CICADO_TEST_DATABASE_URL is unset (testdb.New(t) skips).
func TestHandlePushCreatesClaimableGreetJob(t *testing.T) {
	st := testdb.New(t)
	q := queue.NewPostgres(st.Pool())
	ctx := context.Background()

	// devSeed registers the demo workflow under the cicadoci/org that
	// ResolveRepo will use when HandlePush receives a push for cicadoci/core.
	// No extra seeding is needed: devSeed already does the right thing.
	if err := devSeed(ctx, st, q, devSeedLogger()); err != nil {
		t.Fatalf("devSeed: %v", err)
	}

	ciOrg, err := st.UpsertOrg(ctx, "cicadoci", "Cicadoci")
	if err != nil {
		t.Fatalf("upsert cicadoci org: %v", err)
	}

	// A runner that can execute both shell and cicanscript jobs.  The demo
	// workflow's "greet" job defaults to the cicanscript executor (the MVP
	// default from convertJobSpecs in controller/jobs.go).
	var runnerID uuid.UUID
	pool := st.Pool()
	if err := pool.QueryRow(ctx, `
		INSERT INTO runners (org_id, name, labels, executors, status)
		VALUES ($1, $2, ARRAY['shell'], ARRAY['shell','cicanscript'], 'online')
		RETURNING id`,
		ciOrg,
		"acc-gh-"+uuid.NewString()[:8],
	).Scan(&runnerID); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	// Build the controller exactly as main.go does after CIC-78: StoreLoader
	// reads the workflows table; NoOpForge means no external side-effects.
	ctrl := controller.New(st, controller.NoOpForge{},
		controller.Config{
			WorkflowLoader: controller.NewStoreLoader(st, devSeedLogger()),
			DefaultBranch:  "master",
		},
		devSeedLogger(),
	)

	// Drive a push event for cicadoci/core main. The demo workflow's trigger is
	// on: push: {} which matches any branch.
	push := webhook.PushPayload{}
	push.Ref = "refs/heads/main"
	push.After = "deadbeef"
	push.HeadCommit.ID = "deadbeef"
	push.Repository.FullName = "cicadoci/core"
	push.Repository.Name = "core"
	push.Pusher.Name = "cico"
	push.Pusher.Email = "cico@example.com"

	if err := ctrl.HandlePush(push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	// Claim and assert "greet" is in the claimed set.
	claimed, err := q.Claim(ctx, queue.ClaimRequest{
		RunnerID:  runnerID,
		Labels:    []string{"shell"},
		Executors: []string{"shell", "cicanscript"},
		Limit:     10,
		LeaseTTL:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	seen := make(map[string]bool)
	for _, c := range claimed {
		seen[c.Name] = true
	}
	if !seen["greet"] {
		t.Fatalf("expected 'greet' job in claimed set after HandlePush; got: %v", seen)
	}
}
