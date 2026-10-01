package jobtoken_test

// Postgres-backed tests for the job token subsystem.
//
// Skipped when CICADO_TEST_DATABASE_URL is not set (via testdb.New).
// Run under the with-postgres harness to exercise them.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/jobtoken"
)

// seededRow is an org/repo/run/job tuple that satisfies the FKs on a
// job token.
type seededRow struct {
	OrgID  uuid.UUID
	RepoID uuid.UUID
	RunID  uuid.UUID
	JobID  uuid.UUID
}

// seedPG creates the tenancy rows a job token needs and returns their
// ids.
func seedPG(t *testing.T, pool *pgxpool.Pool) seededRow {
	t.Helper()
	ctx := context.Background()
	var s seededRow
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id`,
		"org-"+uuid.NewString()[:8]).Scan(&s.OrgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'cicadoci', $2) RETURNING id`,
		s.OrgID, "repo-"+uuid.NewString()[:8]).Scan(&s.RepoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event, ref, sha, actor)
		 VALUES ($1, 1, 'push', 'refs/heads/main', 'deadbeef', 'tester') RETURNING id`,
		s.RepoID).Scan(&s.RunID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO jobs (run_id, name, status, executor, required_tags, priority, env)
		 VALUES ($1, $2, 'queued', 'shell', ARRAY['shell'], 100, '{"CI":"true"}'::jsonb)
		 RETURNING id`,
		s.RunID, "job-"+uuid.NewString()[:8]).Scan(&s.JobID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return s
}

func claimsFor(s seededRow) jobtoken.Claims {
	return jobtoken.Claims{
		OrgID:  s.OrgID,
		RepoID: s.RepoID,
		RunID:  s.RunID,
		JobID:  s.JobID,
		Branch: "main",
		Issuer: "controller",
		Scopes: jobtoken.WithRead(jobtoken.ScopeWrite),
	}
}

// TestPostgres_IssueVerifyRevoke exercises the full mint → verify →
// revoke lifecycle on a real Postgres run, using freshly-seeded
// org/repo/run/job rows to satisfy the tokens table's FKs.
func TestPostgres_IssueVerifyRevoke(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	store := jobtoken.NewPostgres(st.Pool())

	s := seedPG(t, st.Pool())
	aud := jobtoken.NewMemoryAuditor()
	mgr, err := jobtoken.NewManager(store, aud)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	tok, err := mgr.Issue(ctx, claimsFor(s), time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok.Raw == "" {
		t.Fatal("issued token has empty raw")
	}

	// The token's record is in the store; its raw is not.
	got, err := mgr.Verify(ctx, tok.Raw, jobtoken.ScopeRead)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ID != tok.ID {
		t.Errorf("verify id = %s, want %s", got.ID, tok.ID)
	}
	if got.Claims.RepoID != s.RepoID || got.Claims.RunID != s.RunID || got.Claims.JobID != s.JobID {
		t.Errorf("verify returned wrong claims: %+v", got.Claims)
	}

	// Revoke and confirm it is refused.
	if err := mgr.Revoke(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = mgr.Verify(ctx, tok.Raw, jobtoken.ScopeRead)
	if !errors.Is(err, jobtoken.ErrRevoked) {
		t.Errorf("post-revoke verify = %v, want ErrRevoked", err)
	}
	if len(aud.Revocs) != 1 {
		t.Errorf("auditor recorded %d revokes, want 1", len(aud.Revocs))
	}

	// A second mint on the same job is independent.
	tok2, err := mgr.Issue(ctx, claimsFor(s), time.Hour)
	if err != nil {
		t.Fatalf("second issue: %v", err)
	}
	if tok2.ID == tok.ID {
		t.Error("second mint returned the same record id")
	}
	if _, err := mgr.Verify(ctx, tok2.Raw, jobtoken.ScopeRead); err != nil {
		t.Errorf("second token verify failed after first was revoked: %v, revocation must be per-record", err)
	}
}

func TestPostgres_ClaimsRoundTrip(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	store := jobtoken.NewPostgres(st.Pool())

	s := seedPG(t, st.Pool())
	claims := claimsFor(s)
	claims.Branch = "feature/foo"
	claims.Issuer = "agent:xyz"
	aud := jobtoken.NewMemoryAuditor()
	mgr, err := jobtoken.NewManager(store, aud)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	tok, err := mgr.Issue(ctx, claims, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	got, err := mgr.Verify(ctx, tok.Raw, jobtoken.ScopeRead)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Claims.Branch != "feature/foo" {
		t.Errorf("branch round-trip = %q, want feature/foo", got.Claims.Branch)
	}
	if got.Claims.Issuer != "agent:xyz" {
		t.Errorf("issuer round-trip = %q, want agent:xyz", got.Claims.Issuer)
	}
	if !got.HasScope(jobtoken.ScopeWrite) {
		t.Error("expected write scope to survive the round trip")
	}
}
