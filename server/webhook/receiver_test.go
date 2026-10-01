package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────────────

// sign computes a GitHub-compatible HMAC-SHA256 signature.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// testBody is a minimal valid push event body.
const testPushBody = `{"ref":"refs/heads/main","after":"abc123","before":"000000","repository":{"name":"myrepo","full_name":"owner/myrepo","private":false}}`

// ── signature tests ──────────────────────────────────────────────

func TestVerify_ValidSignature(t *testing.T) {
	secret := "topsecret"
	body := []byte(testPushBody)
	sig := sign(secret, body)

	err := Verify(secret, sig, body)
	if err != nil {
		t.Errorf("Verify: %v, want nil", err)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	secret := "correct"
	body := []byte(testPushBody)
	sig := sign("wrong", body)

	err := Verify(secret, sig, body)
	if !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("Verify: %v, want ErrSignatureMismatch", err)
	}
}

func TestVerify_WrongBody(t *testing.T) {
	secret := "topsecret"
	body := []byte(testPushBody)
	sig := sign(secret, body)

	// Tamper with body.
	err := Verify(secret, sig, []byte(testPushBody+`"`))
	if !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("Verify with tampered body: %v, want ErrSignatureMismatch", err)
	}
}

func TestVerify_EmptySignature(t *testing.T) {
	secret := "topsecret"
	body := []byte(testPushBody)

	err := Verify(secret, "", body)
	if !errors.Is(err, ErrEmptySignature) {
		t.Errorf("Verify with empty sig: %v, want ErrEmptySignature", err)
	}
}

func TestVerify_BareHexSignature(t *testing.T) {
	// Without the "sha256=" prefix — a bare hex string should still work.
	secret := "topsecret"
	body := []byte(testPushBody)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	err := Verify(secret, sig, body)
	if err != nil {
		t.Errorf("Verify with bare hex: %v, want nil", err)
	}
}

func TestVerify_MalformedHexSignature(t *testing.T) {
	secret := "topsecret"
	body := []byte(testPushBody)

	err := Verify(secret, "sha256=zzzz", body)
	if !errors.Is(err, ErrEmptySignature) {
		t.Errorf("Verify with malformed hex: %v, want ErrEmptySignature", err)
	}
}

// ── delivery tracker tests ───────────────────────────────────────

func TestDeliveryTracker_FirstSeen(t *testing.T) {
	tk := NewDeliveryTracker(10, time.Minute)
	now := time.Now()

	if tk.seen("delivery-1", now, nil) {
		t.Error("first call should return false (not yet recorded)")
	}
}

func TestDeliveryTracker_Duplicate(t *testing.T) {
	tk := NewDeliveryTracker(10, time.Minute)
	now := time.Now()

	if tk.seen("delivery-1", now, nil) {
		t.Error("first call should return false")
	}
	// Second call same delivery ID within TTL → true.
	if !tk.seen("delivery-1", now, nil) {
		t.Error("second call with same ID within TTL should return true")
	}
}

func TestDeliveryTracker_TTLExpiry(t *testing.T) {
	tk := NewDeliveryTracker(10, 100*time.Millisecond)
	now := time.Now()

	if tk.seen("delivery-1", now, nil) {
		t.Error("first call should return false")
	}

	// Sleep past the TTL.
	time.Sleep(150 * time.Millisecond)

	if tk.seen("delivery-1", time.Now(), nil) {
		t.Error("second call past TTL should return false (entry expired)")
	}
}

func TestDeliveryTracker_SuppressedCounter(t *testing.T) {
	tk := NewDeliveryTracker(10, time.Minute)
	now := time.Now()
	var suppressed uint64

	tk.seen("delivery-1", now, &suppressed)
	if suppressed != 0 {
		t.Errorf("suppressed after first call: %d, want 0", suppressed)
	}

	tk.seen("delivery-1", now, &suppressed)
	if suppressed != 1 {
		t.Errorf("suppressed after duplicate: %d, want 1", suppressed)
	}
}

func TestDeliveryTracker_EViction(t *testing.T) {
	tk := NewDeliveryTracker(3, time.Minute)
	now := time.Now()

	// Fill the tracker to capacity + 1.
	for i := 0; i < 3; i++ {
		tk.seen("d"+string(rune('0'+i)), now, nil)
	}
	// This one should evict "d0".
	tk.seen("d3", now, nil)

	// d0 should now be seen again.
	if tk.seen("d0", now, nil) {
		t.Error("d0 should have been evicted; second call should return false")
	}
	// d3 should still be known.
	if !tk.seen("d3", time.Now(), nil) {
		t.Error("d3 should still be in the tracker")
	}
}

// ── receiver HTTP-level tests ─────────────────────────────────────

// fakeDispatcher records what was called and returns a configurable result.
type fakeDispatcher struct {
	pushCalls       *int
	prCalls         *int
	checkSuiteCalls *int
	checkRunCalls   *int
	err             error
}

func (d fakeDispatcher) HandlePush(_ PushPayload) error {
	if d.pushCalls != nil {
		*d.pushCalls++
	}
	return d.err
}
func (d fakeDispatcher) HandlePullRequest(_ PullRequestPayload) error {
	if d.prCalls != nil {
		*d.prCalls++
	}
	return d.err
}
func (d fakeDispatcher) HandleCheckSuite(_ CheckSuitePayload) error {
	if d.checkSuiteCalls != nil {
		*d.checkSuiteCalls++
	}
	return d.err
}
func (d fakeDispatcher) HandleCheckRun(_ CheckRunPayload) error {
	if d.checkRunCalls != nil {
		*d.checkRunCalls++
	}
	return d.err
}

// TestReceiver_ValidPush tests end-to-end happy path for a push event.
func TestReceiver_ValidPush(t *testing.T) {
	secret := "secret123"
	body := []byte(testPushBody)
	sig := sign(secret, body)

	var pushCount int
	receiver := NewReceiver(ReceiverOptions{
		Secret:   secret,
		Dispatch: fakeDispatcher{pushCalls: &pushCount},
		Tracker:  NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", EventPush)
	// httptest.NewRequest with nil body; we need to set the body for the real read.
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if pushCount != 1 {
		t.Errorf("HandlePush called %d times, want 1", pushCount)
	}
}

func TestReceiver_RejectsBadSignature(t *testing.T) {
	secret := "secret123"
	body := []byte(testPushBody)

	receiver := NewReceiver(ReceiverOptions{
		Secret:   secret,
		Dispatch: fakeDispatcher{},
		Tracker:  NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("X-GitHub-Event", EventPush)
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestReceiver_DuplicateDeliverySuppressed(t *testing.T) {
	secret := "secret123"
	body := []byte(testPushBody)
	sig := sign(secret, body)

	var pushCount int
	tracker := NewDeliveryTracker(0, 0)
	receiver := NewReceiver(ReceiverOptions{
		Secret:   secret,
		Dispatch: fakeDispatcher{pushCalls: &pushCount},
		Tracker:  tracker,
	})

	// First delivery: should succeed and call HandlePush.
	req1 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req1.Header.Set("X-Hub-Signature-256", sig)
	req1.Header.Set("X-GitHub-Event", EventPush)
	req1.Header.Set("X-GitHub-Delivery", "delivery-abc")
	req1.Body = newBodyReader(body)

	rr1 := httptest.NewRecorder()
	receiver.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want 200", rr1.Code)
	}
	if pushCount != 1 {
		t.Errorf("HandlePush called %d times, want 1", pushCount)
	}

	// Second delivery (same ID, same body): should be suppressed.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-GitHub-Event", EventPush)
	req2.Header.Set("X-GitHub-Delivery", "delivery-abc")
	req2.Body = newBodyReader(body)

	rr2 := httptest.NewRecorder()
	receiver.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("duplicate delivery status = %d, want 200", rr2.Code)
	}
	// HandlePush should NOT be called a second time.
	if pushCount != 1 {
		t.Errorf("HandlePush called %d times after duplicate, want 1", pushCount)
	}

	// The suppressed counter should be 1.
	if receiver.Suppressed() != 1 {
		t.Errorf("Suppressed() = %d, want 1", receiver.Suppressed())
	}
}

func TestReceiver_UknownEventType(t *testing.T) {
	secret := "secret123"
	// Empty JSON body so json.Unmarshal succeeds on an unknown type too.
	body := []byte(`{}`)
	sig := sign(secret, body)

	receiver := NewReceiver(ReceiverOptions{
		Secret:   secret,
		Dispatch: fakeDispatcher{},
		Tracker:  NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "workflow_dispatch") // CICaDo doesn't handle this.
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown event type", rr.Code)
	}
}

func TestReceiver_NoSignatureCheck_WhenUnauthenticated(t *testing.T) {
	// An empty secret with AllowUnauthenticated=true disables HMAC checking — the
	// explicit local-dev opt-in path behind --insecure-webhook.
	body := []byte(testPushBody)
	var pushCount int

	receiver := NewReceiver(ReceiverOptions{
		Secret:               "", // empty → only checks are skipped because of the explicit opt-in
		AllowUnauthenticated: true,
		Dispatch:             fakeDispatcher{pushCalls: &pushCount},
		Tracker:              NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-GitHub-Event", EventPush)
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no secret check via AllowUnauthenticated)", rr.Code)
	}
	if pushCount != 1 {
		t.Errorf("HandlePush called %d times, want 1", pushCount)
	}
}

func TestReceiver_FloodBackpressure(t *testing.T) {
	secret := "secret123"
	body := []byte(testPushBody)
	sig := sign(secret, body)

	var pushCount int
	// Cap of 1 delivery at a time, and a dispatcher that blocks until released.
	blockingCh := make(chan struct{})
	blockingDispatcher := &blockingDispatcher{
		pushCalls: &pushCount,
		blockCh:   blockingCh,
	}

	receiver := NewReceiver(ReceiverOptions{
		Secret:        secret,
		Dispatch:      blockingDispatcher,
		Tracker:       NewDeliveryTracker(0, 0),
		MaxConcurrent: 1,
	})

	// First delivery acquires the slot and blocks.
	req1 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req1.Header.Set("X-Hub-Signature-256", sig)
	req1.Header.Set("X-GitHub-Event", EventPush)
	req1.Body = newBodyReader(body)

	rr1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		receiver.ServeHTTP(rr1, req1)
		close(done1)
	}()

	// Give the goroutine time to acquire the semaphore.
	time.Sleep(20 * time.Millisecond) //nolint:stylecheck // intentional timing for test

	// Second delivery should be rejected with 503.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-GitHub-Event", EventPush)
	req2.Body = newBodyReader(body)

	rr2 := httptest.NewRecorder()
	receiver.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("second flood request status = %d, want 503", rr2.Code)
	}

	// Release the first.
	close(blockingCh)
	<-done1

	if rr1.Code != http.StatusOK {
		t.Errorf("first delivery status = %d, want 200", rr1.Code)
	}
	if pushCount != 1 {
		t.Errorf("HandlePush called %d times, want 1", pushCount)
	}
}

func TestReceiver_PullRequestEvent(t *testing.T) {
	secret := "secret123"
	body := []byte(`{"action":"opened","number":42,"base":{"ref":"main"},"head":{"ref":"feature-branch","sha":"abc123"},"repository":{"name":"myrepo","full_name":"owner/myrepo","private":false},"fork":false}`)
	sig := sign(secret, body)

	var prCount int
	receiver := NewReceiver(ReceiverOptions{
		Secret:   secret,
		Dispatch: fakeDispatcher{prCalls: &prCount},
		Tracker:  NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", EventPullRequest)
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if prCount != 1 {
		t.Errorf("HandlePullRequest called %d times, want 1", prCount)
	}
}

func TestReceiver_ChechRunEvent(t *testing.T) {
	secret := "secret123"
	body := []byte(`{"action":"rerequested","check_run":{"id":1234},"repository":{"full_name":"owner/repo"}}`)
	sig := sign(secret, body)

	var crCount int
	receiver := NewReceiver(ReceiverOptions{
		Secret:   secret,
		Dispatch: fakeDispatcher{checkRunCalls: &crCount},
		Tracker:  NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", EventCheckRun)
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if crCount != 1 {
		t.Errorf("HandleCheckRun called %d times, want 1", crCount)
	}
}

func TestReceiver_MissingEventType(t *testing.T) {
	secret := "secret123"
	body := []byte("{}")
	sig := sign(secret, body)

	receiver := NewReceiver(ReceiverOptions{
		Secret:   secret,
		Dispatch: fakeDispatcher{},
		Tracker:  NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks/github", nil)
	req.Header.Set("X-Hub-Signature-256", sig)
	// No X-GitHub-Event header.
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing event type", rr.Code)
	}
}

// ── payload decoding tests ────────────────────────────────────────

func TestDispatch_PushPayloadDecoding(t *testing.T) {
	body := []byte(testPushBody)
	var pushCount int
	d := fakeDispatcher{pushCalls: &pushCount}
	err := Dispatch(EventPush, body, &d)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if pushCount != 1 {
		t.Errorf("HandlePush called %d times, want 1", pushCount)
	}
}

func TestDispatch_PullRequestActionPreserved(t *testing.T) {
	body := []byte(`{"action":"synchronize","number":7,"base":{"ref":"main"},"head":{"ref":"dev"},"repository":{"name":"repo","full_name":"owner/repo"},"fork":false}`)

	var captured string
	d := capturePullRequest{fn: func(p PullRequestPayload) error {
		captured = p.Action
		return nil
	}}
	err := Dispatch(EventPullRequest, body, &d)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if captured != "synchronize" {
		t.Errorf("action = %q, want \"synchronize\"", captured)
	}
}

func TestDispatch_UnknownEvent(t *testing.T) {
	body := []byte("{}")
	var d fakeDispatcher
	err := Dispatch("workflow_dispatch", body, &d)
	if !IsUnknownEventType(err) {
		t.Errorf("Dispatch unknown event: %v, want IsUnknownEventType", err)
	}
}

// ── test helpers ──────────────────────────────────────────────────

// newBodyReader returns an io.ReadCloser for body bytes.
func newBodyReader(body []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(body)) }

// blockingDispatcher blocks on HandlePush until the channel is closed.
type blockingDispatcher struct {
	pushCalls *int
	blockCh   chan struct{}
}

func (d *blockingDispatcher) HandlePush(_ PushPayload) error {
	*d.pushCalls++
	<-d.blockCh
	return nil
}
func (d blockingDispatcher) HandlePullRequest(_ PullRequestPayload) error { return nil }
func (d blockingDispatcher) HandleCheckSuite(_ CheckSuitePayload) error   { return nil }
func (d blockingDispatcher) HandleCheckRun(_ CheckRunPayload) error       { return nil }

// capturePullRequest is a Dispatcher that records the pull request action.
type capturePullRequest struct {
	fn func(PullRequestPayload) error
}

func (d *capturePullRequest) HandlePush(_ PushPayload) error {
	return nil
}
func (d capturePullRequest) HandlePullRequest(p PullRequestPayload) error {
	return d.fn(p)
}
func (d *capturePullRequest) HandleCheckSuite(_ CheckSuitePayload) error { return nil }
func (d *capturePullRequest) HandleCheckRun(_ CheckRunPayload) error     { return nil }

// compile-time check
var (
	_ Dispatcher = fakeDispatcher{}
	_ Dispatcher = (*blockingDispatcher)(nil)
	_ Dispatcher = (*capturePullRequest)(nil)
)

// json round-trip of a minimal payload.
func TestPayloadJSONRoutrdTrip(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"action": "opened",
		"number": 42,
		"base":   map[string]any{"ref": "main"},
	})
	var p PullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if p.Action != "opened" || p.Number != 42 {
		t.Errorf("unexpected pull request payload: action=%q number=%d", p.Action, p.Number)
	}
}
func TestReceiver_FailsClosed_WhenSecretEmpty(t *testing.T) {
	// Without AllowUnauthenticated, an empty-secret receiver must REJECT every
	// delivery with 401 rather than silently accepting it unverified.
	body := []byte(testPushBody)
	var pushCount int

	receiver := NewReceiver(ReceiverOptions{
		Secret:   "",
		Dispatch: fakeDispatcher{pushCalls: &pushCount},
		Tracker:  NewDeliveryTracker(0, 0),
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/webhooks/github", nil)
	req.Header.Set("X-GitHub-Event", EventPush)
	req.Header.Set("X-GitHub-Delivery", "del-no-secret")
	req.Body = newBodyReader(body)

	rr := httptest.NewRecorder()
	receiver.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (fail closed on empty secret)", rr.Code)
	}
	// The response body must not leak server configuration to the caller.
	if strings.Contains(rr.Body.String(), "signing secret not configured") {
		t.Errorf("response body leaked server configuration: %s", rr.Body.String())
	}
	if pushCount != 0 {
		t.Errorf("HandlePush called %d times; must not dispatch unauthenticated delivery", pushCount)
	}
}

func TestReceiver_WarnsOnUnauthenticatedConstruction(t *testing.T) {
	// Constructing a receiver with an empty secret and AllowUnauthenticated must
	// produce a logged warning so the operator has an unmissable signal.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	NewReceiver(ReceiverOptions{
		Secret:               "",
		AllowUnauthenticated: true,
		Dispatch:             fakeDispatcher{},
		Tracker:              NewDeliveryTracker(0, 0),
		Logger:               logger,
	})

	out := buf.String()
	if !strings.Contains(out, "WITHOUT signature verification") {
		t.Errorf("missing unauthenticated warning log line, got: %q", out)
	}
}
