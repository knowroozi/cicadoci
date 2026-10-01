// Tests for WorkflowFetcher — two-phase GitHub Contents API fetch.
package github_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cicadoci.com/cicado/server/github"
)

func TestFetchWorkflowFiles_FindsYAML(t *testing.T) {
	ciYAML := []byte("name: CI\non:\n  push:\njobs:\n  build:\n    runs-on: ubuntu-latest\n")
	ciB64 := base64.StdEncoding.EncodeToString(ciYAML)

	// The directory listing has no "content" field; the per-file request does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing or wrong auth header: %q", r.Header.Get("Authorization"))
		}
		switch {
		case strings.Contains(r.URL.Path, "/contents/.github/workflows/"):
			// Per-file request — look at the path tail to know which file
			// was requested.
			if strings.HasSuffix(r.URL.Path, "/ci.yaml") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"name":"ci.yaml","path":".github/workflows/ci.yaml","type":"file","content":"` + ciB64 + `","sha":"abc"}`))
				return
			}
		case strings.Contains(r.URL.Path, "/contents/.github/workflows"):
			// Directory listing — no content field.
			w.Header().Set("Content-Type", "application/json")
			listing := `[{"name":"ci.yaml","path":".github/workflows/ci.yaml","type":"file","sha":"abc"},` +
				`{"name":"README.md","path":".github/workflows/README.md","type":"file","sha":"def"},` +
				`{"name":"subdir","path":".github/workflows/subdir","type":"dir"}]`
			_, _ = w.Write([]byte(listing))
		}
	}))
	defer srv.Close()

	fetcher := github.NewWorkflowFetcher("test-token", srv.Client())
	fetcher.BaseURL = srv.URL

	files, err := fetcher.FetchWorkflowFiles(context.Background(), "org", "repo", "abc123")
	if err != nil {
		t.Fatalf("FetchWorkflowFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].Path != ".github/workflows/ci.yaml" {
		t.Errorf("path = %q, want .github/workflows/ci.yaml", files[0].Path)
	}
	if !strings.Contains(string(files[0].Content), "CI") {
		t.Errorf("content = %q, want to contain CI", string(files[0].Content))
	}
}

func TestFetchWorkflowFiles_NoWorkflows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	fetcher := github.NewWorkflowFetcher("token", srv.Client())
	fetcher.BaseURL = srv.URL

	files, err := fetcher.FetchWorkflowFiles(context.Background(), "org", "repo", "deadbeef")
	if err != nil {
		t.Fatalf("404 on listing should not error, got: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestFetchWorkflowFiles_FileRemoved404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/.github/workflows"):
			// Listing succeeds.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"ci.yaml","path":".github/workflows/ci.yaml","type":"file","sha":"abc"}]`))
		case strings.Contains(r.URL.Path, "/ci.yaml"):
			// Per-file fetch: 404 (file removed between listing and fetch).
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	fetcher := github.NewWorkflowFetcher("token", srv.Client())
	fetcher.BaseURL = srv.URL

	files, err := fetcher.FetchWorkflowFiles(context.Background(), "org", "repo", "deadbeef")
	if err != nil {
		t.Fatalf("404 on per-file should not error, got: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected 0 files when per-file 404s, got %d", len(files))
	}
}

func TestFetchWorkflowFiles_BadRef500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"server error"}`))
	}))
	defer srv.Close()

	fetcher := github.NewWorkflowFetcher("token", srv.Client())
	fetcher.BaseURL = srv.URL

	_, err := fetcher.FetchWorkflowFiles(context.Background(), "org", "repo", "abc123")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}
