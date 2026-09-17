package store_test

import (
	"context"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
)

// Migrations must be idempotent and must leave a recorded version behind: a
// rolling deploy runs them from every instance that starts.
func TestMigrateIsIdempotent(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}

	version, err := st.Version(ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version < 1 {
		t.Errorf("schema version = %d, want at least 1", version)
	}
}

// The tables the architecture doc names as schema v1 must all exist. This is a
// spelling check against project-docs/02-architecture.md §2.
func TestSchemaV1Tables(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	want := []string{
		"orgs", "repos", "workflows", "runs", "jobs", "steps",
		"log_entries", "secrets", "runners", "tokens", "audit_events",
		"job_dependencies", "job_status_updates", "log_archives",
	}
	for _, table := range want {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = $1)`,
			table).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q is missing from schema v1", table)
		}
	}
}

// The lease-consistency constraint is load-bearing: a job in a claimed state
// without a lease would be executable with no authority to revoke.
func TestLeaseConsistencyConstraint(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	var orgID, repoID, runID string
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('acme', 'Acme') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name) VALUES ($1, 'acme', 'api') RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO runs (repo_id, number, event) VALUES ($1, 1, 'push') RETURNING id`,
		repoID).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	_, err := st.Pool().Exec(ctx,
		`INSERT INTO jobs (run_id, name, status) VALUES ($1, 'no-lease', 'claimed')`, runID)
	if err == nil {
		t.Fatal("inserted a claimed job with no lease; the consistency constraint is not enforcing")
	}
}
