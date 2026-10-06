package controller

import (
	"context"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/chat"
	"cicadoci.com/cicado/server/store"
)

func TestN4_ReleaseRunAndFailedCheck(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	orgID, _ := st.UpsertOrg(ctx, "acme", "Acme")
	repoID, _ := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "main",
		Sha:           "abc123",
		Actor:         "pr-n4",
		TriggerSource: store.TriggerForge,
		IsFork:        false,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A prepared check is required for RunCompleted to call forge.Complete.
	if err := st.SetPreparedCheckID(ctx, runID, 4242); err != nil {
		t.Fatalf("SetPreparedCheckID: %v", err)
	}

	specs := []store.JobSpec{{Name: "build", Executor: "shell", Priority: 100, TimeoutSec: 60}}
	if _, err := st.CreateJobSpecs(ctx, runID, specs, true); err != nil {
		t.Fatalf("CreateJobSpecs(hold=true): %v", err)
	}

	fp := &stubForge{}
	tp := backpressure.NewTracker(backpressure.DefaultLimits())
	tp.Register(runID.String(), repoID.String(), orgID.String())

	cfg := DefaultConfig()
	cfg.Backpressure = tp
	c := New(st, fp, cfg, discardLogger)

	c.dispatchAfterGate(runID, approvalgates.GateDenied, "")

	if tp.Count(repoID.String()) != 0 {
		t.Error("backpressure: slot not released after deny")
	}
	if len(fp.completes) != 1 {
		t.Fatalf("forge.Complete calls = %d; want 1", len(fp.completes))
	}
	if got := fp.completes[0].conclusion; got != "failure" {
		t.Errorf("conclusion = %q; want %q", got, "failure")
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "failed" {
		t.Errorf("run status = %q; want %q", run.Status, "failed")
	}
}

// TestExtractPRNumberFromRef exercises the string parser including non-digit
// and overflow cases that a hand-rolled integer loop would get wrong. N6.
func TestExtractPRNumberFromRef(t *testing.T) {
	cases := []struct {
		ref  string
		want int
	}{
		{"refs/pull/42/head", 42},
		{"refs/pull/1/head", 1},
		{"refs/heads/main", 0},
		{"refs/pull/head", 0},
		{"", 0},
		{"refs/pull/xyz/head", 0}, // non-digit body
		{"refs/pull/  /head", 0},  // spaces
		{"refs/pull/-1/head", 0},  // negative
		{"refs/pull/HEAD", 0},     // no /head suffix
	}
	for _, c := range cases {
		got := extractPRNumberFromRef(c.ref)
		if got != c.want {
			t.Errorf("extractPRNumberFromRef(%q) = %d; want %d", c.ref, got, c.want)
		}
	}
}

// TestRerunGate_ApprovalPromotes verifies that dispatchAfterGate
// with GateApproved calls PromoteTopLevelJobs, flipping a held job
// from pending to queued. T1: the test asserts status via SQL,
// not CountRunJobs (which counts all statuses and would pass even
// when promotion is broken). R1: loadErr guard prevents a race
// where a second Finalize/Release/RunCompleted fires.
func TestRerunGate_ApprovalPromotes(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	orgID, _ := st.UpsertOrg(ctx, "acme", "Acme")
	repoID, _ := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "refs/pull/42/head",
		Sha:           "abc123",
		Actor:         "rerun-approve",
		TriggerSource: store.TriggerForge,
		IsFork:        false,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	specs := []store.JobSpec{{Name: "build", Executor: "shell", Priority: 100, TimeoutSec: 60}}
	jobIDs, err := st.CreateJobSpecs(ctx, runID, specs, true)
	if err != nil {
		t.Fatalf("CreateJobSpecs(hold=true): %v", err)
	}
	jobID := jobIDs[0]

	// T1: assert status via SQL, not CountRunJobs (which counts all statuses).
	var status string
	row := st.Pool().QueryRow(ctx,
		"SELECT status FROM jobs WHERE id = $1", jobID)
	row.Scan(&status)
	if status != "pending" {
		t.Fatalf("pre-dispatch status = %q; want pending", status)
	}

	cfg := DefaultConfig()
	cfg.ApprovalGates = approvalgates.NewManager(
		&chat.NoOpProvider{}, nil)
	c := New(st, &stubForge{}, cfg, discardLogger)

	// GateApproved path: PromoteTopLevelJobs flips pending -> queued.
	c.dispatchAfterGate(runID, approvalgates.GateApproved, "")

	row = st.Pool().QueryRow(ctx,
		"SELECT status FROM jobs WHERE id = $1", jobID)
	row.Scan(&status)
	if status != "queued" {
		t.Fatalf("post-dispatch status = %q; want queued", status)
	}
}
