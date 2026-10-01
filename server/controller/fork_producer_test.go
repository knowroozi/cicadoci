// Regression tests for the fork-PR and branch-poisoning workflow producer
// guards (CIC-84 security).  These use a real store + controller (PG-gated
// via testdb.New, which skips when CICADO_TEST_DATABASE_URL is unset) so they
// exercise the full handle path, not a tautological predicate.
//
// Invariants:
//   - A fork PR must NEVER invoke the producer (attacker-controlled head SHA
//     would poison the shared workflow row).
//   - A feature-branch push must NEVER invoke the producer (a feature branch
//     carrying its own .github/workflows/ must not overwrite the shared row).
//   - A push to the default branch DOES invoke the producer.
//
// NOTE: the PullRequestPayload struct is currently flat (Fork, Base, Head,
// Repository are top-level fields); CIC-102 fixes the struct to match
// GitHub's nested pull_request payload.  The JSON below uses the flat shape
// that the CURRENT struct expects.
package controller_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/controller"
	"cicadoci.com/cicado/server/store"
	"cicadoci.com/cicado/server/webhook"
)

// recordingProducer implements controller.WorkflowProducer for testing.
type recordingProducer struct{ calls int }

func (r *recordingProducer) Produce(_ context.Context, _, _, _ string, _ uuid.UUID) error {
	r.calls++
	return nil
}

func seedRepo(t *testing.T, orgOwner string) *store.Store {
	t.Helper()
	st := testdb.New(t)
	ctx := context.Background()
	orgID, err := st.UpsertOrg(ctx, orgOwner, orgOwner+" Org")
	if err != nil {
		t.Fatalf("UpsertOrg: %v", err)
	}
	_, err = st.FindOrCreateRepo(ctx, orgID, "github", orgOwner, "widgets", "main")
	if err != nil {
		t.Fatalf("FindOrCreateRepo: %v", err)
	}
	return st
}

func TestForkPRProducerNeverInvoked(t *testing.T) {
	st := seedRepo(t, "acme")
	rec := &recordingProducer{}
	cfg := controller.DefaultConfig()
	cfg.Producer = rec
	ctrl := controller.New(st, controller.NoOpForge{}, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Flat PR payload (matches current struct; CIC-102 fixes the shape).
	raw := []byte(`{
      "action": "opened",
      "number": 1,
      "fork": true,
      "base": {"ref": "main"},
      "head": {"ref": "feature", "sha": "abc123"},
      "repository": {"full_name": "acme/widgets"}
      }`)

	var payload webhook.PullRequestPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := ctrl.HandlePullRequest(payload); err != nil {
		t.Fatalf("HandlePullRequest (fork) should not error: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("producer MUST NOT be called for a fork PR; calls=%d", rec.calls)
	}
}

func TestFeatureBranchProducerNeverInvoked(t *testing.T) {
	st := seedRepo(t, "acme")
	rec := &recordingProducer{}
	cfg := controller.DefaultConfig()
	cfg.Producer = rec
	cfg.DefaultBranch = "main"
	ctrl := controller.New(st, controller.NoOpForge{}, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Push to a feature (non-default) branch.
	raw := []byte(`{
      "ref": "refs/heads/feature/x",
      "after": "abc123",
      "head_commit": {"id": "abc123", "committed": {"email": "a@b.c"}},
      "repository": {"full_name": "acme/widgets"},
      "pusher": {"email": "u@acme.com"}
      }`)

	var payload webhook.PushPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := ctrl.HandlePush(payload); err != nil {
		t.Fatalf("HandlePush on feature branch: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("producer MUST NOT be called for a non-default-branch push; calls=%d", rec.calls)
	}
}

func TestDefaultBranchProducerIsInvoked(t *testing.T) {
	st := seedRepo(t, "acme")
	rec := &recordingProducer{}
	cfg := controller.DefaultConfig()
	cfg.Producer = rec
	cfg.DefaultBranch = "main"
	ctrl := controller.New(st, controller.NoOpForge{}, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Push to the default branch — producer should run.
	raw := []byte(`{
      "ref": "refs/heads/main",
      "after": "deadbeef",
      "head_commit": {"id": "deadbeef", "committed": {"email": "a@b.c"}},
      "repository": {"full_name": "acme/widgets", "default_branch": "main"},
      "pusher": {"email": "u@acme.com"}
      }`)

	var payload webhook.PushPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := ctrl.HandlePush(payload); err != nil {
		t.Fatalf("HandlePush on default branch: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("producer should be called exactly once on default-branch push; got %d", rec.calls)
	}
}

// TestSameRepoPRProducerNeverInvoked verifies that a non-fork same-repo PR
// also does not invoke the workflow producer. This is the core of the
// branch-poisoning fix: only push events to the default branch are allowed
// to upsert workflows.
func TestSameRepoPRProducerNeverInvoked(t *testing.T) {
	st := seedRepo(t, "acme")
	rec := &recordingProducer{}
	// DefaultBranch is "main" (from DefaultConfig), but only push events
	// should trigger the producer — never PR events.
	cfg := controller.DefaultConfig()
	cfg.Producer = rec
	cfg.DefaultBranch = "main"
	ctrl := controller.New(st, controller.NoOpForge{}, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Same-repo PR (non-fork) to main from a feature branch.
	raw := []byte(`{
       "action": "opened",
       "number": 42,
       "fork": false,
       "base":   {"ref": "main"},
       "head":   {"ref": "feature/x", "sha": "deadbeef", "repo": {"full_name": "acme/widgets"}},
       "repository": {"full_name": "acme/widgets", "default_branch": "main"}
       }`)

	var payload webhook.PullRequestPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := ctrl.HandlePullRequest(payload); err != nil {
		t.Fatalf("HandlePullRequest (same-repo) should not error: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("producer MUST NOT be called for ANY PR event, even same-repo non-fork; calls=%d", rec.calls)
	}
}
