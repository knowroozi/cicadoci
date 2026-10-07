// CIC-209 T1: zero-job gated run ends up 'succeeded', not 'failed'.
//
// When a PR run matches zero workflows and a gate was opened,
// finishIfNoJobs finalises the run to 'succeeded' BEFORE calling
// CloseNoJobs, so the async dispatchAfterGate (if it fires) sees a
// terminal status and skips reportFailed (F1 moved-guard).
//
// Skips when CICADO_TEST_DATABASE_URL is unset (via testdb.New).

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/chat"
	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
)

// TestCic209_CloseNoJobsProducesReasonNoJobs is a pure approvalgates-level
// test: calling mgr.CloseNoJobs(runID) on a pending gate produces
// GateTimedOut with ReasonNoJobs, not ReasonApprovalTimedOut. The
// controller-path test is TestCic209_LoadFailedGatedRun_UsesReasonNoJobs.
func TestCic209_CloseNoJobsProducesReasonNoJobs(t *testing.T) {
	mgr := approvalgates.NewManager(chat.NewSlackProvider(), nil)
	ctx := context.Background()
	const rid = "close-no-jobs-run-id"
	if err := mgr.Open(ctx, &approvalgates.Gate{
		RunID:   rid,
		Channel: "ci",
		Reason:  "needs approval",
		Allow:   "allow",
		Deny:    "deny",
	}); err != nil {
		t.Fatalf("Open gate: %v", err)
	}
	if gs := mgr.State(rid); gs != approvalgates.GatePending {
		t.Fatalf("precondition: gate state = %v, want GatePending", gs)
	}
	mgr.CloseNoJobs(rid)
	gs, reason := mgr.Await(context.Background(), rid)
	if gs != approvalgates.GateTimedOut {
		t.Fatalf("gate state = %v, want GateTimedOut", gs)
	}
	if reason != approvalgates.ReasonNoJobs {
		t.Fatalf("terminal reason = %q, want %q", reason, approvalgates.ReasonNoJobs)
	}
}

// TestCic209_ZeroJobGatedRun_NotFailed is T1 for CIC-209: a zero-job gated run
// (matched zero workflows or load error) must not be marked 'failed'.
//
// Pre: PR run matched zero workflows; gate opened with small ApprovalTimeout.
// Action: finishIfNoJobs(n=0, gateOpened=true) is called.
// Asserts:
//   - run status is 'succeeded' (N1: not 'failed')
//   - gate terminal reason is ReasonNoJobs (N1: not ReasonApprovalTimedOut)
//   - forge has no ConclusionFailure for this run (F1 moved-guard)
func TestCic209_ZeroJobGatedRun_NotFailed(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	db.Migrate(ctx)
	pool := db.Pool()
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
       VALUES ($1, $2, 'refs/pull/42/head', 1, 'pending', 'pull_request', 'abc123', 'user-1', $3)`,
		runID, repoID, store.TriggerForge); err != nil {
		t.Fatal(err)
	}

	fg := &stubForge{}
	mgr := approvalgates.NewManager(chat.NewSlackProvider(), nil)
	c := New(db, fg, Config{
		ApprovalGates:   mgr,
		ApprovalTimeout: 50 * time.Millisecond,
	}, discardLogger)

	// Open the gate (what HandlePullRequest does before LoadAndCreateJobs).
	if err := mgr.Open(ctx, &approvalgates.Gate{
		RunID:   runID.String(),
		Channel: "ci",
		Reason:  "needs approval",
		Allow:   "allow",
		Deny:    "deny",
		Allowed: []string{"user-1"},
		Timeout: 50 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Open gate: %v", err)
	}
	if gs := mgr.State(runID.String()); gs != approvalgates.GatePending {
		t.Fatalf("precondition: gate state = %v, want GatePending", gs)
	}

	// Simulate a zero-job PR run: n=0, gateOpened=true.
	// CIC-209 fix finalises the run to 'succeeded' before CloseNoJobs.
	c.finishIfNoJobs(ctx, runID, nil, true)

	// Start the async waiter (what HandlePullRequest does when gateOpened).
	c.spawnGateDispatch(runID)

	// Give the async waiter time to drain.
	<-time.After(300 * time.Millisecond)

	// N1: the run must be 'succeeded', not 'failed'.
	run, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "succeeded" {
		t.Fatalf("N1: run status = %q, want succeeded", run.Status)
	}

	// N1: gate must be closed with ReasonNoJobs.
	gs, reason := mgr.Await(context.Background(), runID.String())
	if gs != approvalgates.GateTimedOut {
		t.Fatalf("N1: gate state = %v, want GateTimedOut (from CloseNoJobs)", gs)
	}
	if reason != approvalgates.ReasonNoJobs {
		t.Fatalf("N1: terminal reason = %q, want %q", reason, approvalgates.ReasonNoJobs)
	}

	// F1 moved-guard: no ConclusionFailure should have fired.
	for _, cc := range fg.completes {
		if cc.ref.RunID == runID.String() && cc.conclusion == forge.ConclusionFailure {
			t.Fatalf("F1: forge saw ConclusionFailure for zero-job run; moved-guard violated")
		}
	}
}

// TestCic209_LoadFailedGatedRun_UsesReasonNoJobs drives the controller
// load-failure path through HandlePullRequest: a gated PR run with a
// bad-DAG workflow causes LoadAndCreateJobs to return an error,
// triggering CloseNoJobs(runID). The gate must terminate with
// ReasonNoJobs, not ReasonApprovalTimedOut.
func TestCic209_LoadFailedGatedRun_UsesReasonNoJobs(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "core", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}

	// Insert a bad-DAG workflow: BuildDAG fails → LoadAndCreateJobs returns an error.
	src := []byte("name: bad-dag\non:\n  pull_request: {}\n" +
		"jobs:\n" +
		"  build:\n" +
		"    needs: nonexistent\n" +
		"    runs-on: [default]\n" +
		"    steps:\n" +
		"      - run: echo hello\n")
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
		ContentSHA: "bad-dag-sha-load-fail-test",
	}); err != nil {
		t.Fatalf("upsert workflow: %v", err)
	}

	prov := &recordingProvider{}
	mgr := approvalgates.NewManager(prov, nil)

	cfg := DefaultConfig()
	cfg.WorkflowLoader = NewStoreLoader(st, discardLogger)
	cfg.ApprovalGates = mgr
	cfg.ApprovalChannel = "C0002"
	cfg.ApprovalTimeout = time.Hour
	cfg.AllowedApprovers = []string{"U001"}

	c := New(st, NoOpForge{}, cfg, discardLogger)

	// Drive HandlePullRequest — the gate opens, then LoadAndCreateJobs fails.
	payload := webhook.PullRequestPayload{
		PullRequest: webhook.PullRequest{Number: 13},
		Action:      "opened",
		Repository:  webhook.Repo{FullName: "acme/core"},
	}
	// We don't care about the error from HandlePullRequest — load failure
	// is handled internally (FinalizeRunToFailed + CloseNoJobs). The gate close
	// happens synchronously before return.
	_ = c.HandlePullRequest(payload)

	// The gate must be closed with ReasonNoJobs, not ReasonApprovalTimedOut.
	if len(prov.calls) == 0 {
		t.Fatal("expected RequestApproval call in prov.calls")
	}
	gs, reason := mgr.Await(context.Background(), prov.calls[0].Req.RunID)
	if gs != approvalgates.GateTimedOut {
		t.Fatalf("gate state = %v, want GateTimedOut (from CloseNoJobs)", gs)
	}
	if reason != approvalgates.ReasonNoJobs {
		t.Fatalf("terminal reason = %q, want %q", reason, approvalgates.ReasonNoJobs)
	}
}
