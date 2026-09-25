package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPrepare_PostsQueuedCheck creates a check run in the "queued" state.
func TestPrepare_PostsQueuedCheck(t *testing.T) {
	var gotMethod, gotPath string
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ = readAll(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer srv.Close()

	c := &ChecksClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "tok", nil },
		Now:        func() time.Time { return time.Unix(1000, 0) },
	}
	id, err := c.Prepare(context.Background(), RunRef{Org: "o", Repo: "r", Sha: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/repos/o/r/check-runs" {
		t.Fatalf("path = %q, want /repos/o/r/check-runs", gotPath)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "cicado" {
		t.Fatalf("name = %v, want cicado", got["name"])
	}
	if got["status"] != string(StatusQueued) {
		t.Fatalf("status = %v, want queued", got["status"])
	}
	if got["head_sha"] != "abc" {
		t.Fatalf("head_sha = %v, want abc", got["head_sha"])
	}
}

// TestStart_PatchesInProgress updates a check run to "in_progress".
func TestStart_PatchesInProgress(t *testing.T) {
	var gotMethod, gotPath string
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &ChecksClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "tok", nil },
		Now:        func() time.Time { return time.Unix(2000, 0) },
	}
	err := c.Start(context.Background(), 42, RunRef{Org: "o", Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/repos/o/r/check-runs/42" {
		t.Fatalf("path = %q, want /repos/o/r/check-runs/42", gotPath)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != string(StatusInProgress) {
		t.Fatalf("status = %v, want in_progress", got["status"])
	}
	if _, ok := got["started_at"]; !ok {
		t.Fatal("started_at not set")
	}
}

// TestComplete_WithConclusionAndAnnotations posts a completed check
// with a failure conclusion and an annotation.
func TestComplete_WithConclusionAndAnnotations(t *testing.T) {
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &ChecksClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "tok", nil },
		Now:        func() time.Time { return time.Unix(3000, 0) },
	}
	annotations := []Annotation{
		{Path: "main.go", Line: 42, Message: "undefined variable", Level: "failure", Title: "Compile"},
	}
	err := c.Complete(context.Background(), 42, RunRef{Org: "o", Repo: "r"}, ConclusionFailure, annotations)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != string(StatusCompleted) {
		t.Fatalf("status = %v, want completed", got["status"])
	}
	if got["conclusion"] != string(ConclusionFailure) {
		t.Fatalf("conclusion = %v, want failure", got["conclusion"])
	}
	if _, ok := got["annotations"]; !ok {
		t.Fatal("annotations not set")
	}
	annosArr := got["annotations"].([]any)
	if len(annosArr) != 1 {
		t.Fatalf("want 1 annotation, got %d", len(annosArr))
	}
	anno := annosArr[0].(map[string]any)
	if anno["path"] != "main.go" {
		t.Fatalf("annotation path = %v, want main.go", anno["path"])
	}
	if anno["start_line"].(float64) != 42 {
		t.Fatalf("annotation start_line = %v, want 42", anno["start_line"])
	}
}

// TestComplete_NoAnnotations omits the annotations field when empty.
func TestComplete_NoAnnotations(t *testing.T) {
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &ChecksClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "tok", nil },
		Now:        func() time.Time { return time.Unix(3000, 0) },
	}
	err := c.Complete(context.Background(), 1, RunRef{Org: "o", Repo: "r"}, ConclusionSuccess, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["annotations"]; ok {
		t.Fatal("annotations should be omitted when empty")
	}
	if got["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success", got["conclusion"])
	}
}

// TestPrepare_HTTPError returns an error on non-201 status.
func TestPrepare_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message": "rate limited"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	c := &ChecksClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "bad", nil },
		Now:        time.Now,
	}
	_, err := c.Prepare(context.Background(), RunRef{Org: "o", Repo: "r", Sha: "s"})
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

// TestRerun_ReturnsEmptyInStub confirms the stub returns empty.
func TestRerun_ReturnsEmptyInStub(t *testing.T) {
	c := &ChecksClient{
		BaseURL:    "http://example.com",
		HTTPClient: &http.Client{},
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "tok", nil },
		Now:        time.Now,
	}
	id, err := c.Rerun(context.Background(), RerunRef{Org: "o", Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("runID = %q, want empty", id)
	}
}

// TestAuthFunc_Error surfaces auth errors.
func TestAuthFunc_Error(t *testing.T) {
	c := &ChecksClient{
		BaseURL:    "http://example.com",
		HTTPClient: &http.Client{},
		AuthFunc:   func(_ context.Context, _ RunRef) (string, error) { return "", ErrNoToken },
		Now:        time.Now,
	}
	_, err := c.Prepare(context.Background(), RunRef{Org: "o", Repo: "r", Sha: "s"})
	if err == nil {
		t.Fatal("expected auth error")
	}
}

var ErrNoToken = errors.New("no token")

func readAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body) //nolint:bodyclose
}
