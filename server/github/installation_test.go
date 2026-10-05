package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cicadoci.com/cicado/server/redact"
)

// TestInstallationTokenMinter_Mint calls the GitHub API and returns
// a token from the 201 response.
func TestInstallationTokenMinter_Mint(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_ABC123","expiration":"2026-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()

	minter := &InstallationTokenMinter{
		Config: InstallationConfig{
			AppID:      42,
			Expiry:     10 * time.Minute,
			Now:        func() time.Time { return time.Unix(1000, 0) },
			HTTPClient: srv.Client(),
			BaseURL:    srv.URL,
		},
		Auth: func(ctx context.Context, cfg InstallationConfig) (string, error) {
			return "app-jwt-stub", nil
		},
	}
	tok, err := minter.Mint(context.Background(), 99, "myorg", "myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_ABC123" {
		t.Fatalf("token = %q, want ghs_ABC123", tok)
	}
	if !strings.HasPrefix(gotPath, "/app/installations/99/access_tokens") {
		t.Fatalf("path = %q, want /app/installations/99/access_tokens", gotPath)
	}
	if gotAuth != "Bearer app-jwt-stub" {
		t.Fatalf("auth = %q, want Bearer app-jwt-stub", gotAuth)
	}
	if !strings.Contains(string(gotBody), "expires_at") {
		t.Fatalf("body = %s, want contains expires_at", string(gotBody))
	}
}

// TestInstallationTokenMinter_Error surfaces non-201 errors.
func TestInstallationTokenMinter_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad creds"}`))
	}))
	defer srv.Close()

	minter := &InstallationTokenMinter{
		Config: InstallationConfig{
			AppID:      42,
			HTTPClient: srv.Client(),
			BaseURL:    srv.URL,
		},
		Auth: func(_ context.Context, _ InstallationConfig) (string, error) {
			return "bad", nil
		},
	}
	_, err := minter.Mint(context.Background(), 1, "org", "repo")
	if err == nil {
		t.Fatal("expected error on 401")
	}
}

// TestMint_ScopesTokenPerRepo pins the least-privilege requirement
// from PR #25's review: the POST /app/installations/{id}/access_tokens
// body must include "repositories": [repo] so GitHub scopes the
// minted token to a single repo rather than every repo the
// installation can see.
func TestMint_ScopesTokenPerRepo(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_scoped_ABC","expiration":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	m := &InstallationTokenMinter{
		Config: InstallationConfig{BaseURL: srv.URL, Now: func() time.Time { return time.Unix(1700000000, 0) }},
		Auth:   func(_ context.Context, _ InstallationConfig) (string, error) { return "app-jwt", nil },
	}
	_, err := m.Mint(context.Background(), 1, "myorg", "myrepo")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	repos, ok := body["repositories"]
	if !ok {
		t.Fatalf("body missing repositories: %s", gotBody)
	}
	// Type-assert to []string (Go unmarshals []any of strings).
	anyList, ok := repos.([]any)
	if !ok {
		t.Fatalf("repositories not an array: %T", repos)
	}
	if len(anyList) != 1 || anyList[0] != "myrepo" {
		t.Fatalf("repositories = %v, want [myrepo]", anyList)
	}
}

// TestMint_AuthNotConfigured pins that an unconfigured minter fails
// loudly rather than returning a fake token (PR #25 review).
func TestMint_AuthNotConfigured(t *testing.T) {
	m := &InstallationTokenMinter{Config: InstallationConfig{BaseURL: "http://127.0.0.1:1"}}
	_, err := m.Mint(context.Background(), 1, "o", "r")
	if err == nil {
		t.Fatal("want error when Auth is nil, got nil")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want 'not configured'", err)
	}
}

var _ = json.Marshal // keep json import for tests that may use it

// TestMint_RegistersTokenWithMasker pins that a freshly-minted token is
// registered with InstallationTokenMinter.Masker via RegisterWithVariants
// before Mint returns — the exact fix for CIC-74: the NoopMasker gap
// meant that even a correctly-wired *RegisteredMasker would not receive
// the token because RegisterWithVariants had zero non-test call sites.
func TestMint_RegistersTokenWithMasker(t *testing.T) {
	const token = "ghs_REGISTRATION_TEST_TOKEN"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expiration":"2026-01-01T00:00:00Z"}`, token)
	}))
	defer srv.Close()

	// A *RegisteredMasker that starts empty. After Mint, the token and its
	// encodings must be in the scrubber.
	masker := redact.NewRegisteredMasker()

	minter := &InstallationTokenMinter{
		Config: InstallationConfig{
			AppID:      42,
			Expiry:     10 * time.Minute,
			Now:        func() time.Time { return time.Unix(1000, 0) },
			HTTPClient: srv.Client(),
			BaseURL:    srv.URL,
		},
		Auth: func(_ context.Context, _ InstallationConfig) (string, error) {
			return "app-jwt-stub", nil
		},
		Masker: masker,
	}

	tok, err := minter.Mint(context.Background(), 99, "myorg", "myrepo")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok != token {
		t.Fatalf("Mint returned %q, want %q", tok, token)
	}

	// The raw token must be scrubbed (replaced with **REDACTED**).
	sc := masker.Scrub("prefix " + token + " suffix")
	if !strings.Contains(sc, "\u002a\u002aREDACTED\u002a\u002a") {
		t.Fatalf("Scrub did not redact raw token; got %q, want **REDACTED** in output", sc)
	}
	if strings.Contains(sc, token) {
		t.Fatalf("Scrub left raw token in output: %q", sc)
	}
}

// TestMint_NilMaskerDoesNotPanic pins that a nil Masker is safe —
// existing call sites without Masker configured still work.
func TestMint_NilMaskerDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"token":"ghs_NILMASKER","expiration":"2026-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()

	minter := &InstallationTokenMinter{
		Config: InstallationConfig{
			AppID:      42,
			Expiry:     10 * time.Minute,
			Now:        func() time.Time { return time.Unix(1000, 0) },
			HTTPClient: srv.Client(),
			BaseURL:    srv.URL,
		},
		Auth: func(_ context.Context, _ InstallationConfig) (string, error) {
			return "app-jwt-stub", nil
		},
		// Masker: nil — the registration call must be safely skipped.
	}
	tok, err := minter.Mint(context.Background(), 1, "o", "r")
	if err != nil {
		t.Fatalf("Mint with nil masker: %v", err)
	}
	if tok != "ghs_NILMASKER" {
		t.Fatalf("token = %q, want ghs_NILMASKER", tok)
	}
}
