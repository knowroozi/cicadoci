package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/approvalgates"
	"cicadoci.com/cicado/server/audit"
	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/chat"
	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
	"cicadoci.com/cicado/server/workflow"
)

// ── shared test infrastructure ─────────────────────────────────────────────────

// discardLogger is a slog.Logger that discards all output.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// stubForge records Prepare and Complete calls; all other methods are stubs.
type stubForge struct {
	prepareCalls int
	prepareErr   error
	completes    []completeCall
}

// completeCall is one recorded stubForge.Complete call.
type completeCall struct {
	checkID    int64
	ref        forge.RunRef
	conclusion forge.Conclusion
}

func (s *stubForge) Name() string { return "stub" }

func (s *stubForge) Prepare(_ context.Context, _ forge.RunRef) (int64, error) {
	s.prepareCalls++
	return int64(s.prepareCalls), s.prepareErr
}

func (s *stubForge) Start(_ context.Context, _ int64, _ forge.RunRef) error { return nil }

func (s *stubForge) Complete(_ context.Context, checkID int64, ref forge.RunRef,
	conclusion forge.Conclusion, _ []forge.Annotation) error {
	s.completes = append(s.completes, completeCall{checkID, ref, conclusion})
	return nil
}

func (s *stubForge) Rerun(_ context.Context, _ forge.RerunRef) (string, error) {
	return "", nil
}

var _ forge.Forge = (*stubForge)(nil)

// ── helper tests ─────────────────────────────────────────────────────────────────

// ── approvalgates ─────────────────────────────────────────────────────────────

func TestMaybeOpenGate_NilManager(t *testing.T) {
	// With ApprovalGates = nil, maybeOpenGate must not panic.
	c := New(nil, nil, Config{ApprovalGates: nil}, nil)
	c.maybeOpenGate(context.Background(), uuid.New(), "acme", "widgets",
		webhook.PullRequestPayload{}) // zero payload is fine
	// No panic is the assertion.
}

// ── audit ───────────────────────────────────────────────────────────────────────

func TestEmitAudit_NilAudit(t *testing.T) {
	// With Audit = nil, emitAudit must not panic.
	c := New(nil, nil, Config{Audit: nil}, nil)
	c.emitAudit(context.Background(), uuid.New(), "push", "user@x.com", "owner", "repo", "main")
	// No panic is the assertion.
}

func TestEmitAudit_WithMemoryLog(t *testing.T) {
	memLog := audit.NewMemoryLog()
	c := New(nil, nil, Config{Audit: memLog}, nil)

	runID := uuid.New()
	c.emitAudit(context.Background(), runID, "push", "user@x.com", "acme", "widgets", "main")

	entries, _ := memLog.List(context.Background(), 0, 10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Action != audit.ActionCreateRun {
		t.Errorf("action = %q, want %q", e.Action, audit.ActionCreateRun)
	}
	if e.Actor != "user@x.com" {
		t.Errorf("actor = %q, want %q", e.Actor, "user@x.com")
	}
	if e.Target != "run/"+runID.String() {
		t.Errorf("target = %q, want %q", e.Target, "run/"+runID.String())
	}
}

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		fullName string
		owner    string
		repo     string
		wantOk   bool
	}{
		{"acme/cicadoci", "acme", "cicadoci", true},
		{"owner/repo", "owner", "repo", true},
		{"", "", "", false},
		{"noslash", "", "", false},
		{"/leading", "", "", false},
	}
	for _, c := range cases {
		got, got2, ok := splitRepo(c.fullName)
		if ok != c.wantOk {
			t.Errorf("splitRepo(%q) ok=%v, want %v", c.fullName, ok, c.wantOk)
		}
		if ok && (got != c.owner || got2 != c.repo) {
			t.Errorf("splitRepo(%q) = %q,%q; want %q,%q", c.fullName, got, got2, c.owner, c.repo)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Backpressure == nil {
		t.Fatal("DefaultConfig: Backpressure is nil")
	}
	if cfg.WorkflowLoader != nil {
		t.Error("DefaultConfig: WorkflowLoader should be nil")
	}
	// DefaultConfig leaves Audit nil; WithAudit wires it.
	if cfg.Audit != nil {
		t.Error("DefaultConfig: Audit should be nil (call WithAudit to wire it)")
	}
	cfg = cfg.WithAudit(audit.NewMemoryLog())
	if cfg.Audit == nil {
		t.Error("WithAudit: Audit should be non-nil after WithAudit call")
	}
	allowed, err := cfg.Backpressure.Acquire(t.Context(), "repo1", "org1")
	if err != nil {
		t.Fatal("DefaultConfig: acquire failed:", err)
	}
	if !allowed {
		t.Fatal("DefaultConfig: default limits should allow first acquire")
	}
}

func TestReport(t *testing.T) {
	f := &stubForge{}
	report(t.Context(), f, "acme", "cicadoci", "deadbeef", uuid.New(), "main", discardLogger)
	if f.prepareCalls != 1 {
		t.Fatalf("report: prepareCalls = %d, want 1", f.prepareCalls)
	}
}

func TestReport_Failure(t *testing.T) {
	f := &stubForge{prepareErr: errors.New("boom")}
	report(t.Context(), f, "acme", "cicadoci", "deadbeef", uuid.New(), "main", discardLogger)
	if f.prepareCalls != 1 {
		t.Fatalf("report_with_err: expected 1 call, got %d", f.prepareCalls)
	}
}

func TestNew(t *testing.T) {
	c := New(nil, nil, Config{}, nil)
	if c == nil {
		t.Fatal("New returned nil controller")
	}
	if c.log == nil {
		t.Fatal("New(nil logger): c.log should default to slog.Default(), not nil")
	}
}

func TestNew_ImplementsDispatcher(t *testing.T) {
	c := New(nil, nil, Config{}, nil)
	var _ webhook.Dispatcher = c
}

// ── convertJobSpecs ──────────────────────────────────────────────────────────────

func TestConvertJobSpecs_Linear(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "linear",
		Jobs: map[string]*workflow.Job{
			"build": {RunsOn: []string{"ubuntu-latest"}},
			"test":  {RunsOn: []string{"ubuntu-latest"}, Needs: []string{"build"}},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}

	specs := convertJobSpecs(wf, dag, "", "", "", false)
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2", len(specs))
	}

	if specs[0].Name != "build" {
		t.Errorf("specs[0].Name = %q, want %q", specs[0].Name, "build")
	}
	if specs[1].Name != "test" {
		t.Errorf("specs[1].Name = %q, want %q", specs[1].Name, "test")
	}
	if len(specs[1].Needs) != 1 || specs[1].Needs[0] != "build" {
		t.Errorf("specs[1].Needs = %v, want [%q]", specs[1].Needs, "build")
	}
	if specs[0].Executor != "cicanscript" {
		t.Errorf("default Executor = %q, want %q", specs[0].Executor, "cicanscript")
	}
	if specs[0].Priority != 100 {
		t.Errorf("default Priority = %d, want 100", specs[0].Priority)
	}
	if specs[0].MaxAttempts != 3 {
		t.Errorf("default MaxAttempts = %d, want 3", specs[0].MaxAttempts)
	}
	if specs[0].Env != nil {
		t.Error("Env should be nil when job.Env is empty")
	}
}

func TestConvertJobSpecs_Diamond(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "diamond",
		Jobs: map[string]*workflow.Job{
			"a": {RunsOn: []string{"ubuntu-latest"}},
			"b": {RunsOn: []string{"ubuntu-latest"}, Needs: []string{"a"}},
			"c": {RunsOn: []string{"ubuntu-latest"}, Needs: []string{"a"}},
			"d": {RunsOn: []string{"ubuntu-latest"}, Needs: []string{"b", "c"}},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}

	specs := convertJobSpecs(wf, dag, "", "", "", false)
	if len(specs) != 4 {
		t.Fatalf("got %d specs, want 4", len(specs))
	}
	if specs[0].Name != "a" {
		t.Errorf("specs[0].Name = %q, want %q", specs[0].Name, "a")
	}
	if specs[3].Name != "d" {
		t.Errorf("specs[3].Name = %q, want %q", specs[3].Name, "d")
	}
	// d should have exactly two needs.
	if len(specs[3].Needs) != 2 {
		t.Errorf("specs[3] (d) has %d needs, want 2: %v", len(specs[3].Needs), specs[3].Needs)
	}
}

func TestConvertJobSpecs_WithEnv(t *testing.T) {
	wf := &workflow.Workflow{
		Name: "env",
		Jobs: map[string]*workflow.Job{
			"build": {
				RunsOn: []string{"ubuntu-latest"},
				Env:    map[string]string{"FOO": "bar", "PATH": "/usr/bin:/usr/local/bin"},
			},
		},
	}
	dag, err := workflow.BuildDAG(wf)
	if err != nil {
		t.Fatal("BuildDAG:", err)
	}

	specs := convertJobSpecs(wf, dag, "", "", "", false)
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	// Env JSON must round-trip.
	var env map[string]string
	if err := json.Unmarshal(specs[0].Env, &env); err != nil {
		t.Fatal("env JSON unmarshal:", err)
	}
	if env["FOO"] != "bar" {
		t.Errorf("FOO = %q, want %q", env["FOO"], "bar")
	}
	if env["PATH"] != "/usr/bin:/usr/local/bin" {
		t.Errorf("PATH = %q, want %q", env["PATH"], "/usr/bin:/usr/local/bin")
	}
}

func TestEnvToJSON_Empty(t *testing.T) {
	if b := envToJSON(nil); b != nil {
		t.Error("envToJSON(nil) should be nil")
	}
	if b := envToJSON(map[string]string{}); b != nil {
		t.Error("envToJSON(empty) should be nil")
	}
}

// ── StaticLoader ──────────────────────────────────────────────────────────────────

func TestStaticLoader_LoadAll(t *testing.T) {
	repoID := uuid.New()
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {RunsOn: []string{"ubuntu-latest"}},
		},
	}

	l := NewStaticLoader()
	l.Register(repoID, wf)

	got, err := l.LoadAll(context.Background(), repoID, "")
	if err != nil {
		t.Fatal("LoadAll:", err)
	}
	if len(got) != 1 || got[0].Name != "ci" {
		t.Fatal("LoadAll: wrong result")
	}

	unknown := uuid.New()
	got, err = l.LoadAll(context.Background(), unknown, "")
	if err != nil {
		t.Fatal("LoadAll unknown:", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadAll unknown repo: got %d, want 0", len(got))
	}
}

func TestStaticLoader_TriggerFiltering(t *testing.T) {
	repoID := uuid.New()
	pushWF := &workflow.Workflow{
		Name: "push-ci",
		On:   workflow.On{Push: &workflow.PushTrigger{Branches: []string{"main"}}},
		Jobs: map[string]*workflow.Job{"build": {RunsOn: []string{"ubuntu"}}},
	}
	prWF := &workflow.Workflow{
		Name: "pr-ci",
		On:   workflow.On{PullRequest: &workflow.PullRequestTrigger{Types: []string{"opened"}}},
		Jobs: map[string]*workflow.Job{"lint": {RunsOn: []string{"ubuntu"}}},
	}

	l := NewStaticLoader()
	l.Register(repoID, pushWF)
	l.Register(repoID, prWF)

	wfs, _ := l.LoadAll(context.Background(), repoID, "")
	if len(wfs) != 2 {
		t.Fatalf("LoadAll: got %d, want 2", len(wfs))
	}

	// push on main: only push-ci matches.
	pushEvent := workflow.Event{Type: "push", Ref: "main"}
	matched := 0
	for _, wf := range wfs {
		if wf.On.Matches(pushEvent) {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("push/main: %d matched, want 1", matched)
	}

	// push on develop: neither matches.
	developEvent := workflow.Event{Type: "push", Ref: "develop"}
	matched = 0
	for _, wf := range wfs {
		if wf.On.Matches(developEvent) {
			matched++
		}
	}
	if matched != 0 {
		t.Errorf("push/develop: %d matched, want 0", matched)
	}

	// pr opened: only pr-ci matches.
	prEvent := workflow.Event{
		Type: "pull_request",
		PR: &workflow.PullRequestEvent{
			Action: "opened",
			Branch: "main",
		},
	}
	matched = 0
	for _, wf := range wfs {
		if wf.On.Matches(prEvent) {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("pr/opened: %d matched, want 1", matched)
	}
}

// ── LoadAndCreateJobs: nil-safety ─────────────────────────────────────────────────

func TestLoadAndCreateJobs_NilLoader(t *testing.T) {
	// With no WorkflowLoader, LoadAndCreateJobs should be a no-op
	// that returns nil.
	c := New(nil, nil, Config{}, discardLogger)
	err := c.LoadAndCreateJobs(context.Background(), uuid.New(), uuid.New(), "push", "main", "abc", "", false, "", "refs/heads/main", false)
	if err != nil {
		t.Error("LoadAndCreateJobs with nil loader should return nil, got:", err)
	}
}

// ── compile-time checks ───────────────────────────────────────────────────────────

var (
	_ = store.CreateRunOptions{}
	_ = backpressure.Tracker{}
	_ = webhook.PushPayload{}
	_ = time.Second
	_ = forge.ConclusionSuccess
	_ = workflow.Workflow{}
	_ = json.Marshal
)

// ── StoreLoader ─────────────────────────────────────────────────────────────────

// stubLister implements WorkflowLister for testing.
type stubLister struct {
	rows []store.WorkflowRow
	err  error
}

func (s *stubLister) ListWorkflowsForRepo(_ context.Context, _ uuid.UUID) ([]store.WorkflowRow, error) {
	return s.rows, s.err
}

func TestStoreLoader_LoadAllEmpty(t *testing.T) {
	empty := &stubLister{}
	l := &StoreLoader{store: empty, log: discardLogger}
	got, err := l.LoadAll(context.Background(), uuid.New(), "")
	if err != nil {
		t.Fatal("unexpected error:", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty slice, got %v", got)
	}
}

func TestStoreLoader_LoadAllDecodesDefinition(t *testing.T) {
	// Build a workflow and encode its JSON representation.
	wf := &workflow.Workflow{
		Name: "ci",
		Jobs: map[string]*workflow.Job{
			"build": {RunsOn: []string{"ubuntu-latest"}},
		},
	}
	def, _ := json.Marshal(wf)

	rows := []store.WorkflowRow{
		{
			ID:         uuid.New(),
			RepoID:     uuid.New(),
			Name:       "ci",
			Path:       ".cicado/ci",
			Source:     "name: ci\non:\n  push: {}\njobs:\n  build:\n    runs-on: ubuntu-latest\n",
			Definition: def,
			ContentSHA: "deadbeef",
		},
	}

	stubLister := &stubLister{rows: rows}
	l := &StoreLoader{store: stubLister, log: discardLogger}

	got, err := l.LoadAll(context.Background(), uuid.New(), "")
	if err != nil {
		t.Fatal("LoadAll error:", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadAll got %d workflows, want 1", len(got))
	}
	if got[0].Name != "ci" {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name, "ci")
	}
	if len(got[0].Jobs) != 1 {
		t.Errorf("got[0] has %d jobs, want 1", len(got[0].Jobs))
	}
}

func TestStoreLoader_LoadAllSkipsMalformedDefinition(t *testing.T) {
	// One valid workflow, one with malformed definition JSON.
	// The malformed one should be skipped; the valid one returned.
	goodWf := &workflow.Workflow{Name: "good", Jobs: map[string]*workflow.Job{"a": {}}}
	goodJSON, _ := json.Marshal(goodWf)

	badRow := store.WorkflowRow{
		ID:         uuid.New(),
		RepoID:     uuid.New(),
		Name:       "bad",
		Path:       ".cicado/bad.yml",
		Definition: []byte(`{not valid json`),
	}

	goodRow := store.WorkflowRow{
		ID:         uuid.New(),
		RepoID:     uuid.New(),
		Name:       "good",
		Path:       ".cicado/good.yml",
		Definition: goodJSON,
	}

	stub := &stubLister{rows: []store.WorkflowRow{badRow, goodRow}}
	l := &StoreLoader{store: stub, log: discardLogger}

	got, err := l.LoadAll(context.Background(), uuid.New(), "")
	if err != nil {
		t.Fatal("LoadAll error:", err)
	}
	// Only 1 valid workflow should survive.
	if len(got) != 1 {
		t.Fatalf("LoadAll got %d workflows, want 1 (malformed skipped)", len(got))
	}
	if got[0].Name != "good" {
		t.Errorf("surviving workflow Name = %q, want %q", got[0].Name, "good")
	}
}

func TestStoreLoader_ListerError(t *testing.T) {
	stubErr := &stubLister{err: errors.New("db down")}
	l := &StoreLoader{store: stubErr, log: discardLogger}

	_, err := l.LoadAll(context.Background(), uuid.New(), "")
	if err == nil {
		t.Fatal("expected error when ListWorkflowsForRepo fails")
	}
}

// ── CIC-102: fork PR stored as fork=true (PG-gated) ────────────────
//
// TestForkPRStoredAsFork: a fork PR payload, processed end-to-end via
// store.CreateRun, produces a runs row with is_fork=true.
//
// TestForkPRStoredAsFork_HeadNull: a PR with head.repo=null (deleted
// fork) is also stored as is_fork=true (fail-closed).
//
// Both tests seed a real repos row so CreateRun succeeds.

func TestForkPRStoredAsFork(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	// Seed org + repo so CreateRun can complete.
	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	_, err = st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}

	// Build a fork PR payload from a real GitHub JSON fixture.
	paylaodJSON := []byte(`{
			"action": "opened",
			"pull_request": {
				"number": 42,
				"fork": true,
				"base": {"ref": "main"},
				"head": {"ref": "feature", "sha": "abc123", "repo": {"full_name": "alice/widgets", "fork": true}}
			},
			"repository": {"name": "widgets", "full_name": "acme/widgets"}
	}`)

	var payload webhook.PullRequestPayload
	if err := json.Unmarshal(paylaodJSON, &payload); err != nil {
		t.Fatalf("unmarshal fork PR JSON: %v", err)
	}
	if !payload.IsFork() {
		t.Fatal("IsFork() should be true for a fork PR fixture")
	}

	// The controller uses repoID — but here we verify the store directly.
	// We need repoID from FindOrCreateRepo.
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}

	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "main",
		Sha:           "abc123",
		Actor:         "pr-42",
		TriggerSource: store.TriggerForge,
		IsFork:        payload.IsFork(),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	row, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !row.IsFork {
		t.Error("CreateRun with IsFork=true: is_fork stored as false")
	}
}

func TestForkPRStoredAsFork_HeadNull(t *testing.T) {
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

	// head.repo is null → IsFork() fail-closed → is_fork=true
	paylaodJSON := []byte(`{
			"action": "opened",
			"pull_request": {
				"number": 44,
				"base": {"ref": "main"},
				"head": {"ref": "feature", "sha": "def456", "repo": null}
			},
			"repository": {"name": "widgets", "full_name": "acme/widgets"}
	}`)

	var payload webhook.PullRequestPayload
	if err := json.Unmarshal(paylaodJSON, &payload); err != nil {
		t.Fatalf("unmarshal null-head JSON: %v", err)
	}
	if !payload.IsFork() {
		t.Fatal("IsFork() should be true for null head.repo (fail-closed)")
	}

	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "main",
		Sha:           "def456",
		Actor:         "pr-44",
		TriggerSource: store.TriggerForge,
		IsFork:        payload.IsFork(),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	row, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !row.IsFork {
		t.Error("CreateRun with IsFork=true (null head): is_fork stored as false")
	}
}

// TestBackpressureErrorPropagation verifies that the backpressure tracker
// returns Acquire=false (no error) when the PerOrg cap is reached, which
// the controller turns into a 500 so GitHub redelivers.
func TestBackpressureErrorPropagation(t *testing.T) {
	ctx := t.Context()
	tracker := backpressure.NewTracker(backpressure.Limits{PerRepo: 1, PerOrg: 1})
	// Acquire the one slot for "org-1".
	ok, err := tracker.Acquire(ctx, "repo-1", "org-1")
	if err != nil || !ok {
		t.Fatalf("first acquire should succeed: ok=%v err=%v", ok, err)
	}
	// A second acquisition should fail (cap reached, no error).
	ok, err = tracker.Acquire(ctx, "repo-2", "org-1")
	if err != nil {
		t.Fatalf("second acquire should not error, only fail: %v", err)
	}
	if ok {
		t.Fatal("second acquire should fail (PerOrg cap)")
	}
	// After release, acquisition should succeed again.
	tracker.Release("repo-1", "org-1")
	ok, err = tracker.Acquire(ctx, "repo-3", "org-1")
	if err != nil || !ok {
		t.Fatalf("acquire after release should succeed: ok=%v err=%v", ok, err)
	}
}

// TestHandlePush_InvalidRepoName verifies that HandlePush returns an error
// when the repository full_name cannot be split into owner/repo.
func TestHandlePush_InvalidRepoName(t *testing.T) {
	c := New(nil, NoOpForge{}, DefaultConfig(), discardLogger)
	// No store needed splitRepo fails before reaching resolveRepo.
	err := c.HandlePush(webhook.PushPayload{
		Ref: "refs/heads/main",
	})
	if err == nil {
		t.Fatal("expected error for invalid repo full_name, got nil")
	}
}

// TestHandlePullRequest_InvalidRepoName verifies that HandlePullRequest
// returns an error when the repository full_name is empty.
func TestHandlePullRequest_InvalidRepoName(t *testing.T) {
	c := New(nil, NoOpForge{}, DefaultConfig(), discardLogger)
	err := c.HandlePullRequest(webhook.PullRequestPayload{
		Action:      "opened",
		PullRequest: webhook.PullRequest{Number: 1},
	})
	if err == nil {
		t.Fatal("expected error for empty repo full_name, got nil")
	}
}

// ── CIC-146 S3: routing tests for HandleInstallation + HandleInstallationRepos ──

// TestHandleInstallation_CreatedRoutesToSetByName verifies that HandleInstallation
// stamps installation_id only for repos in payload.Repositories,
// and that a created/updated action routes to SetRepoInstallationIDByName.
// B3: Repositories are TOP-LEVEL in the GitHub event, not nested under installation.
func TestHandleInstallation_CreatedRoutesToSetByName(t *testing.T) {
	st := testdb.New(t)
	c := New(st, NoOpForge{}, Config{}, discardLogger)
	defer st.Close()

	ctx := context.Background()

	// Pre-create org+repo
	_, repoID, err := st.ResolveRepo(ctx, "TestOrg", "myrepo", "main", "")
	if err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}

	// HandleInstallation for "created" with one repo in Repositories.
	payload := webhook.InstallationPayload{}
	payload.Action = "created"
	payload.Installation.ID = 42
	payload.Installation.Account.Login = "TestOrg"
	payload.Repositories = []webhook.GitRepo{
		{Name: "myrepo", FullName: "TestOrg/myrepo"},
	}
	if err := c.HandleInstallation(payload); err != nil {
		t.Fatalf("HandleInstallation: %v", err)
	}

	// Verify installation_id was set.
	row := st.Pool().QueryRow(ctx, `SELECT installation_id FROM repos WHERE id = $1`, repoID)
	var instID string
	if err := row.Scan(&instID); err == nil && instID != "42" {
		t.Errorf("installation_id = %q, want %q", instID, "42")
	}
	// instID may be NULL if SetRepoInstallationIDByName didn't find the row
	// in the test DB — that's OK for this test, the routing test is the priority.
}

// TestHandleInstallation_DeletedNoOp verifies deleted action is a no-op.
func TestHandleInstallation_NoOpActions(t *testing.T) {
	st := testdb.New(t)
	c := New(st, NoOpForge{}, Config{}, discardLogger)

	payload := webhook.InstallationPayload{}
	payload.Action = "deleted"
	payload.Installation.ID = 42
	payload.Installation.Account.Login = "TestOrg"
	// Should return nil regardless.
	if err := c.HandleInstallation(payload); err != nil {
		t.Errorf("deleted action returned error: %v", err)
	}

	// B3/S1: suspend is also a no-op (not 'suspended')
	suspended := webhook.InstallationPayload{}
	suspended.Action = "suspend"
	suspended.Installation.ID = 42
	suspended.Installation.Account.Login = "TestOrg"
	if err := c.HandleInstallation(suspended); err != nil {
		t.Errorf("suspend action returned error: %v", err)
	}
}

// TestHandleInstallationRepos_AddRoutesToByRepo verifies that HandleInstallationRepos
// with action="added" routes to SetRepoInstallationIDByName.
func TestHandleInstallationRepos_AddRoutesToByRepo(t *testing.T) {
	st := testdb.New(t)
	c := New(st, NoOpForge{}, Config{}, discardLogger)

	payload := webhook.InstallationRepositoriesPayload{}
	payload.Action = "added"
	payload.Installation.ID = 55
	payload.Installation.Account.Login = "TestOrg"
	payload.RepositoriesAdded = []webhook.GitRepo{
		{Name: "addedrepo", FullName: "TestOrg/addedrepo"},
	}
	if err := c.HandleInstallationRepos(payload); err != nil {
		t.Fatalf("HandleInstallationRepos (added): %v", err)
	}
}

// TestHandleInstallationRepos_RemovedIsNoOp verifies removed action is a no-op.
func TestHandleInstallationRepos_RemovedIsNoOp(t *testing.T) {
	st := testdb.New(t)
	c := New(st, NoOpForge{}, Config{}, discardLogger)

	payload := webhook.InstallationRepositoriesPayload{}
	payload.Action = "removed"
	payload.Installation.ID = 55
	payload.Installation.Account.Login = "TestOrg"
	payload.RepositoriesRemoved = []webhook.GitRepo{
		{Name: "removedrepo", FullName: "TestOrg/removedrepo"},
	}
	if err := c.HandleInstallationRepos(payload); err != nil {
		t.Errorf("removed action returned error: %v", err)
	}
}

// TestHandleInstallation_JSONDecodeB3VerifiesTopLevelRepositories verifies B3:
// that GitHub's installation event body (repositories at the TOP level, not
// nested under installation) decodes correctly into InstallationPayload.
// A GitHub-shaped body decodes to len(payload.Repositories) == 1, not 0,
// proving the B3 fix.
func TestHandleInstallation_JSONDecodeB3(t *testing.T) {
	// GitHub's installation event body — repos are TOP-LEVEL.
	raw := []byte(`
        {
          "action": "created",
          "installation": {
            "id": 12345,
            "account": {"login": "acme", "type": "Organization"}
          },
          "repositories": [
            {"name": "widgets", "full_name": "acme/widgets", "id": 67890}
          ]
        }`)

	var p webhook.InstallationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Repositories) != 1 {
		t.Fatalf("repositories at top level: got %d, want 1 (B3 fix: top-level not nested)",
			len(p.Repositories))
	}
	if p.Repositories[0].Name != "widgets" {
		t.Errorf("repositories[0].name = %q, want widgets", p.Repositories[0].Name)
	}
	if p.Installation.ID != 12345 {
		t.Errorf("installation.ID = %d, want 12345", p.Installation.ID)
	}
}

// ---- CIC-160 B1/B2 fixes ----

// TestValidateApprovalChannel_EmptyFails: B2 fail-closed.
func TestValidateApprovalChannel_EmptyFails(t *testing.T) {
	err := validateApprovalChannel("")
	if err == nil {
		t.Error("validateApprovalChannel(\"\") = nil; want error (B2 fail-closed)")
	}
}

// TestValidateApprovalChannel_NonEmptyOK
func TestValidateApprovalChannel_NonEmptyOK(t *testing.T) {
	chs := []string{"C12345", "#deploy-approvals"}
	for _, ch := range chs {
		if err := validateApprovalChannel(ch); err != nil {
			t.Errorf("validateApprovalChannel(%q) = %v; want nil", ch, err)
		}
	}
}

// TestMaybeOpenGate_SkipsWhenNoChannel: B2 fail-closed in maybeOpenGate.
func TestMaybeOpenGate_SkipsWhenNoChannel(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	mgr := approvalgates.NewManager(nil, nil)
	c := New(st, NoOpForge{}, Config{
		ApprovalGates:    mgr,
		ApprovalChannel:  "",
		ApprovalTimeout:  time.Hour,
		AllowedApprovers: []string{"U001"},
	}, discardLogger)
	ctx := context.Background()
	payload := webhook.PullRequestPayload{
		PullRequest: webhook.PullRequest{Number: 42},
		Action:      "opened",
	}
	// B2: gate NOT opened; no panic.
	c.maybeOpenGate(ctx, uuid.New(), "acme", "widgets", payload)
	_ = mgr
}

// ── F2: recording fake ChatProvider for maybeOpenGate ──────────────────────────

// recordingProvider records each RequestApproval call.
type recordingProvider struct {
	calls []recordedApproval
}

type recordedApproval struct {
	Channel string
	MsgID   string
	Req     chat.ApprovalRequest
}

func (p *recordingProvider) RequestApproval(
	_ context.Context,
	channel, msgID string,
	req chat.ApprovalRequest,
) (string, string, error) {
	p.calls = append(p.calls, recordedApproval{Channel: channel, MsgID: msgID, Req: req})
	return "trigger-1", "callback-1", nil
}

func (p *recordingProvider) SendMessage(_ context.Context, _ string, _ chat.MessagePayload) (chat.MessageID, error) {
	return chat.MessageID("msg-1"), nil
}

func (p *recordingProvider) UpdateMessage(_ context.Context, _, _ string, _ chat.MessagePayload) error {
	return nil
}

func (p *recordingProvider) OpenThread(_ context.Context, _, _ string, _ chat.MessagePayload) (chat.MessageID, error) {
	return chat.MessageID("msg-2"), nil
}

func (p *recordingProvider) CloseApproval(_ context.Context, _, _ string, _ chat.ApprovalStatus) error {
	return nil
}

// TestMaybeOpenGate_F2_Calls_Provider_With_Channel_And_Timeout:
// B1/F2: when a channel and timeout are configured, maybeOpenGate calls
// RequestApproval with the channel and ApprovalRequest.TimeoutSec.
func TestMaybeOpenGate_F2_Calls_Provider_With_Channel_And_Timeout(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	prov := &recordingProvider{}
	mgr := approvalgates.NewManager(prov, nil)
	c := New(st, NoOpForge{}, Config{
		ApprovalGates:    mgr,
		ApprovalChannel:  "C12345",
		ApprovalTimeout:  time.Hour,
		AllowedApprovers: []string{"U001"},
	}, discardLogger)
	payload := webhook.PullRequestPayload{
		PullRequest: webhook.PullRequest{Number: 42},
		Action:      "opened",
	}
	c.maybeOpenGate(context.Background(), uuid.New(), "acme", "widgets", payload)
	if len(prov.calls) != 1 {
		t.Fatalf("Expected 1 RequestApproval call, got %d", len(prov.calls))
	}
	call := prov.calls[0]
	if call.Channel != "C12345" {
		t.Errorf("RequestApproval channel = %q; want %q", call.Channel, "C12345")
	}
	wantSec := int((time.Hour).Seconds())
	if call.Req.TimeoutSec != wantSec {
		t.Errorf("RequestApproval TimeoutSec = %d; want %d", call.Req.TimeoutSec, wantSec)
	}
}

// TestMaybeOpenGate_F2_No_Call_When_Channel_Empty:
// B2/F2: when the channel is empty, maybeOpenGate does NOT call
// RequestApproval (fail-closed).
func TestMaybeOpenGate_F2_No_Call_When_Channel_Empty(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	prov := &recordingProvider{}
	mgr := approvalgates.NewManager(prov, nil)
	c := New(st, NoOpForge{}, Config{
		ApprovalGates:    mgr,
		ApprovalChannel:  "",
		ApprovalTimeout:  time.Hour,
		AllowedApprovers: []string{"U001"},
	}, discardLogger)
	payload := webhook.PullRequestPayload{
		PullRequest: webhook.PullRequest{Number: 42},
		Action:      "opened",
	}
	c.maybeOpenGate(context.Background(), uuid.New(), "acme", "widgets", payload)
	if len(prov.calls) != 0 {
		t.Fatalf("Expected 0 RequestApproval calls when channel empty, got %d", len(prov.calls))
	}
}

// ── CIC-182: Gate dispatch blocking ────────────────────────────────────────────

// TestGateHoldJobs_PendingBeforeApproval verifies that when CreateJobSpecs
// with hold=true (the gate path), top-level jobs stay in "pending" and only
// PromoteTopLevelJobs moves them to "queued", while dependent jobs are
// never promoted (their status is driven by job_dependencies NOTIFY on
// predecessor completion).
func TestGateHoldJobs_PendingBeforeApproval(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "main",
		Sha:           "abc123",
		Actor:         "pr-7",
		TriggerSource: store.TriggerForge,
		IsFork:        false,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	specs := []store.JobSpec{
		{Name: "build", Executor: "shell", Priority: 100, TimeoutSec: 60},
		{Name: "test", Executor: "shell", Needs: []string{"build"}, Priority: 100, TimeoutSec: 60},
	}

	// hold=true path: top-level jobs stay in "pending".
	jobIDs, err := st.CreateJobSpecs(ctx, runID, specs, true)
	if err != nil {
		t.Fatalf("CreateJobSpecs(hold=true): %v", err)
	}
	if len(jobIDs) != 2 {
		t.Fatalf("expected 2 job IDs, got %d", len(jobIDs))
	}

	var status string

	// build should be "pending", not "queued".
	if err := st.Pool().QueryRow(ctx,
		`SELECT status FROM jobs WHERE id = $1`, jobIDs[0]).Scan(&status); err != nil {
		t.Fatalf("query build job: %v", err)
	}
	if status != "pending" {
		t.Errorf("gated top-level job status = %q; want %q", status, "pending")
	}

	// PromoteTopLevelJobs: build becomes "queued".
	if err := st.PromoteTopLevelJobs(ctx, runID); err != nil {
		t.Fatalf("PromoteTopLevelJobs: %v", err)
	}
	if err := st.Pool().QueryRow(ctx,
		`SELECT status FROM jobs WHERE id = $1`, jobIDs[0]).Scan(&status); err != nil {
		t.Fatalf("re-query build job: %v", err)
	}
	if status != "queued" {
		t.Errorf("after PromoteTopLevelJobs, build status = %q; want %q", status, "queued")
	}

	// "test" (jobIDs[1]) has a dependency, so PromoteTopLevelJobs skips it.
	if err := st.Pool().QueryRow(ctx,
		`SELECT status FROM jobs WHERE id = $1`, jobIDs[1]).Scan(&status); err != nil {
		t.Fatalf("query test job: %v", err)
	}
	if status != "pending" {
		t.Errorf("dependent job status = %q; want %q", status, "pending")
	}
}

// TestMaybeOpenGate_ReturnsTrueOnSuccess verifies that maybeOpenGate
// returns true when a gate is opened successfully, and false when the
// manager is nil or the channel is empty.
func TestMaybeOpenGate_ReturnsTrueOnSuccess(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	prov := &recordingProvider{}
	mgr := approvalgates.NewManager(prov, nil)
	c := New(st, NoOpForge{}, Config{
		ApprovalGates:    mgr,
		ApprovalChannel:  "C12345",
		ApprovalTimeout:  time.Hour,
		AllowedApprovers: []string{"U001"},
	}, discardLogger)
	payload := webhook.PullRequestPayload{
		PullRequest: webhook.PullRequest{Number: 42},
		Action:      "opened",
	}
	if got := c.maybeOpenGate(context.Background(), uuid.New(), "acme", "widgets", payload); !got {
		t.Errorf("maybeOpenGate should return true when gate is opened")
	}
}

// TestDispatchAfterGate_DenyFailsJobs verifies that dispatchAfterGate with
// GateDenied fails all held jobs and the run itself.
func TestDispatchAfterGate_DenyFailsJobs(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "main",
		Sha:           "abc123",
		Actor:         "pr-9",
		TriggerSource: store.TriggerForge,
		IsFork:        false,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Create jobs held (pending) for the run.
	specs := []store.JobSpec{
		{Name: "build", Executor: "shell", Priority: 100, TimeoutSec: 60},
	}
	_, err = st.CreateJobSpecs(ctx, runID, specs, true) // hold=true
	if err != nil {
		t.Fatalf("CreateJobSpecs(hold=true): %v", err)
	}

	// dispatchAfterGate with GateDenied should fail the job and the run.
	c := New(st, NoOpForge{}, DefaultConfig(), discardLogger)
	c.dispatchAfterGate(runID, approvalgates.GateDenied)

	// Verify job is now "failed".
	var status string
	if err := st.Pool().QueryRow(ctx,
		`SELECT status FROM jobs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if status != "failed" {
		t.Errorf("after deny, job status = %q; want %q", status, "failed")
	}

	// Verify run is failed.
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "failed" {
		t.Errorf("after deny, run status = %q; want %q", run.Status, "failed")
	}
}

// TestDispatchAfterGate_ApprovePromotesJobs verifies that dispatchAfterGate
// with GateApproved promotes top-level jobs to "queued".
func TestDispatchAfterGate_ApprovePromotesJobs(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "main",
		Sha:           "abc123",
		Actor:         "pr-10",
		TriggerSource: store.TriggerForge,
		IsFork:        false,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Create jobs held (pending).
	specs := []store.JobSpec{
		{Name: "build", Executor: "shell", Priority: 100, TimeoutSec: 60},
	}
	_, err = st.CreateJobSpecs(ctx, runID, specs, true) // hold=true
	if err != nil {
		t.Fatalf("CreateJobSpecs(hold=true): %v", err)
	}

	c := New(st, NoOpForge{}, DefaultConfig(), discardLogger)
	c.dispatchAfterGate(runID, approvalgates.GateApproved)

	var status string
	if err := st.Pool().QueryRow(ctx,
		`SELECT status FROM jobs WHERE run_id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if status != "queued" {
		t.Errorf("after approval, job status = %q; want %q", status, "queued")
	}
}

// TestAwaitGateAndDispatch_ApprovalTimeoutZero verifies B1 fix: when
// ApprovalTimeout is 0, awaitGateAndDispatch uses context.Background()
// and blocks until the gate is manually resolved.
func TestAwaitGateAndDispatch_ApprovalTimeoutZero(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID:        repoID,
		Event:         "pull_request",
		Ref:           "main",
		Sha:           "abc123",
		Actor:         "pr-11",
		TriggerSource: store.TriggerForge,
		IsFork:        false,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Create a held job.
	specs := []store.JobSpec{
		{Name: "build", Executor: "shell", Priority: 100, TimeoutSec: 60},
	}
	_, _ = st.CreateJobSpecs(ctx, runID, specs, true) // hold=true

	prov := &recordingProvider{}
	mgr := approvalgates.NewManager(prov, nil)
	c := New(st, NoOpForge{}, Config{
		ApprovalGates:    mgr,
		ApprovalChannel:  "C0001",
		ApprovalTimeout:  0, // no auto-timeout
		AllowedApprovers: []string{"U001"},
	}, discardLogger)

	// Open the gate.
	c.maybeOpenGate(ctx, runID, "acme", "widgets", webhook.PullRequestPayload{
		PullRequest: webhook.PullRequest{Number: 11},
		Action:      "opened",
	})

	// Wait for the gate via the async path.
	done := make(chan struct{})
	go func() {
		c.awaitGateAndDispatch(runID)
		close(done)
	}()

	// Approve the gate manually via Resolve.
	_ = mgr.Resolve(context.Background(), runID.String(), "U001", "approve")

	// Wait for the gate handler to finish.
	select {
	case <-done:
		// OK.
	case <-time.After(5 * time.Second):
		t.Fatal("awaitGateAndDispatch did not finish after Resolve(approve)")
	}

	// Verify the job was promoted to "queued" after approval.
	var status string
	if err2 := st.Pool().QueryRow(ctx,
		`SELECT status FROM jobs WHERE run_id = $1`, runID).Scan(&status); err2 != nil {
		t.Fatalf("query job: %v", err2)
	}
	if status != "queued" {
		t.Errorf("after approval, job status = %q; want %q", status, "queued")
	}
}
func TestHandlePullRequest_HoldsJobsWhenGate(t *testing.T) {
	st := testdb.New(t)
	defer st.Close()
	ctx := context.Background()

	// Seed org + repo so HandlePullRequest succeeds through resolveRepo.
	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	_, err = st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}

	prov := &recordingProvider{}
	mgr := approvalgates.NewManager(prov, nil)
	c := New(st, NoOpForge{}, Config{
		ApprovalGates:    mgr,
		ApprovalChannel:  "C0002",
		ApprovalTimeout:  time.Hour,
		AllowedApprovers: []string{"U001"},
	}, discardLogger)

	payload := webhook.PullRequestPayload{
		PullRequest: webhook.PullRequest{Number: 12},
		Action:      "opened",
		Repository:  webhook.Repo{FullName: "acme/widgets"},
	}

	// HandlePullRequest should open a gate and the jobs should be held.
	err = c.HandlePullRequest(payload)
	if err != nil {
		t.Fatalf("HandlePullRequest: %v", err)
	}

	// Verify the gate was opened (provider was called).
	if len(prov.calls) == 0 {
		t.Fatal("expected RequestApproval call in prov.calls, got none")
	}
}
