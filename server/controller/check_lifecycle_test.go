package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
	"cicadoci.com/cicado/server/workflow"
)

// CIC-81: the forge check is prepared before the run has jobs, and a run
// that gets no jobs completes its check at once. These run against Postgres.

// orderForge is a stubForge that records how many jobs the run had when
// Prepare was called.
type orderForge struct {
	*stubForge
	st          *store.Store
	jobsAtPrep  []int
	prepCountEr error
}

func (f *orderForge) Prepare(ctx context.Context, ref forge.RunRef) (int64, error) {
	n, err := f.st.CountRunJobs(ctx, uuid.MustParse(ref.RunID))
	if err != nil {
		f.prepCountEr = err
	}
	f.jobsAtPrep = append(f.jobsAtPrep, n)
	return f.stubForge.Prepare(ctx, ref)
}

// Prepare runs before LoadAndCreateJobs on every path that creates a run, so
// a job that finishes at once still finds the run's check ID recorded.
func TestCheckPreparedBeforeJobsAreCreated(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)
	of := &orderForge{stubForge: fx.forge, st: fx.st}
	fx.c.forge = of

	if err := pushTo(t, fx, "sha-1"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := fx.c.HandleCheckSuite(decode[webhook.CheckSuitePayload](t, `{"action": "rerequested",
		"check_suite": {"id": 1, "head_sha": "sha-1"}, "repository": {"full_name": "acme/widgets"}}`)); err != nil {
		t.Fatalf("HandleCheckSuite: %v", err)
	}

	if of.prepCountEr != nil {
		t.Fatalf("count jobs in Prepare: %v", of.prepCountEr)
	}
	if len(of.jobsAtPrep) != 2 {
		t.Fatalf("Prepare calls = %d, want 2 (push + re-run)", len(of.jobsAtPrep))
	}
	for i, n := range of.jobsAtPrep {
		if n != 0 {
			t.Errorf("Prepare call %d saw %d jobs, want 0 (check must be prepared before jobs exist)", i+1, n)
		}
	}
	for _, r := range fx.runs(t) {
		if r.jobs != 1 || !r.checkSet {
			t.Errorf("run %s: jobs = %d, check set = %v; want 1 job and a recorded check", r.id, r.jobs, r.checkSet)
		}
	}
	// Both runs have jobs, so their checks wait for OnRunTerminal.
	if n := len(fx.forge.completes); n != 0 {
		t.Errorf("forge.Complete calls = %d, want 0 for runs with jobs", n)
	}
}

func pushTo(t *testing.T, fx rerunFixture, sha string) error {
	t.Helper()
	return fx.c.HandlePush(decode[webhook.PushPayload](t, `{"ref": "refs/heads/main", "after": "`+sha+`",
		"repository": {"full_name": "acme/widgets"}}`))
}

func prTo(t *testing.T, fx rerunFixture) error {
	t.Helper()
	return fx.c.HandlePullRequest(decode[webhook.PullRequestPayload](t, `{"action": "opened",
		"pull_request": {"number": 7, "head": {"sha": "pr-sha", "ref": "feature",
			"repo": {"full_name": "acme/widgets"}}, "base": {"ref": "main",
			"repo": {"full_name": "acme/widgets"}}},
		"repository": {"full_name": "acme/widgets"}, "sender": {"login": "dev"}}`))
}

// A run that matches no workflow never fires OnRunTerminal, so its check is
// completed before the webhook returns, on push and pull_request alike.
func TestZeroMatchRunCompletesCheck(t *testing.T) {
	t.Run("push", func(t *testing.T) {
		fx := newRerunFixture(t, prWorkflow) // pull_request only: a push matches nothing
		if err := pushTo(t, fx, "sha-1"); err != nil {
			t.Fatalf("push: %v", err)
		}
		assertOneCompletion(t, fx, forge.ConclusionSuccess)
	})
	t.Run("pull_request", func(t *testing.T) {
		fx := newRerunFixture(t, pushWorkflow) // push only: a PR matches nothing
		if err := prTo(t, fx); err != nil {
			t.Fatalf("pull_request: %v", err)
		}
		assertOneCompletion(t, fx, forge.ConclusionSuccess)
	})
}

// failingLoader fails every load.
type failingLoader struct{}

func (failingLoader) LoadAll(context.Context, uuid.UUID, string) ([]*workflow.Workflow, error) {
	return nil, errors.New("store unavailable")
}

// A run whose workflows could not be loaded has no jobs and completes its
// check as a failure, not a success.
func TestLoadErrorRunCompletesCheckAsFailure(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)
	fx.c.cfg.WorkflowLoader = failingLoader{}
	if err := pushTo(t, fx, "sha-1"); err != nil {
		t.Fatalf("push: %v", err)
	}
	assertOneCompletion(t, fx, forge.ConclusionFailure)
}

// A re-run whose workflows could not be loaded is finalised as failed, as on
// push and pull_request, instead of staying pending, and its check is
// completed as a failure.
func TestRerunLoadErrorFinalisesRunAsFailed(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)
	if err := pushTo(t, fx, "sha-1"); err != nil {
		t.Fatalf("push: %v", err)
	}
	fx.c.cfg.WorkflowLoader = failingLoader{}
	if err := fx.c.HandleCheckSuite(decode[webhook.CheckSuitePayload](t, `{"action": "rerequested",
		"check_suite": {"id": 1, "head_sha": "sha-1"}, "repository": {"full_name": "acme/widgets"}}`)); err != nil {
		t.Fatalf("HandleCheckSuite: %v", err)
	}

	runs := fx.runs(t)
	if len(runs) != 2 || runs[1].jobs != 0 {
		t.Fatalf("runs = %+v, want the push run plus a zero-job re-run", runs)
	}
	run, err := fx.st.GetRun(context.Background(), runs[1].id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != runFailed {
		t.Errorf("re-run status = %q, want %q", run.Status, runFailed)
	}
	if n := len(fx.forge.completes); n != 1 {
		t.Fatalf("forge.Complete calls = %d, want 1 (the re-run's check)", n)
	}
	if got := fx.forge.completes[0]; got.ref.RunID != runs[1].id.String() || got.conclusion != forge.ConclusionFailure {
		t.Errorf("completed run %s as %q, want run %s as %q",
			got.ref.RunID, got.conclusion, runs[1].id, forge.ConclusionFailure)
	}
}

// When the run already has a terminal status (CIC-85 finalises zero-job
// runs), that status decides the conclusion.
func TestZeroJobRunUsesTerminalStatus(t *testing.T) {
	fx := newRerunFixture(t, prWorkflow)
	if err := pushTo(t, fx, "sha-1"); err != nil {
		t.Fatalf("push: %v", err)
	}
	run := fx.runs(t)[0]
	if _, err := fx.st.Pool().Exec(context.Background(),
		`UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, run.id); err != nil {
		t.Fatalf("set run failed: %v", err)
	}
	fx.c.finishIfNoJobs(context.Background(), run.id, nil, false)

	if n := len(fx.forge.completes); n != 2 {
		t.Fatalf("forge.Complete calls = %d, want 2", n)
	}
	if got := fx.forge.completes[1].conclusion; got != forge.ConclusionFailure {
		t.Errorf("conclusion = %q, want %q (the run's own status)", got, forge.ConclusionFailure)
	}
}

func assertOneCompletion(t *testing.T, fx rerunFixture, want forge.Conclusion) {
	t.Helper()
	runs := fx.runs(t)
	if len(runs) != 1 || runs[0].jobs != 0 {
		t.Fatalf("runs = %+v, want one zero-job run", runs)
	}
	if len(fx.forge.completes) != 1 {
		t.Fatalf("forge.Complete calls = %d, want 1", len(fx.forge.completes))
	}
	got := fx.forge.completes[0]
	if got.checkID != 1 || got.ref.RunID != runs[0].id.String() {
		t.Errorf("completed check %d for run %s, want check 1 for run %s", got.checkID, got.ref.RunID, runs[0].id)
	}
	if got.conclusion != want {
		t.Errorf("conclusion = %q, want %q", got.conclusion, want)
	}
}
