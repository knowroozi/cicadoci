// cic180_b1_test.go: B1 tests for CIC-180 — REAL runnersvc.NewCloneMinterFunc
// with httptest GitHub, proving per-call minting end-to-end.
//
// B1.1: two repos, different installation_ids, each push upserts its own
//
//	workflows; content fetch uses the matching per-install token.
//
// B1.2: refresh path — two pushes, each triggers a separate mint (2nd
//
//	access_tokens call, distinct token).
//
// B1.3: main wiring from App creds — covered by
//
//	TestWorkflowProducerWiring_MinterPathSyncs in
//	producer_wiring_test.go (drives newWorkflowProducer with MinterFunc).
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/controller"
	"cicadoci.com/cicado/server/redact"
	"cicadoci.com/cicado/server/runnersvc"
	"cicadoci.com/cicado/server/webhook"
)

func testPEMKey180(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// TestCIC180_B1_TwoReposSeparateInstallations: two repos in the same org
// with different installation_ids. Each push mints a per-install token
// and upserts only its own workflow. The content fetch handler only
// accepts the correct per-install token.
func TestCIC180_B1_TwoReposSeparateInstallations(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()

	var orgID, repoID1, repoID2 uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('acme', 'Acme') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name, installation_id)
		VALUES ($1, 'acme', 'widgets', '1111') RETURNING id`,
		orgID).Scan(&repoID1); err != nil {
		t.Fatalf("seed repo1: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name, installation_id)
		VALUES ($1, 'acme', 'gadgets', '2222') RETURNING id`,
		orgID).Scan(&repoID2); err != nil {
		t.Fatalf("seed repo2: %v", err)
	}

	ciYAML := "name: ci\non:\n  push: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo ci180\"]\n"
	buildYAML := "name: build\non:\n  push: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo build180\"]\n"

	var tokenForRepo1, tokenForRepo2 string
	var tokenMu sync.Mutex
	var minterCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/1111/access_tokens":
			minterCalls.Add(1)
			_, _ = io.ReadAll(r.Body)
			resp, _ := json.Marshal(map[string]any{
				"token":      "ghs_install_1111",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(resp)
		case "/app/installations/2222/access_tokens":
			minterCalls.Add(1)
			_, _ = io.ReadAll(r.Body)
			resp, _ := json.Marshal(map[string]any{
				"token":      "ghs_install_2222",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(resp)
		case "/repos/acme/widgets/contents/.cicado/workflows":
			tokenMu.Lock()
			tokenForRepo1 = r.Header.Get("Authorization")
			tokenMu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"name": "ci.yaml", "path": ".cicado/workflows/ci.yaml", "type": "file"},
			})
		case "/repos/acme/widgets/contents/.cicado/workflows/ci.yaml":
			tokenMu.Lock()
			tokenForRepo1 = r.Header.Get("Authorization")
			tokenMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name":    "ci.yaml",
				"path":    ".cicado/workflows/ci.yaml",
				"type":    "file",
				"content": base64.StdEncoding.EncodeToString([]byte(ciYAML)),
			})
		case "/repos/acme/gadgets/contents/.cicado/workflows":
			tokenMu.Lock()
			tokenForRepo2 = r.Header.Get("Authorization")
			tokenMu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"name": "build.yaml", "path": ".cicado/workflows/build.yaml", "type": "file"},
			})
		case "/repos/acme/gadgets/contents/.cicado/workflows/build.yaml":
			tokenMu.Lock()
			tokenForRepo2 = r.Header.Get("Authorization")
			tokenMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name":    "build.yaml",
				"path":    ".cicado/workflows/build.yaml",
				"type":    "file",
				"content": base64.StdEncoding.EncodeToString([]byte(buildYAML)),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)

	cloneMinter := runnersvc.NewCloneMinterFunc(
		ctx, pool, 42, testPEMKey180(t),
		redact.NewRegisteredMasker(),
		runnersvc.WithBaseURL(srv.URL),
		runnersvc.WithHTTPClient(srv.Client()),
	)
	if cloneMinter == nil {
		t.Fatal("expected non-nil CloneMinterFunc")
	}

	contentClient := &http.Client{Transport: redirectTransport{target: target}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	prod := newWorkflowProducer(cloneMinter, "", st, log, contentClient)
	if prod == nil {
		t.Fatal("expected non-nil producer with minter set")
	}

	ctrl := controller.New(st, controller.NoOpForge{},
		controller.Config{
			DefaultBranch:  "main",
			WorkflowLoader: controller.NewStoreLoader(st, log),
			Producer:       prod,
		}, log)

	var push1 webhook.PushPayload
	_ = json.Unmarshal([]byte(`{"ref":"refs/heads/main","after":"aaa111","head_commit":{"id":"aaa111"},"repository":{"full_name":"acme/widgets","default_branch":"main"},"pusher":{"email":"u@acme.com"}}`), &push1)
	if err := ctrl.HandlePush(push1); err != nil {
		t.Fatalf("HandlePush repo1: %v", err)
	}

	var push2 webhook.PushPayload
	_ = json.Unmarshal([]byte(`{"ref":"refs/heads/main","after":"bbb222","head_commit":{"id":"bbb222"},"repository":{"full_name":"acme/gadgets","default_branch":"main"},"pusher":{"email":"u@acme.com"}}`), &push2)
	if err := ctrl.HandlePush(push2); err != nil {
		t.Fatalf("HandlePush repo2: %v", err)
	}

	rows1, err := st.ListWorkflowsForRepo(ctx, repoID1)
	if err != nil {
		t.Fatalf("list repo1: %v", err)
	}
	if len(rows1) != 1 || rows1[0].Path != ".cicado/workflows/ci.yaml" {
		var got []string
		for _, r := range rows1 {
			got = append(got, r.Path)
		}
		t.Fatalf("repo1 workflows = %v, want [ci.yaml]", got)
	}

	rows2, err := st.ListWorkflowsForRepo(ctx, repoID2)
	if err != nil {
		t.Fatalf("list repo2: %v", err)
	}
	if len(rows2) != 1 || rows2[0].Path != ".cicado/workflows/build.yaml" {
		var got []string
		for _, r := range rows2 {
			got = append(got, r.Path)
		}
		t.Fatalf("repo2 workflows = %v, want [build.yaml]", got)
	}

	if n := minterCalls.Load(); n < 2 {
		t.Errorf("minter called %d times, want >= 2", n)
	}

	if got := tokenForRepo1; got != "Bearer ghs_install_1111" {
		t.Errorf("repo1 content fetch token = %q, want Bearer ghs_install_1111", got)
	}
	if got := tokenForRepo2; got != "Bearer ghs_install_2222" {
		t.Errorf("repo2 content fetch token = %q, want Bearer ghs_install_2222", got)
	}
}

// TestCIC180_B1_TokenRefreshAfterExpiry: two pushes for the same repo;
// each triggers a separate minter call (2nd access_tokens request,
// distinct token).
func TestCIC180_B1_TokenRefreshAfterExpiry(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()

	var orgID, repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('acme', 'Acme') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name, installation_id)
		VALUES ($1, 'acme', 'widgets', '3333') RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	var minterCalls atomic.Int32
	var tokens []string
	var muTest sync.Mutex

	ciYAML := "name: ci\non:\n  push: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo refresh180\"]\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/3333/access_tokens":
			n := minterCalls.Add(1)
			muTest.Lock()
			tok := "ghs_install3333_v" + strconv.FormatInt(int64(n), 10)
			tokens = append(tokens, tok)
			muTest.Unlock()
			_, _ = io.ReadAll(r.Body)
			resp, _ := json.Marshal(map[string]any{
				"token":      tok,
				"expires_at": time.Now().Add(time.Minute).Format(time.RFC3339),
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(resp)
		case "/repos/acme/widgets/contents/.cicado/workflows":
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"name": "ci.yaml", "path": ".cicado/workflows/ci.yaml", "type": "file"},
			})
		case "/repos/acme/widgets/contents/.cicado/workflows/ci.yaml":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name":    "ci.yaml",
				"path":    ".cicado/workflows/ci.yaml",
				"type":    "file",
				"content": base64.StdEncoding.EncodeToString([]byte(ciYAML)),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)

	cloneMinter := runnersvc.NewCloneMinterFunc(
		ctx, pool, 42, testPEMKey180(t),
		redact.NewRegisteredMasker(),
		runnersvc.WithBaseURL(srv.URL),
		runnersvc.WithHTTPClient(srv.Client()),
	)
	if cloneMinter == nil {
		t.Fatal("expected non-nil CloneMinterFunc")
	}

	contentClient := &http.Client{Transport: redirectTransport{target: target}}
	logC180 := slog.New(slog.NewTextHandler(io.Discard, nil))
	prod := newWorkflowProducer(cloneMinter, "", st, logC180, contentClient)
	if prod == nil {
		t.Fatal("expected non-nil producer with minter set")
	}

	ctrl := controller.New(st, controller.NoOpForge{},
		controller.Config{
			DefaultBranch:  "main",
			WorkflowLoader: controller.NewStoreLoader(st, logC180),
			Producer:       prod,
		}, logC180)

	pushBody1 := `{"ref":"refs/heads/main","after":"aaa111","head_commit":{"id":"aaa111"},"repository":{"full_name":"acme/widgets","default_branch":"main"},"pusher":{"email":"u@acme.com"}}`
	var push1 webhook.PushPayload
	_ = json.Unmarshal([]byte(pushBody1), &push1)
	if err := ctrl.HandlePush(push1); err != nil {
		t.Fatalf("first HandlePush: %v", err)
	}

	pushBody2 := `{"ref":"refs/heads/main","after":"bbb222","head_commit":{"id":"bbb222"},"repository":{"full_name":"acme/widgets","default_branch":"main"},"pusher":{"email":"u@acme.com"}}`
	var push2 webhook.PushPayload
	_ = json.Unmarshal([]byte(pushBody2), &push2)
	if err := ctrl.HandlePush(push2); err != nil {
		t.Fatalf("second HandlePush: %v", err)
	}

	if n := minterCalls.Load(); n < 2 {
		t.Errorf("minter called %d times, want >= 2", n)
	}
	muTest.Lock()
	defer muTest.Unlock()
	if len(tokens) < 2 {
		t.Fatalf("expected >= 2 distinct tokens, got %d: %v", len(tokens), tokens)
	}
	if tokens[0] == tokens[1] {
		t.Errorf("both tokens are %q, expected distinct per-call minting", tokens[0])
	}
}

// TestCIC180_B1_MinterBeatsPAT: when both a minter and a static PAT are
// passed, the minter wins.
func TestCIC180_B1_MinterBeatsPAT(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	// Set installation_id for this repo.
	if _, err := pool.Exec(ctx,
		`UPDATE repos SET installation_id = '4444' WHERE id = $1`, repoID); err != nil {
		t.Fatalf("set installation_id: %v", err)
	}

	const ciYAML = "name: ci\non:\n  push: {}\njobs:\n  build:\n    runs-on: shell\n    steps: [run: \"echo beats\"]\n"

	var gotContentToken string
	var contentMu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/4444/access_tokens":
			_, _ = io.ReadAll(r.Body)
			resp, _ := json.Marshal(map[string]any{
				"token":      "ghs_minter_beats_pat",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(resp)
		case "/repos/acme/widgets/contents/.cicado/workflows":
			contentMu.Lock()
			gotContentToken = r.Header.Get("Authorization")
			contentMu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"name": "ci.yaml", "path": ".cicado/workflows/ci.yaml", "type": "file"},
			})
		case "/repos/acme/widgets/contents/.cicado/workflows/ci.yaml":
			contentMu.Lock()
			gotContentToken = r.Header.Get("Authorization")
			contentMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{
				"name":    "ci.yaml",
				"path":    ".cicado/workflows/ci.yaml",
				"type":    "file",
				"content": base64.StdEncoding.EncodeToString([]byte(ciYAML)),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)

	cloneMinter := runnersvc.NewCloneMinterFunc(
		ctx, pool, 42, testPEMKey180(t),
		redact.NewRegisteredMasker(),
		runnersvc.WithBaseURL(srv.URL),
		runnersvc.WithHTTPClient(srv.Client()),
	)
	if cloneMinter == nil {
		t.Fatal("expected non-nil CloneMinterFunc")
	}

	contentClient := &http.Client{Transport: redirectTransport{target: target}}
	logBeats := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Pass BOTH a minter and a static PAT — minter wins.
	prod := newWorkflowProducer(cloneMinter, "ghp_static_pat_should_not_win", st, logBeats, contentClient)
	if prod == nil {
		t.Fatal("expected non-nil producer")
	}

	ctrl := controller.New(st, controller.NoOpForge{},
		controller.Config{
			DefaultBranch:  "main",
			WorkflowLoader: controller.NewStoreLoader(st, logBeats),
			Producer:       prod,
		}, logBeats)

	var push webhook.PushPayload
	_ = json.Unmarshal([]byte(`{"ref":"refs/heads/main","after":"aaa","head_commit":{"id":"aaa"},"repository":{"full_name":"acme/widgets","default_branch":"main"},"pusher":{"email":"u@acme.com"}}`), &push)
	if err := ctrl.HandlePush(push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	// The minter token should have been used, not the static PAT.
	contentMu.Lock()
	defer contentMu.Unlock()
	if gotContentToken != "Bearer ghs_minter_beats_pat" {
		t.Errorf("content fetch token = %q, want Bearer ghs_minter_beats_pat (minter must beat PAT)", gotContentToken)
	}
	if strings.Contains(gotContentToken, "ghp_static_pat") {
		t.Errorf("content fetch used the static PAT, not the minter token")
	}

	rows, err := st.ListWorkflowsForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != ".cicado/workflows/ci.yaml" {
		t.Fatalf("repo workflows = %d, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Source, "echo beats") {
		t.Errorf("source = %q, want echo beats", rows[0].Source)
	}
}
