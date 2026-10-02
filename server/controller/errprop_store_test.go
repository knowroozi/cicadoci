package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/backpressure"
	"cicadoci.com/cicado/server/webhook"
)

// CIC-105: a store or backpressure failure inside the controller must reach
// the receiver as an error (→ 5xx), never be swallowed as success (→ 200).
// These tests are gated on a real Postgres; they skip when
// CICADO_TEST_DATABASE_URL is unset.

func pushPayload(fullName string) webhook.PushPayload {
	var p webhook.PushPayload
	p.Ref = "refs/heads/main"
	p.After = "abc123"
	p.Repository.FullName = fullName
	return p
}

// TestHandlers_StoreErrorPropagates closes the store's pool so ResolveRepo
// fails, then asserts both handlers return the error.
func TestHandlers_StoreErrorPropagates(t *testing.T) {
	st := testdb.New(t)
	st.Pool().Close()
	c := New(st, NoOpForge{}, DefaultConfig(), discardLogger)

	if err := c.HandlePush(pushPayload("acme/core")); err == nil {
		t.Error("HandlePush returned nil on a store error; want an error (→ 5xx)")
	}

	var pr webhook.PullRequestPayload
	pr.Action = "opened"
	pr.PullRequest.Number = 1
	pr.PullRequest.Base.Ref = "main"
	pr.PullRequest.Head.SHA = "abc123"
	pr.Repository.FullName = "acme/core"
	if err := c.HandlePullRequest(pr); err == nil {
		t.Error("HandlePullRequest returned nil on a store error; want an error (→ 5xx)")
	}
}

// TestHandlePush_BackpressureErrorPropagates fills the repo's only
// backpressure slot, then asserts HandlePush returns an error instead of
// dropping the push with a nil (→ 200).
func TestHandlePush_BackpressureErrorPropagates(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.Backpressure = backpressure.NewTracker(backpressure.Limits{PerRepo: 1})
	c := New(st, NoOpForge{}, cfg, discardLogger)

	orgID, repoID, err := st.ResolveRepo(ctx, "acme", "core", cfg.DefaultBranch, "")
	if err != nil {
		t.Fatalf("resolve repo: %v", err)
	}
	if ok, err := cfg.Backpressure.Acquire(ctx, repoID.String(), orgID.String()); err != nil || !ok {
		t.Fatalf("pre-fill acquire: ok=%v err=%v", ok, err)
	}

	if err := c.HandlePush(pushPayload("acme/core")); err == nil {
		t.Error("HandlePush returned nil when backpressure refused; want an error (→ 5xx)")
	}
}

// TestReceiver_StoreErrorIs5xxAndNotDeduped wires the real controller behind
// the webhook receiver.  With a failing store, a delivery must get a 5xx and
// must not be recorded as seen: a redelivery with the same GUID is dispatched
// again (another 5xx), not answered "200 duplicate".
func TestReceiver_StoreErrorIs5xxAndNotDeduped(t *testing.T) {
	st := testdb.New(t)
	st.Pool().Close()
	c := New(st, NoOpForge{}, DefaultConfig(), discardLogger)

	const secret = "s3cret"
	receiver := webhook.NewReceiver(webhook.ReceiverOptions{
		Secret:   secret,
		Dispatch: c,
		Tracker:  webhook.NewDeliveryTracker(0, 0),
		Logger:   discardLogger,
	})

	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"name":"core","full_name":"acme/core"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	for i := 1; i <= 2; i++ {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("X-GitHub-Event", webhook.EventPush)
		req.Header.Set("X-GitHub-Delivery", "delivery-store-err")
		rr := httptest.NewRecorder()
		receiver.ServeHTTP(rr, req)
		if rr.Code < 500 {
			t.Fatalf("delivery %d: status = %d, want 5xx; body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if n := receiver.Suppressed(); n != 0 {
		t.Errorf("Suppressed() = %d, want 0 (a failed delivery must not be recorded as seen)", n)
	}
}
