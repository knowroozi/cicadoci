package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/controller"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
	"cicadoci.com/cicado/server/workflow"
)

// TestNewWorkflowProducer_EmptyTokenIsNil: no --github-workflow-token means no
// producer, as an untyped nil so the controller's `Producer != nil` gate is off.
func TestNewWorkflowProducer_EmptyTokenIsNil(t *testing.T) {
	if p := newWorkflowProducer("", nil, nil, nil); p != nil {
		t.Fatalf("empty token: producer = %#v, want nil", p)
	}
	if p := newWorkflowProducer("github_pat_x", nil, nil, nil); p == nil {
		t.Fatal("non-empty token: producer is nil")
	}
}

// redirectTransport sends every request to the test server, keeping path and
// query, so the producer's api.github.com URLs hit the fake GitHub.
type redirectTransport struct{ target *url.URL }

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

// TestWorkflowProducerWiring_DefaultBranchPushSyncs drives the CIC-120 wiring
// end to end: the producer main builds from the token, set on the controller
// config the way run() does, makes a default-branch push upsert the files
// present at the ref and delete the repo's row for a file that is gone, while
// another repo's row with the same path is untouched.
func TestWorkflowProducerWiring_DefaultBranchPushSyncs(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	const token = "github_pat_test"
	const ciYAML = "name: ci\non:\n  push: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo hi\"]\n"
	var authOK atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+token {
			authOK.Add(1)
		}
		if r.URL.Query().Get("ref") != "deadbeef" {
			http.Error(w, "wrong ref", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/contents/.github/workflows":
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"name": "ci.yaml", "path": ".github/workflows/ci.yaml", "type": "file"},
			})
		case "/repos/acme/widgets/contents/.github/workflows/ci.yaml":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name": "ci.yaml", "path": ".github/workflows/ci.yaml", "type": "file",
				"content": base64.StdEncoding.EncodeToString([]byte(ciYAML)),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	target, _ := url.Parse(gh.URL)

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	otherID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "other", "main")
	if err != nil {
		t.Fatalf("other repo: %v", err)
	}
	def, _ := json.Marshal(&workflow.Workflow{Name: "old"})
	for _, in := range []store.WorkflowInput{
		{RepoID: repoID, Name: "old-deploy", Path: ".github/workflows/old-deploy.yml", Definition: def},
		{RepoID: otherID, Name: "old-deploy", Path: ".github/workflows/old-deploy.yml", Definition: def},
	} {
		if _, err := st.UpsertWorkflow(ctx, in); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := controller.DefaultConfig()
	cfg.DefaultBranch = "main"
	cfg.WorkflowLoader = controller.NewStoreLoader(st, log)
	cfg.Producer = newWorkflowProducer(token, st, log,
		&http.Client{Transport: redirectTransport{target: target}})
	if cfg.Producer == nil {
		t.Fatal("producer is nil with a token set")
	}
	ctrl := controller.New(st, controller.NoOpForge{}, cfg, log)

	var push webhook.PushPayload
	if err := json.Unmarshal([]byte(`{
      "ref": "refs/heads/main",
      "after": "deadbeef",
      "head_commit": {"id": "deadbeef"},
      "repository": {"full_name": "acme/widgets", "default_branch": "main"},
      "pusher": {"email": "u@acme.com"}
    }`), &push); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := ctrl.HandlePush(push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	if authOK.Load() == 0 {
		t.Error("no request to GitHub carried the configured token")
	}
	rows, err := st.ListWorkflowsForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != ".github/workflows/ci.yaml" {
		var got []string
		for _, r := range rows {
			got = append(got, r.Path)
		}
		t.Fatalf("repo workflows = %v, want only ci.yaml (old-deploy.yml deleted)", got)
	}
	if !strings.Contains(rows[0].Source, "echo hi") {
		t.Errorf("ci.yaml source not stored: %q", rows[0].Source)
	}
	other, err := st.ListWorkflowsForRepo(ctx, otherID)
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 1 {
		t.Errorf("other repo workflows = %d, want 1 (untouched)", len(other))
	}
}

// TestWorkflowProducerWiring_ListingNotFoundKeepsRows: GitHub answers 404 on
// the .github/workflows listing for a private repo the fine-grained PAT cannot
// read, not only for a repo without workflows. A default-branch push that gets
// that 404 must not prune: every stored workflow row for the repo survives.
func TestWorkflowProducerWiring_ListingNotFoundKeepsRows(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	var listed atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/secret/contents/.github/workflows" {
			listed.Add(1)
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer gh.Close()
	target, _ := url.Parse(gh.URL)

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "secret", "main")
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	def, _ := json.Marshal(&workflow.Workflow{Name: "ci"})
	for _, in := range []store.WorkflowInput{
		{RepoID: repoID, Name: "ci", Path: ".github/workflows/ci.yaml", Definition: def},
		{RepoID: repoID, Name: "deploy", Path: ".github/workflows/deploy.yml", Definition: def},
	} {
		if _, err := st.UpsertWorkflow(ctx, in); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := controller.DefaultConfig()
	cfg.DefaultBranch = "main"
	cfg.WorkflowLoader = controller.NewStoreLoader(st, log)
	cfg.Producer = newWorkflowProducer("github_pat_test", st, log,
		&http.Client{Transport: redirectTransport{target: target}})
	ctrl := controller.New(st, controller.NoOpForge{}, cfg, log)

	var push webhook.PushPayload
	if err := json.Unmarshal([]byte(`{
      "ref": "refs/heads/main",
      "after": "deadbeef",
      "head_commit": {"id": "deadbeef"},
      "repository": {"full_name": "acme/secret", "default_branch": "main"},
      "pusher": {"email": "u@acme.com"}
    }`), &push); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := ctrl.HandlePush(push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	if listed.Load() == 0 {
		t.Fatal("producer never requested the workflow listing")
	}
	rows, err := st.ListWorkflowsForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("repo workflows after listing 404 = %d, want 2 (nothing pruned)", len(rows))
	}
}
