// service_minter_test.go: B3c + B4 — GetJobSecrets CloneMinter path tests.
//
// These tests prove the CloneMinter branch in GetJobSecrets is executed and
// the masker registration fires. Deleting the minter branch or the masker
// registration causes a test failure.
//
// The tests call Service.GetJobSecrets directly with a gRPC incoming metadata
// context carrying a runner credential, backed by a real Postgres DB (testdb).
// They require CICADO_TEST_DATABASE_URL to run.
package runnersvc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/metadata"

	"cicadoci.com/cicado/internal/testdb"
	runnerv1 "cicadoci.com/cicado/proto/runner/v1"
	"cicadoci.com/cicado/server/auth"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/redact"
	"cicadoci.com/cicado/server/runnersvc"
	"cicadoci.com/cicado/server/secrets"
)

// seedRun creates an org/repo/runner/job in the test database with the given
// fork status and secret_refs. Returns the job UUID, lease UUID, and runner
// credential raw value.
func seedRun(t *testing.T, pool *pgxpool.Pool, isFork bool, secretRefs []string) (jobID uuid.UUID, leaseID uuid.UUID, credRaw string) {
	t.Helper()
	ctx := context.Background()
	var orgID, repoID, runID, runnerID uuid.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO orgs (slug, name) VALUES ('b4org', 'B4') RETURNING id").Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO repos (org_id, owner, name, installation_id) VALUES ($1, 'o', 'r', '123456') RETURNING id", orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	event := "push"
	litFork := "false"
	if isFork {
		event, litFork = "pull_request", "true"
	}
	if err := pool.QueryRow(ctx, "INSERT INTO runs (repo_id, number, event, is_fork) VALUES ($1, 1, $2, $3) RETURNING id",
		repoID, event, litFork).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	cred, err := auth.Generate(auth.KindRunner)
	if err != nil {
		t.Fatalf("auth.Generate: %v", err)
	}
	credRaw = cred
	if err := pool.QueryRow(ctx,
		"INSERT INTO runners (org_id, name, labels, executors, os, arch, runner_version, features, status) "+
			"VALUES ($1, 'test-r', ARRAY[]::text[], ARRAY['shell'], 'linux', 'amd64', 'v0.1.0', ARRAY[]::text[], 'offline') "+
			"RETURNING id", orgID).Scan(&runnerID); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO tokens (kind, token_hash, org_id, runner_id, claims) "+
			"VALUES ('runner', $1, $2, $3, '{}'::jsonb)",
		auth.Hash(cred), orgID, runnerID); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	refsLit := "ARRAY[]::text[]"
	if len(secretRefs) > 0 {
		parts := make([]string, 0, len(secretRefs))
		for _, r := range secretRefs {
			parts = append(parts, "'"+r+"'")
		}
		refsLit = "ARRAY[" + joinStrings(parts, ", ") + "]"
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO jobs (run_id, name, status, lease_id, claimed_by, secret_refs) "+
			"VALUES ($1, 'main', 'claimed', gen_random_uuid(), $2, "+refsLit+") "+
			"RETURNING id, lease_id",
		runID, runnerID).Scan(&jobID, &leaseID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return
}

// joinStrings joins strings with a separator (test helper).
func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// TestCloneMinter_NonForkGetsToken: B4. A non-fork job with
// secret_refs=["github_clone_token"] and a CloneMinter configured returns
// the minted token via GetJobSecrets; masker registration fires.
func TestCloneMinter_NonForkGetsToken(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	const minterToken = "ghs_b4_nf_00000000000000000000000000000000000"
	called := false
	fakeMinter := func(_ context.Context, _, _ string, isFork bool) (string, error) {
		called = true
		if isFork {
			t.Error("non-fork: minter called with isFork=true")
			return "", context.Canceled
		}
		return minterToken, nil
	}
	rm := redact.NewRegisteredMasker()
	cfg := runnersvc.DefaultConfig()
	cfg.CloneMinter = fakeMinter
	cfg.Masker = rm
	svc := runnersvc.New(pool, q, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	jobID, leaseID, credRaw := seedRun(t, pool, false, []string{"github_clone_token"})
	cctx := metadata.NewIncomingContext(ctx,
		metadata.Pairs("authorization", "Bearer "+credRaw))
	resp, err := svc.GetJobSecrets(cctx, &runnerv1.GetJobSecretsRequest{
		JobId: jobID.String(), LeaseId: leaseID.String(),
	})
	if err != nil {
		t.Fatalf("GetJobSecrets: %v", err)
	}
	if !called {
		t.Error("B4: CloneMinter not called; deleting the branch would not be detected")
	}
	token, ok := resp.GetSecrets()["github_clone_token"]
	if !ok {
		t.Error("B4: github_clone_token NOT in response")
	}
	if token != minterToken {
		t.Errorf("B4: got token %q, want %q", token, minterToken)
	}
	// Masker registration check: deleting rm.Register in service.go
	// silently stops masking; this assertion catches that.
	masked := string(rm.Mask([]byte(minterToken)))
	if masked == minterToken {
		t.Error("B4: minter token not registered in server-side masker; deletion would be silent")
	}
}

// TestCloneMinter_MinterFailureOmitsToken: B3c. When the minter returns an
// error, the token is omitted and a gRPC error is returned (fail-closed).
func TestCloneMinter_MinterFailureOmitsToken(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	failMinter := func(_ context.Context, _, _ string, _ bool) (string, error) {
		return "", errors.New("GitHub API 500")
	}
	cfg := runnersvc.DefaultConfig()
	cfg.CloneMinter = failMinter
	cfg.Masker = redact.NewRegisteredMasker()
	svc := runnersvc.New(pool, q, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	jobID, leaseID, credRaw := seedRun(t, pool, false, []string{"github_clone_token"})
	cctx := metadata.NewIncomingContext(ctx,
		metadata.Pairs("authorization", "Bearer "+credRaw))
	_, err := svc.GetJobSecrets(cctx, &runnerv1.GetJobSecretsRequest{
		JobId: jobID.String(), LeaseId: leaseID.String(),
	})
	// B3c: minter error must NOT be swallowed.
	if err == nil {
		t.Error("B3c: minter error was swallowed; token should be omitted and error returned")
	}
}

// TestCloneMinter_ForkGetsToken: B1+B4a. A fork run with a minter set gets
// ONLY the token; a static secret in the same ref list is NOT returned and
// secrets.Get is never called.
func TestCloneMinter_ForkGetsToken(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	const minterToken = "ghs_b4_fork_00000000000000000000000000000000000"
	var getCalled bool
	fakeStore := &failingStore{fail: &getCalled}

	forkMinter := func(_ context.Context, _, _ string, isFork bool) (string, error) {
		if !isFork {
			t.Error("fork test: minter called with isFork=false")
		}
		return minterToken, nil
	}
	rm := redact.NewRegisteredMasker()
	cfg := runnersvc.DefaultConfig()
	cfg.CloneMinter = forkMinter
	cfg.Masker = rm
	svc := runnersvc.New(pool, q, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakeStore)

	// secret_refs includes both github_clone_token and a static secret.
	// B4a: the static secret must NOT appear in the response, and
	// secrets.Get must not be called for a fork run.
	jobID, leaseID, credRaw := seedRun(t, pool, true,
		[]string{"github_clone_token", "other_secret"})

	cctx := metadata.NewIncomingContext(ctx,
		metadata.Pairs("authorization", "Bearer "+credRaw))
	resp, err := svc.GetJobSecrets(cctx, &runnerv1.GetJobSecretsRequest{
		JobId: jobID.String(), LeaseId: leaseID.String(),
	})
	if err != nil {
		t.Fatalf("GetJobSecrets fork: %v", err)
	}
	// B1: github_clone_token must be present.
	token, ok := resp.GetSecrets()["github_clone_token"]
	if !ok {
		t.Error("B1: fork run did NOT get github_clone_token; fork carve-out not implemented")
	}
	if token != minterToken {
		t.Errorf("B1: fork got wrong token %q, want %q", token, minterToken)
	}
	// B4a: static secret must NOT appear in the fork response.
	if _, ok := resp.GetSecrets()["other_secret"]; ok {
		t.Error("B4a: static secret 'other_secret' returned for fork run; not enforced")
	}
	// B4a: secrets.Get must not have been called for a fork run.
	if getCalled {
		t.Error("B4a: secrets.Get was called for a fork run; fork security boundary violated")
	}
	// Masker registration check.
	masked := string(rm.Mask([]byte(minterToken)))
	if masked == minterToken {
		t.Error("B4: minter token not registered in masker; deletion would be silent")
	}
}

// TestCloneMinter_ForkNoMinter: B4b. A fork run with no minter and
// secret_refs including github_clone_token must return an empty secrets map.
// secrets.Get is never called for fork runs.
func TestCloneMinter_ForkNoMinter(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	// No minter — cfg.CloneMinter is nil from DefaultConfig.
	rm := redact.NewRegisteredMasker()
	cfg := runnersvc.DefaultConfig()
	cfg.Masker = rm

	var getCalled bool
	fakeStore := &failingStore{fail: &getCalled}
	svc := runnersvc.New(pool, q, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fakeStore)

	jobID, leaseID, credRaw := seedRun(t, pool, true,
		[]string{"github_clone_token", "other_secret"})

	cctx := metadata.NewIncomingContext(ctx,
		metadata.Pairs("authorization", "Bearer "+credRaw))
	resp, err := svc.GetJobSecrets(cctx, &runnerv1.GetJobSecretsRequest{
		JobId: jobID.String(), LeaseId: leaseID.String(),
	})
	if err != nil {
		t.Fatalf("GetJobSecrets fork no-minter: %v", err)
	}
	// B4b: fork + no minter → no secrets returned.
	if len(resp.GetSecrets()) != 0 {
		t.Error("B4b: fork run with no minter returned secrets; expected empty map")
	}
	if getCalled {
		t.Error("B4b: secrets.Get called for fork run; no-minter case should skip static secrets")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// failingStore implements secrets.Store. If Get is called it flips the
// bool pointed at by fail, so the test can assert that fork runs never
// invoke the static-secret store.
type failingStore struct {
	fail *bool
}

func (s *failingStore) Put(_ context.Context, _, _, _ string, _ []byte, _ []string) error {
	return nil
}

func (s *failingStore) Get(_ context.Context, _, _, _ string) (*secrets.Lease, error) {
	if s.fail != nil {
		*s.fail = true
		//nolint:staticcheck // test-only flag pointer deref
	}
	return nil, errors.New("Get called — fork security boundary violated")
}

func (s *failingStore) Delete(_ context.Context, _, _, _ string) error        { return nil }
func (s *failingStore) List(_ context.Context, _, _ string) ([]string, error) { return nil, nil }

// TestCloneMinter_B5OmitsToken: when the clone minter returns ("", nil)
// (B5 soft-skip: installation_id not set yet), the gRPC response does
// not include github_clone_token — it is omitted from the map.
func TestCloneMinter_B5OmitsToken(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()
	q := queue.NewPostgres(pool)

	// Minter returns ("", nil) — B5 soft-skip: no installation yet.
	cloneMinter := func(_ context.Context, _, _ string, _ bool) (string, error) {
		return "", nil
	}
	rm := redact.NewRegisteredMasker()
	cfg := runnersvc.DefaultConfig()
	cfg.CloneMinter = cloneMinter
	cfg.Masker = rm
	svc := runnersvc.New(pool, q, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	jobID, leaseID, credRaw := seedRun(t, pool, false, []string{"github_clone_token"})
	cctx := metadata.NewIncomingContext(ctx,
		metadata.Pairs("authorization", "Bearer "+credRaw))
	resp, err := svc.GetJobSecrets(cctx, &runnerv1.GetJobSecretsRequest{
		JobId: jobID.String(), LeaseId: leaseID.String(),
	})
	if err != nil {
		t.Fatalf("GetJobSecrets: %v", err)
	}
	if _, found := resp.GetSecrets()["github_clone_token"]; found {
		t.Error("B5 soft-skip: expected github_clone_token omitted when minter returns empty token")
	}
	t.Log("B5 soft-skip: github_clone_token correctly omitted when minter returns empty token")
}
