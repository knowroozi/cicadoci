// clone_minter_httptest_test.go: B2+B4a — assert the GitHub API request
// body sent by NewCloneMinterFunc contains repositories:[name] and
// permissions:{contents:"read"}.
//
// B4a: the test goes through the REAL closure (via WithBaseURL +
// WithHTTPClient injectable in NewCloneMinterFunc), not a direct
// construct of github.InstallationTokenMinter.  This mutation-proves
// the per-repo scoping fix.
package runnersvc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/redact"
	"cicadoci.com/cicado/server/runnersvc"
)

// testPEMKey returns a PEM-encoded RSA private key for httptest.
func testPEMKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// TestB2_ThroughClosure: B4a — NewCloneMinterFunc with WithBaseURL
// + WithHTTPClient sends a POST to the httptest server with
// repositories:[b2repo] and permissions:{contents:read} in the body.
func TestB2_ThroughClosure(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	ctx := context.Background()

	var orgID, repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('b2org', 'B2 Org') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	const installationID = "1234"
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name, installation_id)
		VALUES ($1, 'b2o', 'b2repo', $2)
		RETURNING id`,
		orgID, installationID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// httptest server that captures the request body.
	var capturedBody []byte
	var gotRepo, gotPerm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/1234/access_tokens" {
			t.Errorf("expected path /app/installations/1234/access_tokens, got %s", r.URL.Path)
		}
		capturedBody, _ = io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(capturedBody, &body); err == nil {
			if repos, ok := body["repositories"].([]any); ok && len(repos) > 0 {
				if s, ok := repos[0].(string); ok {
					gotRepo = s
				}
			}
			if perm, ok := body["permissions"].(map[string]any); ok {
				gotPerm, _ = perm["contents"].(string)
			}
		}
		resp, _ := json.Marshal(map[string]any{"token": "ghs_b2_through", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(resp)
	}))
	defer srv.Close()

	// Build the real minter with WithBaseURL + WithHTTPClient.
	cloneMinter := runnersvc.NewCloneMinterFunc(
		ctx, st.Pool(), 42, testPEMKey(t),
		redact.NewRegisteredMasker(),
		runnersvc.WithBaseURL(srv.URL),
		runnersvc.WithHTTPClient(srv.Client()),
	)
	if cloneMinter == nil {
		t.Fatal("expected non-nil CloneMinterFunc")
	}

	// Call the closure.
	_, err := cloneMinter(ctx, orgID.String(), repoID.String(), false)
	if err != nil {
		t.Fatalf("cloneMinter: %v", err)
	}

	// Assert the request body contained the right values.
	if gotRepo != "b2repo" {
		t.Errorf("B2: request body repositories[0] = %q, want \"b2repo\" (body=%s)", gotRepo, string(capturedBody))
	}
	if gotPerm != "read" {
		t.Errorf("B2: request body permissions.contents = %q, want \"read\"\nbody=%s", gotPerm, string(capturedBody))
	}
}

// TestB2_B5NoInstallationID asserts the B5 soft-skip when
// installation_id is not set.  No HTTP request is made.
func TestB2_B5NoInstallationID(t *testing.T) {
	st := testdb.New(t)
	pool := st.Pool()
	ctx := context.Background()

	var orgID, repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('b5org', 'B5 Org') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name)
		VALUES ($1, 'b5o', 'b5repo')
		RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// No installation ID — the resolver returns "0".
	var reqHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cloneMinter := runnersvc.NewCloneMinterFunc(
		ctx, st.Pool(), 42, testPEMKey(t),
		redact.NewRegisteredMasker(),
		runnersvc.WithBaseURL(srv.URL),
		runnersvc.WithHTTPClient(srv.Client()),
	)
	if cloneMinter == nil {
		t.Fatal("expected non-nil CloneMinterFunc")
	}

	// B5: no installation_id => ("", nil), no HTTP request.
	token, err := cloneMinter(ctx, orgID.String(), repoID.String(), false)
	if err != nil {
		t.Fatalf("B5: expected nil error, got %v", err)
	}
	if token != "" {
		t.Fatalf("B5: expected empty token, got %q", token)
	}
	if reqHits != 0 {
		t.Fatalf("B5: expected zero HTTP requests, got %d", reqHits)
	}
}
