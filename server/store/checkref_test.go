package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/store"
)

// CIC-81: prepared_check_id is nullable. A run with no check must still read
// through the runs API, and RunCheckRef must report the check as absent
// rather than as ID 0.
func TestRunCheckRef(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	orgID, err := st.UpsertOrg(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	repoID, err := st.FindOrCreateRepo(ctx, orgID, "github", "acme", "widgets", "main")
	if err != nil {
		t.Fatalf("find or create repo: %v", err)
	}
	runID, _, err := st.CreateRun(ctx, store.CreateRunOptions{
		RepoID: repoID, Event: "push", Ref: "main", Sha: "abc123",
		Actor: "dev", TriggerSource: store.TriggerForge,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if _, err := st.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun on a run with NULL prepared_check_id: %v", err)
	}
	ref, err := st.GetRunCheckRef(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunCheckRef: %v", err)
	}
	if ref.CheckID.Valid {
		t.Errorf("CheckID = %+v before Prepare, want not Valid", ref.CheckID)
	}
	if ref.Owner != "acme" || ref.Repo != "widgets" || ref.SHA != "abc123" || ref.Ref != "main" {
		t.Errorf("ref = %+v, want acme/widgets abc123 main", ref)
	}

	if err := st.SetPreparedCheckID(ctx, runID, 4242); err != nil {
		t.Fatalf("SetPreparedCheckID: %v", err)
	}
	ref, err = st.GetRunCheckRef(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunCheckRef: %v", err)
	}
	if !ref.CheckID.Valid || ref.CheckID.Int64 != 4242 {
		t.Errorf("CheckID = %+v, want 4242", ref.CheckID)
	}

	if _, err := st.GetRunCheckRef(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetRunCheckRef(unknown) err = %v, want ErrNotFound", err)
	}
	if err := st.SetPreparedCheckID(ctx, uuid.New(), 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetPreparedCheckID(unknown) err = %v, want ErrNotFound", err)
	}
}
