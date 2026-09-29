// Tests for the Postgres-backed secrets store.
//
// Skipped when CICADO_TEST_DATABASE_URL is not set.
package secrets_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/secrets"
	"cicadoci.com/cicado/server/store"
)

// TestPostgres_SecretsLifecycle exercises the Put → Get → Delete → Get
// lifecycle using a real Postgres database.
func TestPostgres_SecretsLifecycle(t *testing.T) {
	dsn := os.Getenv("CICADO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CICADO_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	orgID, repoID, cleanup := seedTestRepo(t, ctx, st)
	defer cleanup()

	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	defer key.Zero()

	ps := secrets.NewPostgres(st, key)

	// Put a repo-scoped secret.
	err = ps.Put(ctx, orgID.String(), repoID.String(), "API_KEY", []byte("s3cr3t-value"), []string{"prod"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get it back.
	lease, err := ps.Get(ctx, orgID.String(), repoID.String(), "API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(lease.Value) != "s3cr3t-value" {
		t.Fatalf("expected 's3cr3t-value', got %q", string(lease.Value))
	}
	if lease.Name != "API_KEY" {
		t.Fatalf("expected API_KEY, got %q", lease.Name)
	}
	lease.Close()

	// Missing secret returns an error.
	if _, err := ps.Get(ctx, orgID.String(), repoID.String(), "MISSING"); err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}

	// Delete it.
	if err := ps.Delete(ctx, orgID.String(), repoID.String(), "API_KEY"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Get should now fail.
	if _, err := ps.Get(ctx, orgID.String(), repoID.String(), "API_KEY"); err == nil {
		t.Fatal("expected error after delete, got nil")
	}
}

// TestPostgres_List verifies that List returns only names, not values,
// for secrets visible in the given org/repo scope.
func TestPostgres_List(t *testing.T) {
	dsn := os.Getenv("CICADO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CICADO_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	orgID, repoID, cleanup := seedTestRepo(t, ctx, st)
	defer cleanup()

	key, _ := secrets.GenerateKey()
	defer key.Zero()
	ps := secrets.NewPostgres(st, key)

	_ = ps.Put(ctx, orgID.String(), repoID.String(), "FOO", []byte("bar-baz"), nil)
	_ = ps.Put(ctx, orgID.String(), repoID.String(), "BAZ", []byte("qux"), nil)

	names, err := ps.List(ctx, orgID.String(), repoID.String())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) < 2 {
		t.Fatalf("expected at least 2 names, got %d", len(names))
	}
	for _, n := range names {
		if n == "bar-baz" || n == "qux" {
			t.Fatal("List exposed secret values instead of names")
		}
	}
}

// seedTestRepo creates a minimal org + repo with deterministic UUIDs so that
// the secrets table's foreign key constraints are satisfied.
func seedTestRepo(t *testing.T, ctx context.Context, st *store.Store) (orgID, repoID uuid.UUID, cleanup func()) {
	t.Helper()
	pool := st.Pool()
	orgID = uuid.New()
	repoID = uuid.New()

	if _, err := pool.Exec(ctx, `
	  INSERT INTO orgs (id, slug, name)
	  VALUES ($1, $2, $3)
	  ON CONFLICT (id) DO NOTHING`,
		orgID, "test-org-"+orgID.String(), "Test Org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	if _, err := pool.Exec(ctx, `
	  INSERT INTO repos (id, org_id, owner, name, default_branch)
	  VALUES ($1, $2, 'test-owner', 'test-repo', 'main')
	  ON CONFLICT (id) DO NOTHING`,
		repoID, orgID); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	cleanup = func() {
		_, _ = pool.Exec(ctx, `DELETE FROM secrets WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, repoID)
		_, _ = pool.Exec(ctx, `DELETE FROM orgs WHERE id = $1`, orgID)
	}

	return orgID, repoID, cleanup
}
