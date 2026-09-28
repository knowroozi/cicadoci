// Package e2e: M3 exit-criterion test for GitHub integration.
//
// This is a genuine cross-package integration test: it exercises the
// real github.ChecksReporterAdapter and github.InstallationTokenMinterAdapter
// composed through forge.NewGitHubForge, registered in a forge.NewRegistry,
// then drives a full Prepare -> Start -> Complete -> CloneCredentialsForRun
// lifecycle against an httptest server.
//
// This is the test that PR #28 originally promised but couldn't deliver in
// its first form because server/github's concrete types did not yet satisfy
// server/forge's interfaces (fixed by CIC-67 / PR #49). It is not a duplicate
// of checks_test.go or fork_test.go: it tests the end-to-end wiring that
// those unit tests cannot.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cicadoci.com/cicado/server/forge"
	"cicadoci.com/cicado/server/github"
)

// TestM3_ForgeRegistry verifies:
//  1. github.ChecksReporterAdapter + github.InstallationTokenMinterAdapter
//     compose into a forge.Forge via forge.NewGitHubForge.
//  2. That composite Forge registers and looks up by name "github"
//     in a forge.NewRegistry.
//  3. The full lifecycle dispatches correct HTTP calls through the real
//     adapters to an httptest server, and the installation ID is resolved.
func TestM3_ForgeRegistry(t *testing.T) {
	var (
		paths  []string
		bodies []string
	)

	// Prepare uses POST /repos/myorg/myrepo/check-runs
	// Start/Complete use PATCH /repos/myorg/myrepo/check-runs/7
	// CloneCredentialsForRun uses POST /app/installations/55/access_tokens
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Body != nil {
			bodyBytes, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(bodyBytes))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.Method == http.MethodPost:
			// Prepare
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7}`))
		case strings.Contains(r.URL.Path, "/check-runs/") && r.Method == http.MethodPatch:
			// Start / Complete
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/access_tokens") && r.Method == http.MethodPost:
			// CloneCredentialsForRun
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_TEST_CLONE","expiration":"2030-01-01T00:00:00Z"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	checksClient := &github.ChecksClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		AuthFunc:   func(_ context.Context, _ github.RunRef) (string, error) { return "test-bearer", nil },
		Now:        func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	rep := github.NewChecksReporterAdapter(checksClient)

	minter := &github.InstallationTokenMinter{
		Config: github.InstallationConfig{
			HTTPClient: srv.Client(),
			BaseURL:    srv.URL,
			Now:        func() time.Time { return time.Unix(1_700_000_000, 0) },
		},
		Auth: func(_ context.Context, _ github.InstallationConfig) (string, error) {
			return "test-app-jwt", nil
		},
	}

	resolver := func(org, repo string) (int64, error) {
		if org == "myorg" && repo == "myrepo" {
			return 55, nil
		}
		return 0, context.DeadlineExceeded
	}
	mint := github.NewInstallationTokenMinterAdapter(minter, resolver)

	f := forge.NewGitHubForge(rep, mint)
	reg := forge.NewRegistry()
	reg.Register(f)

	got, ok := reg.Get("github")
	if !ok {
		t.Fatal("registry: github not found after Register")
	}
	if got.Name() != "github" {
		t.Fatalf("got name = %q, want github", got.Name())
	}

	ref := forge.RunRef{Org: "myorg", Repo: "myrepo", Sha: "abc123", RunID: "run-1"}

	checkID, err := got.Prepare(context.Background(), ref)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if checkID != 7 {
		t.Fatalf("Prepare returned checkID = %d, want 7", checkID)
	}

	if err := got.Start(context.Background(), checkID, ref); err != nil {
		t.Fatalf("Start: %v", err)
	}

	anns := []forge.Annotation{{Path: "main.go", Line: 42, Message: "boom", Level: "failure"}}
	if err := got.Complete(context.Background(), checkID, ref, forge.ConclusionSuccess, anns); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	cloneURL, err := got.CloneCredentialsForRun(context.Background(), ref)
	if err != nil {
		t.Fatalf("CloneCredentialsForRun: %v", err)
	}
	if !strings.Contains(cloneURL, "ghs_TEST_CLONE@github.com/myorg/myrepo.git") {
		t.Fatalf("cloneURL = %q, want embedded token ghs_TEST_CLONE", cloneURL)
	}

	// Verify the HTTP call sequence went through the correct adapter paths.
	// Prepare:     POST   /repos/myorg/myrepo/check-runs
	// Start:      PATCH  /repos/myorg/myrepo/check-runs/7
	// Complete:   PATCH  /repos/myorg/myrepo/check-runs/7
	// Clone:      POST   /app/installations/55/access_tokens
	expected := []string{
		"/repos/myorg/myrepo/check-runs",
		"/repos/myorg/myrepo/check-runs/7",
		"/repos/myorg/myrepo/check-runs/7",
		"/app/installations/55/access_tokens",
	}
	if len(paths) != len(expected) {
		t.Fatalf("got %d HTTP calls, want %d: %v", len(paths), len(expected), paths)
	}
	for i, want := range expected {
		if !strings.HasPrefix(paths[i], want) {
			t.Fatalf("call[%d] = %q, want prefix %q", i, paths[i], want)
		}
	}

	if len(bodies) < 3 {
		t.Fatalf("expected at least 3 request bodies, got %d", len(bodies))
	}
	var completeBody map[string]any
	if err := json.Unmarshal([]byte(bodies[2]), &completeBody); err != nil {
		t.Fatalf("Complete body unmarshal: %v", err)
	}
	if concl, ok := completeBody["conclusion"].(string); !ok || concl != "success" {
		t.Fatalf("Complete body conclusion = %v, want success", completeBody["conclusion"])
	}
}

// TestM3_ForgeRegistryEmpty confirms that an unregistered forge lookup
// returns (nil, false) without panicking.
func TestM3_ForgeRegistryEmpty(t *testing.T) {
	reg := forge.NewRegistry()
	_, ok := reg.Get("github")
	if ok {
		t.Fatal("empty registry should return false for any lookup")
	}
	if len(reg.Names()) != 0 {
		t.Fatalf("empty registry Names = %v, want empty", reg.Names())
	}
}

// TestM3_ResolverUnknownInstallation verifies that the minter adapter
// surfaces a resolver error early without making a network call.
func TestM3_ResolverUnknownInstallation(t *testing.T) {
	minter := &github.InstallationTokenMinter{}
	adapter := github.NewInstallationTokenMinterAdapter(minter, func(_, _ string) (int64, error) {
		return 0, context.Canceled
	})
	_, err := adapter.CloneCredentialsForRun(
		context.Background(),
		forge.RunRef{Org: "unknown", Repo: "repo"},
	)
	if err == nil {
		t.Fatal("expected error for unknown org/repo, got nil")
	}
	if !strings.Contains(err.Error(), "resolve installation id") {
		t.Fatalf("err = %q, want wrapped 'resolve installation id'", err.Error())
	}
}
