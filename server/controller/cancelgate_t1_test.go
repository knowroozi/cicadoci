// CIC-208: cancel-then-resolve race tests (Postgres-gated via testdb.New).
//
// TestCancelGate_ThenResolveRejected (T1) verifies that after CancelRunJobs
// + CancelGate, the controller does not fire a 'failed' forge Complete.
//
// TestFinalizeSucceeded_ThenDispatchAfterGate_T3 (T3) verifies the
// F1 moved-guard: when a run is already terminal ('succeeded'),
// dispatchAfterGate does not call ReportFailed.
//
// Skips when CICADO_TEST_DATABASE_URL is unset (via testdb.New).

package controller

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/chat"
	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/store"
)

// TestCancelGate_ThenResolveRejected is T1 for CIC-208 F1/F2.
//
// After CancelRunJobs finalises a run as 'cancelled' and mgr.CancelGate
// closes the approval gate with GateTimedOut, dispatchAfterGate calls
// FinalizeRunToFailed which returns (moved=false) because the row is
// no longer pending. The F1 guard skips both ReleaseRun and reportFailed.
func TestCancelGate_ThenResolveRejected(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (id, repo_id, ref, number, status, event, sha, actor, trigger_source)
      VALUES ($1, $2, 'main', 1, 'pending', 'push', 'abc123', 'user-1', $3)`,
		runID, repoID, store.TriggerForge); err != nil {
		t.Fatal(err)
	}

	fg := &stubForge{}
	mgr := approvalgates.NewManager(chat.NewSlackProvider(), nil)
	c := New(st, fg, Config{ApprovalGates: mgr}, discardLogger)

	gate := &approvalgates.Gate{
		RunID:   runID.String(),
		Channel: "ci",
		Reason:  "needs approval",
		Allow:   "allow",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: 0,
	}
	_ = mgr.Open(ctx, gate)

	_, err := st.CancelRunJobs(ctx, runID, "operator cancel",
		func(_ context.Context, _ uuid.UUID, _ string) error { return nil },
		func(_ context.Context, _ uuid.UUID) {},
	)
	if err != nil {
		t.Fatalf("CancelRunJobs: %v", err)
	}

	mgr.CancelGate(runID.String())
	c.dispatchAfterGate(runID, approvalgates.GateTimedOut, approvalgates.ReasonRunCancelled)

	if s := mgr.State(runID.String()); s != approvalgates.GateTimedOut {
		t.Fatalf("gate state = %v, want GateTimedOut", s)
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", run.Status)
	}

	for _, cc := range fg.completes {
		if cc.ref.RunID == runID.String() && cc.conclusion == forge.ConclusionFailure {
			t.Fatal("F1/T1: forge saw ConclusionFailure after cancel; reportFailed not skipped")
		}
	}
}

// TestFinalizeSucceeded_ThenDispatchAfterGate_T3 is T3 from the review.
//
// After c.report + c.RunCompleted("succeeded"), dispatchAfterGate with
// GateTimedOut must NOT call ReportFailed because FinalizeRunToFailed
// returns moved=false. Exactly one Complete is recorded: the succeeded.
func TestFinalizeSucceeded_ThenDispatchAfterGate_T3(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	st.Migrate(ctx)
	pool := st.Pool()
	orgID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO orgs (id, slug, name) VALUES ($1, 'acme', 'Acme')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, org_id, owner, name) VALUES ($1, $2, 'acme', 'core')`, repoID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (id, repo_id, ref, number, status, event, sha, actor, trigger_source)
      VALUES ($1, $2, 'main', 1, 'pending', 'push', 'abc123', 'user-1', $3)`,
		runID, repoID, store.TriggerForge); err != nil {
		t.Fatal(err)
	}

	fg := &stubForge{}
	mgr := approvalgates.NewManager(chat.NewSlackProvider(), nil)
	c := New(st, fg, Config{ApprovalGates: mgr}, discardLogger)

	// Prepare the forge check and finalise the run as succeeded.
	c.report(ctx, "acme", "core", "abc123", runID, "main")
	if err := mgr.Open(ctx, &approvalgates.Gate{
		RunID:   runID.String(),
		Channel: "ci",
		Reason:  "needs approval",
		Allow:   "allow",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: 0,
	}); err != nil {
		t.Fatalf("Open gate: %v", err)
	}
	if err := c.store.FinalizeRunToSucceeded(ctx, runID); err != nil {
		t.Fatalf("FinalizeRunToSucceeded: %v", err)
	}
	if err := c.RunCompleted(ctx, runID, "succeeded"); err != nil {
		t.Fatalf("RunCompleted succeeded: %v", err)
	}

	// F1 moved guard: dispatchAfterGate must NOT fire reportFailed here.
	c.dispatchAfterGate(runID, approvalgates.GateTimedOut, "approval timed out")

	// Exactly one Complete: the succeeded one.
	var n int
	for _, cc := range fg.completes {
		if cc.ref.RunID != runID.String() {
			continue
		}
		n++
		if cc.conclusion == forge.ConclusionFailure {
			t.Fatalf("T3: forge saw ConclusionFailure; F1 moved guard not working")
		}
	}
	if n != 1 {
		t.Fatalf("T3: expected exactly 1 Complete for runID, got %d", n)
	}
}
