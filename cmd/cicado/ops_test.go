package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCmdSecretSet_SendsPutAndSucceeds drives `cicado secrets set` against a
// stub server and asserts it issues an authenticated PUT-shaped POST carrying
// the org/repo/name/value and the bearer token.
func TestCmdSecretSet_SendsPutAndSucceeds(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	var gotMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	err := cmdSecrets([]string{"set",
		"--server", srv.URL, "--token", "op-token",
		"--org", "o1", "--repo", "r1", "--name", "DB_PASSWORD", "--value", "s3cr3t"})
	if err != nil {
		t.Fatalf("cmdSecrets set: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/secrets" {
		t.Fatalf("routing = %s %s, want POST /api/v1/secrets", gotMethod, gotPath)
	}
	if gotAuth != "Bearer op-token" {
		t.Errorf("auth header = %q, want Bearer op-token", gotAuth)
	}
	for _, want := range []string{`"org_id":"o1"`, `"repo_id":"r1"`, `"name":"DB_PASSWORD"`, `"value":"s3cr3t"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body %s missing %q", gotBody, want)
		}
	}
}

// TestCmdSecretSet_RequiresFields pins the client-side validation: a missing
// org/name/value must error before any request is issued.
func TestCmdSecretSet_RequiresFields(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		hit = true
	}))
	defer srv.Close()

	err := cmdSecrets([]string{"set", "--server", srv.URL, "--org", "o1"}) // no name/value
	if err == nil {
		t.Fatal("cmdSecrets set: expected error for missing name/value, got nil")
	}
	if hit {
		t.Error("a request was issued despite a validation error")
	}
}

// TestCmdRunCancel_SendsCancel routes `cicado run cancel <jobID>` to the job
// cancel route with the reason and bearer token.
func TestCmdRunCancel_SendsCancel(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"active":true}`))
	}))
	defer srv.Close()

	err := cmdRun([]string{"cancel", "--server", srv.URL, "--token", "op-token",
		"--reason", "manual stop", "22222222-2222-2222-2222-222222222222"})
	if err != nil {
		t.Fatalf("cmdRun cancel: %v", err)
	}
	if gotPath != "/api/v1/jobs/22222222-2222-2222-2222-222222222222/cancel" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer op-token" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"reason":"manual stop"`) {
		t.Errorf("body = %q, want reason", gotBody)
	}
}

// TestCmdRunCancel_RequiresJobID pins that a missing job id is a client error.
func TestCmdRunCancel_RequiresJobID(t *testing.T) {
	if err := cmdRun([]string{"cancel", "--server", "http://unused"}); err == nil {
		t.Fatal("cmdRun cancel: expected error for missing job id")
	}
}

// TestCmdSecretSet_ControlCharValueStaysValidJSON is the J1 regression: a value
// containing a JSON-hostile control char (ESC 0x1b) must be json.Marshal-encoded
// so the request body is valid JSON the server can parse. (The old %q builder
// produced an unparseable body for such a value.)
func TestCmdSecretSet_ControlCharValueStaysValidJSON(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// A value with an ESC (0x1b) and a backslash: safe as JSON via json.Marshal.
	val := string([]rune{'p', 'a', 's', 0x1b, 's', 'w', '\\', 'z'})
	if err := cmdSecrets([]string{"set",
		"--server", srv.URL, "--token", "t",
		"--org", "o1", "--name", "DB_PASSWORD", "--value", val}); err != nil {
		t.Fatalf("cmdSecrets set: %v", err)
	}
	// The body must be parseable JSON whose value field decodes back to the raw value.
	var probe struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(gotBody), &probe); err != nil {
		t.Fatalf("body is not valid JSON: %v (body=%q)", err, gotBody)
	}
	if probe.Value != val {
		t.Errorf("value round-trip = %q, want %q", probe.Value, val)
	}
}

// TestCmdSecretSet_RequiresServer pins that --server (or CICADO_SERVER) is
// mandatory so the CLI never issues a request to a relative/empty URL.
func TestCmdSecretSet_RequiresServer(t *testing.T) {
	t.Setenv("CICADO_SERVER", "")
	if err := cmdSecrets([]string{"set", "--org", "o1", "--name", "N", "--value", "v"}); err == nil {
		t.Fatal("expected an error when --server is unset")
	}
}
