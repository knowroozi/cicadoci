package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cicadoci.com/cicado/server/forge"
)

// TestGitHubForge_SatisfiesForgeInterface exercises the full
// forge.Forge lifecycle (Prepare/Start/Complete/CloneCredentialsForRun)
// through a *ChecksClient and *InstallationTokenMinter wired up via
// the two adapters, against a real HTTP server. This is the exact
// wiring path (forge.NewGitHubForge(reporterAdapter, minterAdapter))
// that CIC-28's original Reporter/TokenMinter definitions could never
// actually compile for (CIC-67).
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
		case strings.Contains(r.URL.Path, "/access_tokens"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_ADAPTER","expiration":"2030-01-01T00:00:00Z"}`))
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
	minter := &InstallationTokenMinter{
		Config: InstallationConfig{
			HTTPClient: srv.Client(),
			BaseURL:    srv.URL,
			Now:        func() time.Time { return time.Unix(1000, 0) },
		},
		Auth: func(_ context.Context, _ InstallationConfig) (string, error) { return "app-jwt", nil },
	}
	resolver := func(org, repo string) (int64, error) {
		if org == "myorg" && repo == "myrepo" {
			return 55, nil
		}
		return 0, errors.New("no installation for " + org + "/" + repo)
	}

	f := forge.NewGitHubForge(
		NewChecksReporterAdapter(checksClient),
		NewInstallationTokenMinterAdapter(minter, resolver),
	)

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

	cloneURL, err := f.CloneCredentialsForRun(context.Background(), ref)
	if err != nil {
		t.Fatalf("CloneCredentialsForRun: %v", err)
	}
	if !strings.Contains(cloneURL, "ghs_ADAPTER@github.com/myorg/myrepo.git") {
		t.Fatalf("cloneURL = %q, want embedded ghs_ADAPTER token", cloneURL)
	}

	// Prepare (POST check-runs), Start (PATCH check-runs/7),
	// Complete (PATCH check-runs/7), CloneCredentialsForRun (POST access_tokens).
	if len(gotPaths) != 4 {
		t.Fatalf("got %d HTTP calls, want 4: %v", len(gotPaths), gotPaths)
	}
}

// TestInstallationTokenMinterAdapter_ResolverError surfaces a resolver
// failure without ever reaching the network.
func TestInstallationTokenMinterAdapter_ResolverError(t *testing.T) {
	minter := &InstallationTokenMinter{}
	adapter := NewInstallationTokenMinterAdapter(minter, func(_, _ string) (int64, error) {
		return 0, errors.New("unknown installation")
	})
	_, err := adapter.CloneCredentialsForRun(context.Background(), forge.RunRef{Org: "o", Repo: "r"})
	if err == nil || !strings.Contains(err.Error(), "unknown installation") {
		t.Fatalf("err = %v, want wrapped 'unknown installation'", err)
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

var (
	_ forge.Reporter    = (*ChecksReporterAdapter)(nil)
	_ forge.TokenMinter = (*InstallationTokenMinterAdapter)(nil)
)
