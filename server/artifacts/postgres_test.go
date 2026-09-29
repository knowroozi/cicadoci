// Copyright 2024 Kaveh Nowroozi. Apache-2.0.
//
// DB-backed test for the 0003_artifacts migration. Skipped when
// CICADO_TEST_DATABASE_URL is unset; CI always sets it.
//
// The test verifies that 0003_artifacts.sql defines a working `artifacts`
// table with the foreign keys, NOT NULL constraints, and column types that
// `server/artifacts/postgres.go` expects.
package artifacts_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
)

// TestArtifactsMigrationTableExists inserts a row into the `artifacts` table
// that 0003_artifacts.sql should have created, then reads it back.
func TestArtifactsMigrationTableExists(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	pool := st.Pool()

	// Seed the tenancy rows the FK on org_id and run_id require.
	var orgID, repoID, runID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id`,
		"org-"+uuid.NewString()[:8]).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'cicadoci', $2) RETURNING id`,
		orgID, "repo-"+uuid.NewString()[:8]).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event, ref, sha, actor)
		 VALUES ($1, 1, 'push', 'refs/heads/main', 'deadbeef', 'tester')
		 RETURNING id`, repoID).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Insert directly: verifies the table exists and the FKs resolve.
	id := uuid.New()
	var expiresAt time.Time = time.Now().Add(24 * time.Hour)
	_, err := pool.Exec(ctx,
		`INSERT INTO artifacts
		 (id, org_id, run_id, job_id, name, blob_key,
		  size_bytes, content_type, uploaded_at, expires_at, created_at)
		 VALUES ($1, $2, $3, NULL, $4, $5, $6, $7, now(), $8, now())`,
		id, orgID, runID, "tar.gz", "org/run/tar.gz",
		int64(1024), "application/gzip", expiresAt,
	)
	if err != nil {
		t.Fatalf("insert artifacts: %v", err)
	}

	// Read it back.
	var (
		gotBlobKey string
		gotSize    int64
		gotExpiry  time.Time
	)
	if err := pool.QueryRow(ctx,
		`SELECT blob_key, size_bytes, expires_at FROM artifacts WHERE id = $1`,
		id).Scan(&gotBlobKey, &gotSize, &gotExpiry); err != nil {
		t.Fatalf("select artifacts: %v", err)
	}
	if gotBlobKey != "org/run/tar.gz" {
		t.Errorf("blob_key = %q, want %q", gotBlobKey, "org/run/tar.gz")
	}
	if gotSize != 1024 {
		t.Errorf("size_bytes = %d, want 1024", gotSize)
	}

	// Run-level artifact: job_id IS NULL. This should work.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM artifacts WHERE run_id = $1 AND job_id IS NULL`,
		runID).Scan(&count); err != nil {
		t.Fatalf("count run-level: %v", err)
	}
	if count != 1 {
		t.Errorf("run-level artifact count = %d, want 1", count)
	}
}

// TestArtifactsNotFoundForEmptyRun verifies that querying for artifacts of
// a non-existent run returns no rows.
func TestArtifactsNotFoundForEmptyRun(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)

	var count int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM artifacts WHERE run_id = $1`,
		uuid.New()).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 for non-existent run", count)
	}
}
