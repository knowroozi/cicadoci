package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
)

// CIC-81: forge re-run requests (check_suite / check_run "rerequested") and
// check completion on run terminal. These run against Postgres.

// rerunFixture is a controller over a real store with one repo, acme/widgets,
// and one workflow registered for it.
type rerunFixture struct {
	st     *store.Store
	c      *Controller
	forge  *stubForge
	repoID uuid.UUID
}

func newRerunFixture(t *testing.T, workflowYAML string) rerunFixture {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}
	wf, err := ParseWorkflowBytes([]byte(workflowYAML), "ci.yaml")
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	def, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("marshal workflow: %v", err)
	}
	if _, err := st.UpsertWorkflow(ctx, store.WorkflowInput{
		RepoID: repoID, Name: wf.Name, Path: ".github/workflows/ci.yaml",
		Source: workflowYAML, Definition: def, ContentSHA: "c0ffee",
	}); err != nil {
		t.Fatalf("upsert workflow: %v", err)
	}

	f := &stubForge{}
	cfg := DefaultConfig()
	cfg.WorkflowLoader = NewStoreLoader(st, discardLogger)
	return rerunFixture{st: st, c: New(st, f, cfg, discardLogger), forge: f, repoID: repoID}
}

// runRow is the subset of a runs row these tests assert on.
type runRow struct {
	id       uuid.UUID
	event    string
	ref      string
	isFork   bool
	jobs     int
	gitRef   string
	actor    string
	checkSet bool
}

// runs returns the repo's runs, oldest first.
func (fx rerunFixture) runs(t *testing.T) []runRow {
	t.Helper()
	rows, err := fx.st.Pool().Query(context.Background(), `
SELECT r.id, r.event, coalesce(r.ref, ''), r.is_fork, r.actor,
       r.prepared_check_id IS NOT NULL,
       (SELECT count(*) FROM jobs j WHERE j.run_id = r.id),
       coalesce((SELECT min(j.git_ref) FROM jobs j WHERE j.run_id = r.id), '')
  FROM runs r WHERE r.repo_id = $1 ORDER BY r.number`, fx.repoID)
	if err != nil {
		t.Fatalf("query runs: %v", err)
	}
	defer rows.Close()
	var out []runRow
	for rows.Next() {
		var r runRow
		if err := rows.Scan(&r.id, &r.event, &r.ref, &r.isFork, &r.actor, &r.checkSet, &r.jobs, &r.gitRef); err != nil {
			t.Fatalf("scan run: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return v
}

const pushWorkflow = "name: ci\non:\n  push: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo build\"]\n"
const prWorkflow = "name: ci\non:\n  pull_request: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo build\"]\n"

// A check_suite re-request replays the original push run: same event and
// ref, jobs created, and its own forge check prepared and recorded.
func TestCheckSuiteRerequestedReplaysPushRun(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)

	push := decode[webhook.PushPayload](t, `{"ref": "refs/heads/main", "after": "abc123",
		"repository": {"full_name": "acme/widgets"}, "pusher": {"email": "dev@acme.test"}}`)
	if err := fx.c.HandlePush(push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	suite := decode[webhook.CheckSuitePayload](t, `{"action": "rerequested",
		"check_suite": {"id": 7, "head_sha": "abc123", "head_branch": "main"},
		"repository": {"full_name": "acme/widgets"}}`)
	if err := fx.c.HandleCheckSuite(suite); err != nil {
		t.Fatalf("HandleCheckSuite: %v", err)
	}

	runs := fx.runs(t)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2 (original + re-run)", len(runs))
	}
	re := runs[1]
	if re.event != "push" || re.ref != "main" {
		t.Errorf("re-run event/ref = %q/%q, want push/main", re.event, re.ref)
	}
	if re.jobs != 1 {
		t.Errorf("re-run jobs = %d, want 1", re.jobs)
	}
	if re.gitRef != "refs/heads/main" {
		t.Errorf("re-run git_ref = %q, want refs/heads/main", re.gitRef)
	}
	if re.actor != "github-rerequest" {
		t.Errorf("re-run actor = %q, want github-rerequest", re.actor)
	}
	if !re.checkSet {
		t.Error("re-run has no prepared_check_id; RunCompleted could not complete its check")
	}
	if fx.forge.prepareCalls != 2 {
		t.Errorf("forge.Prepare calls = %d, want 2", fx.forge.prepareCalls)
	}
}

// A check_run re-request of a fork PR's run creates jobs (the replayed event
// carries a PR action, so pull_request triggers match) and stays a fork run
// checking out the same refs/pull/<N>/head.
func TestCheckRunRerequestedReplaysForkPRRun(t *testing.T) {
	fx := newRerunFixture(t, prWorkflow)

	pr := decode[webhook.PullRequestPayload](t, `{"action": "opened",
		"pull_request": {"number": 42, "base": {"ref": "main"},
			"head": {"ref": "feature", "sha": "f00d", "repo": {"full_name": "mallory/widgets"}}},
		"repository": {"full_name": "acme/widgets"}, "sender": {"login": "mallory"}}`)
	if err := fx.c.HandlePullRequest(pr); err != nil {
		t.Fatalf("HandlePullRequest: %v", err)
	}
	checkRun := decode[webhook.CheckRunPayload](t, `{"action": "rerequested",
		"check_run": {"id": 9, "head_sha": "f00d"},
		"repository": {"full_name": "acme/widgets"}}`)
	if err := fx.c.HandleCheckRun(checkRun); err != nil {
		t.Fatalf("HandleCheckRun: %v", err)
	}

	runs := fx.runs(t)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2 (original + re-run)", len(runs))
	}
	orig, re := runs[0], runs[1]
	if !orig.isFork || orig.jobs != 1 {
		t.Fatalf("original PR run: is_fork=%v jobs=%d, want true/1", orig.isFork, orig.jobs)
	}
	if !re.isFork {
		t.Error("SECURITY: re-run of a fork PR run is not a fork run")
	}
	if re.event != "pull_request" || re.ref != "main" {
		t.Errorf("re-run event/ref = %q/%q, want pull_request/main", re.event, re.ref)
	}
	if re.jobs != 1 {
		t.Errorf("re-run jobs = %d, want 1 (pull_request trigger must match)", re.jobs)
	}
	if re.gitRef != "refs/pull/42/head" {
		t.Errorf("re-run git_ref = %q, want refs/pull/42/head", re.gitRef)
	}
}

// A re-request for a commit CICaDo never ran creates nothing: there is no
// event or fork flag to replay, and guessing either is unsafe.
func TestRerequestUnknownCommitCreatesNoRun(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)

	suite := decode[webhook.CheckSuitePayload](t, `{"action": "rerequested",
		"check_suite": {"id": 7, "head_sha": "never-ran"}, "repository": {"full_name": "acme/widgets"}}`)
	if err := fx.c.HandleCheckSuite(suite); err != nil {
		t.Fatalf("HandleCheckSuite: %v", err)
	}
	if n := len(fx.runs(t)); n != 0 {
		t.Fatalf("runs = %d, want 0", n)
	}
}

// check_suite "requested" arrives for every push; HandlePush already made
// that run, so it must not create a second one.
func TestCheckSuiteRequestedIsIgnored(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)

	push := decode[webhook.PushPayload](t, `{"ref": "refs/heads/main", "after": "abc123",
		"repository": {"full_name": "acme/widgets"}}`)
	if err := fx.c.HandlePush(push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	suite := decode[webhook.CheckSuitePayload](t, `{"action": "requested",
		"check_suite": {"id": 7, "head_sha": "abc123"}, "repository": {"full_name": "acme/widgets"}}`)
	if err := fx.c.HandleCheckSuite(suite); err != nil {
		t.Fatalf("HandleCheckSuite: %v", err)
	}
	if n := len(fx.runs(t)); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// RunCompleted completes the check Prepare returned for the run, addressed
// to the run's repo and commit, with a conclusion from the run's state.
func TestRunCompletedCompletesPreparedCheck(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  forge.Conclusion
	}{
		{"succeeded", forge.ConclusionSuccess},
		{"failed", forge.ConclusionFailure},
		{"cancelled", forge.ConclusionCancelled},
	} {
		t.Run(tc.state, func(t *testing.T) {
			fx := newRerunFixture(t, pushWorkflow)
			push := decode[webhook.PushPayload](t, `{"ref": "refs/heads/main", "after": "abc123",
				"repository": {"full_name": "acme/widgets"}}`)
			if err := fx.c.HandlePush(push); err != nil {
				t.Fatalf("HandlePush: %v", err)
			}
			runID := fx.runs(t)[0].id

			if err := fx.c.RunCompleted(context.Background(), runID, tc.state); err != nil {
				t.Fatalf("RunCompleted: %v", err)
			}
			if len(fx.forge.completes) != 1 {
				t.Fatalf("forge.Complete calls = %d, want 1", len(fx.forge.completes))
			}
			got := fx.forge.completes[0]
			// stubForge.Prepare returns its call count, so the first run's check is 1.
			if got.checkID != 1 {
				t.Errorf("checkID = %d, want 1 (the ID Prepare returned)", got.checkID)
			}
			if got.ref.Org != "acme" || got.ref.Repo != "widgets" || got.ref.Sha != "abc123" ||
				got.ref.Branch != "main" || got.ref.RunID != runID.String() {
				t.Errorf("ref = %+v, want acme/widgets abc123 main %s", got.ref, runID)
			}
			if got.conclusion != tc.want {
				t.Errorf("conclusion = %q, want %q", got.conclusion, tc.want)
			}
		})
	}
}

// When Prepare failed there is no check to complete; RunCompleted must not
// call Complete with ID 0 (a guaranteed 404 on GitHub).
func TestRunCompletedSkipsRunWithoutCheck(t *testing.T) {
	fx := newRerunFixture(t, pushWorkflow)
	fx.forge.prepareErr = errors.New("github down")

	push := decode[webhook.PushPayload](t, `{"ref": "refs/heads/main", "after": "abc123",
		"repository": {"full_name": "acme/widgets"}}`)
	if err := fx.c.HandlePush(push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	run := fx.runs(t)[0]
	if run.checkSet {
		t.Fatal("prepared_check_id recorded although Prepare failed")
	}
	if err := fx.c.RunCompleted(context.Background(), run.id, "succeeded"); err != nil {
		t.Fatalf("RunCompleted: %v", err)
	}
	if n := len(fx.forge.completes); n != 0 {
		t.Fatalf("forge.Complete calls = %d, want 0", n)
	}
}
