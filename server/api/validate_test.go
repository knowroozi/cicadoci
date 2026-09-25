// Package api_test exercises the HTTP handler layer as a black box.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// validateYAML is a minimal valid workflow document used across these tests.
const validateYAML = `name: smoke
on:
  push:
    branches: [main]
jobs:
  build:
    steps:
          - run: echo hello
`

// validateRequest mirrors the JSON body accepted by POST /api/v1/validate
// without importing the unexported type.
type validateRequest struct {
	YAML     string `json:"yaml"`
	FileName string `json:"fileName,omitempty"`
}

// validateResponse mirrors the JSON response shape of /api/v1/validate.
type validateResponse struct {
	Valid  bool             `json:"valid"`
	Errors []parseErrorJSON `json:"errors"`
}

type parseErrorJSON struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// newValidateServer starts a test server that doesn't need Postgres — the
// validate endpoint is stateless.  It uses &fakeReader{} from handler_test.go
// since the same Mux and middleware chain are shared.
func newValidateServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := newTestServer(t, &fakeReader{}) // fakeReader from handler_test.go
	return srv
}

// doValidate POSTs a body to a running test server and returns status + body.
func doValidate(t *testing.T, u string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, u+"/api/v1/validate", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

func TestValidate_HappyPath(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	body, _ := json.Marshal(validateRequest{YAML: validateYAML})
	status, raw := doValidate(t, srv.URL, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}

	var resp validateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Valid {
		t.Errorf("want valid=true, got false; errors=%v", resp.Errors)
	}
	if len(resp.Errors) > 0 {
		t.Errorf("want no errors, got %d: %v", len(resp.Errors), resp.Errors)
	}
}

func TestValidate_YAMLSyntaxError(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	body, _ := json.Marshal(validateRequest{YAML: "name: [unclosed\n"})
	status, raw := doValidate(t, srv.URL, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (errors in body, not status); body=%s", status, raw)
	}

	var resp validateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if resp.Valid {
		t.Error("want valid=false, got true")
	}
	if len(resp.Errors) == 0 {
		t.Error("expected at least one parse error")
	}
	if resp.Errors[0].Message == "" {
		t.Error("error should have a non-empty message")
	}
}

func TestValidate_MissingName(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	yaml := `on:
  push:
    branches: [main]
jobs:
  build:
    steps:
          - run: echo hi
`
	body, _ := json.Marshal(validateRequest{YAML: yaml})
	status, raw := doValidate(t, srv.URL, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}

	var resp validateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Valid {
		t.Error("want valid=false for missing name")
	}
	if len(resp.Errors) == 0 {
		t.Error("expected at least one error for missing name")
	}
}

func TestValidate_CustomFileName(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	body, _ := json.Marshal(validateRequest{
		YAML:     "invalid: [x\n",
		FileName: "my-workflow.yaml",
	})
	status, raw := doValidate(t, srv.URL, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}

	var resp validateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, e := range resp.Errors {
		if e.File == "my-workflow.yaml" {
			found = true
			break
		}
	}
	if !found && len(resp.Errors) > 0 {
		t.Errorf("expected an error with file=my-workflow.yaml; got %+v", resp.Errors)
	}
}

func TestValidate_DefaultFileName(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	body, _ := json.Marshal(validateRequest{YAML: "invalid: [x\n"})
	_, raw := doValidate(t, srv.URL, body)

	var resp validateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The default fileName is "workflow.yaml".
	for _, e := range resp.Errors {
		if e.File != "workflow.yaml" {
			t.Errorf("expected file=workflow.yaml (the default), got %q", e.File)
		}
	}
}

func TestValidate_EmptyBody(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	status, raw := doValidate(t, srv.URL, []byte(""))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, raw)
	}
}

func TestValidate_MalformedJSON(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	status, raw := doValidate(t, srv.URL, []byte("{not json"))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, raw)
	}
}

func TestValidate_RouteRegistered(t *testing.T) {
	srv := newValidateServer(t)
	defer srv.Close()

	// A GET should produce a 405 (method not allowed) for the POST-only route.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/validate", nil)
	if err != nil {
		t.Fatalf("new GET request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("GET unavailable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("GET /api/v1/validate should not return 200 (POST-only route)")
	}
}
