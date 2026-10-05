package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cicadoci.com/cicado/server/forge"
)

// TestGitHubForge_SatisfiesForgeInterface exercises the full
// forge.Forge lifecycle (Prepare/Start/Complete) through a
// *ChecksClient wired via ChecksReporterAdapter, against a real
// HTTP server.
func TestGitHubForge_SatisfiesForgeInterface(t *testing.T) {
	var gotPaths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7}`))
		case strings.Contains(r.URL.Path, "/check-runs/"):
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	checksClient := &ChecksClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "tok", nil },
		Now:        func() time.Time { return time.Unix(1000, 0) },
	}

	f := forge.NewGitHubForge(NewChecksReporterAdapter(checksClient))

	ref := forge.RunRef{Org: "myorg", Repo: "myrepo", Sha: "abc123"}

	checkID, err := f.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if checkID != 7 {
		t.Fatalf("checkID = %d, want 7", checkID)
	}

	if err := f.Start(context.Background(), checkID, ref); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := f.Complete(context.Background(), checkID, ref, forge.ConclusionSuccess, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// 3 HTTP calls: POST check-runs, PATCH check-runs/7 (start), PATCH check-runs/7 (complete).
	if len(gotPaths) != 3 {
		t.Fatalf("got %d HTTP calls, want 3: %v", len(gotPaths), gotPaths)
	}
}

// TestChecksReporterAdapter_TranslatesRerun exercises the RerunRef path,
// the one Reporter method not covered by TestGitHubForge_SatisfiesForgeInterface.
func TestChecksReporterAdapter_TranslatesRerun(t *testing.T) {
	c := &ChecksClient{
		BaseURL:    "http://example.com",
		HTTPClient: &http.Client{},
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "tok", nil },
		Now:        time.Now,
	}
	adapter := NewChecksReporterAdapter(c)
	runID, err := adapter.Rerun(context.Background(), forge.RerunRef{Org: "o", Repo: "r", CheckName: "cicado"})
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("runID = %q, want empty (stub)", runID)
	}
}

var _ forge.Reporter = (*ChecksReporterAdapter)(nil)
