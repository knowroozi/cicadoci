package store_test

import (
	"context"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/store"
)

// TestSetRepoInstallationIDByName creates the org+repo and sets the ID,
// then reads it back (S3: verify actual value was stored).
func TestSetRepoInstallationIDByName(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	err := st.SetRepoInstallationIDByName(ctx, "NewOrg", "newrepo", "777")
	if err != nil {
		t.Fatalf("SetRepoInstallationIDByName: %v", err)
	}

	// S3: read installation_id back and verify value.
	orgID, err := st.UpsertOrg(ctx, "NewOrg", "NewOrg")
	if err != nil {
		t.Fatalf("UpsertOrg: %v", err)
	}
	repoRow := st.Pool().QueryRow(ctx,
		`SELECT installation_id FROM repos WHERE org_id = $1 AND name = $2`,
		orgID, "newrepo")
	var instID string
	if err := repoRow.Scan(&instID); err != nil {
		var nullable *string
		if scanErr := repoRow.Scan(&nullable); scanErr != nil {
			t.Fatalf("SELECT installation_id: %v", err)
		}
		if nullable == nil {
			t.Error("installation_id is NULL, want '777'")
			return
		}
		instID = *nullable
	}
	if instID != "777" {
		t.Errorf("installation_id = %q, want %q", instID, "777")
	}
}

// compile-time check.
var _ store.Store
