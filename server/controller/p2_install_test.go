package controller

import (
	"context"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/webhook"
)

// ── CIC-146 P2: push/PR payload stamps installation_id ──

// TestHandlePush_InstallsInstallationID verifies that a push payload with a
// non-zero Installation.ID causes HandlePush to stamp repos.installation_id.
func TestHandlePush_InstallsInstallationID(t *testing.T) {
	st := testdb.New(t)
	c := New(st, NoOpForge{}, Config{}, discardLogger)
	defer st.Close()

	ctx := context.Background()
	if _, _, err := st.ResolveRepo(ctx, "TestOrg", "myrepo", "main", ""); err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}

	payload := webhook.PushPayload{Ref: "refs/heads/main", After: "abc123"}
	payload.Repository.FullName = "TestOrg/myrepo"
	payload.Installation.ID = 12345

	if err := c.HandlePush(payload); err != nil {
		// Non-fatal: store update is fire-and-forget; log but don't fail.
		t.Logf("HandlePush non-fatal: %v", err)
	}

	// Verify installation_id was set on the repo row.
	var got string
	if err := st.Pool().QueryRow(ctx,
		`SELECT installation_id FROM repos WHERE owner = $1 AND name = $2`,
		"TestOrg", "myrepo").
		Scan(&got); err != nil {
		t.Fatalf("query installation_id: %v", err)
	}
	if got != "12345" {
		t.Errorf("installation_id = %q, want 12345", got)
	}
}

// TestHandlePullRequest_InstallsInstallationID verifies that a
// pull_request payload with a non-zero Installation.ID causes
// HandlePullRequest to stamp repos.installation_id.
func TestHandlePullRequest_InstallsInstallationID(t *testing.T) {
	st := testdb.New(t)
	c := New(st, NoOpForge{}, Config{}, discardLogger)
	defer st.Close()

	ctx := context.Background()
	if _, _, err := st.ResolveRepo(ctx, "TestOrg", "myrepo", "main", ""); err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}

	payload := webhook.PullRequestPayload{
		Action:       "opened",
		Installation: webhook.Installation{ID: 54321},
	}
	payload.PullRequest.Number = 7
	payload.Repository.FullName = "TestOrg/myrepo"

	if err := c.HandlePullRequest(payload); err != nil {
		t.Logf("HandlePullRequest non-fatal: %v", err)
	}

	var got string
	if err := st.Pool().QueryRow(ctx,
		`SELECT installation_id FROM repos WHERE owner = $1 AND name = $2`,
		"TestOrg", "myrepo").
		Scan(&got); err != nil {
		t.Fatalf("query installation_id: %v", err)
	}
	if got != "54321" {
		t.Errorf("installation_id = %q, want 54321", got)
	}
}

// TestHandlePush_ZeroInstallationIDIsNoOp verifies that a push payload with
// Installation.ID == 0 does not attempt to stamp the store (no-op).
func TestHandlePush_ZeroInstallationIDIsNoOp(t *testing.T) {
	st := testdb.New(t)
	c := New(st, NoOpForge{}, Config{}, discardLogger)
	defer st.Close()

	ctx := context.Background()
	// Pre-seed the repo with an existing installation_id (99999).
	if _, _, err := st.ResolveRepo(ctx, "TestOrg", "myrepo", "main", "99999"); err != nil {
		t.Fatalf("ResolveRepo: %v", err)
	}

	payload := webhook.PushPayload{Ref: "refs/heads/main", After: "abc123"}
	payload.Repository.FullName = "TestOrg/myrepo"
	// No Installation set — Installation.ID is the zero value (0).

	if err := c.HandlePush(payload); err != nil {
		t.Logf("HandlePush non-zero-installation returned: %v", err)
	}

	// N1: an existing installation_id must be preserved.
	var got string
	if err := st.Pool().QueryRow(ctx,
		`SELECT installation_id FROM repos WHERE owner = $1 AND name = $2`,
		"TestOrg", "myrepo").
		Scan(&got); err != nil {
		t.Fatalf("query installation_id: %v", err)
	}
	if got != "99999" {
		t.Errorf("existing installation_id = %q, want 99999 (preserved)", got)
	}
}
