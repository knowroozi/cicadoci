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

	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
	"cicadoci.com/cicado/server/workflow"
)

// ── shared test infrastructure ─────────────────────────────────────────────────

// discardLogger is a slog.Logger that discards all output.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// stubForge records Prepare calls; all other methods are stubs.
type stubForge struct {
	prepareCalls int
	prepareErr   error
}

func (s *stubForge) Name() string { return "stub" }

func (s *stubForge) Prepare(_ context.Context, _ forge.RunRef) (int64, error) {
	s.prepareCalls++
	return int64(s.prepareCalls), s.prepareErr
}

func (s *stubForge) Start(_ context.Context, _ int64, _ forge.RunRef) error { return nil }

func (s *stubForge) Complete(_ context.Context, _ int64, _ forge.RunRef,
	_ forge.Conclusion, _ []forge.Annotation) error {
	return nil
}

func (s *stubForge) Rerun(_ context.Context, _ forge.RerunRef) (string, error) {
	return "", nil
}

func (s *stubForge) CloneCredentialsForRun(_ context.Context, _ forge.RunRef) (string, error) {
	return "", nil
}

var _ forge.Forge = (*stubForge)(nil)

// ── helper tests ─────────────────────────────────────────────────────────────────

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

	specs := convertJobSpecs(wf, dag)
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

	specs := convertJobSpecs(wf, dag)
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

	specs := convertJobSpecs(wf, dag)
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
	err := c.LoadAndCreateJobs(context.Background(), uuid.New(), uuid.New(), "push", "main", "abc", "", false)
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
