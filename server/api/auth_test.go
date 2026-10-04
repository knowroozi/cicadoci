package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWithAuthenticator_WebhookPathExempt pins that requests to the
// webhook path prefix bypass the user-session authenticator.
func TestWithAuthenticator_WebhookPathExempt(t *testing.T) {
	// The authenticator rejects all requests unless the path is
	// under /api/v1/webhooks.
	a := &rejectAuthenticator{}
	handler := withAuthenticator(a, slog.Default())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	// A non-webhook path is rejected.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/runs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-webhook path: code = %d, want 401", rec.Code)
	}

	// A webhook path is exempt.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/webhooks/github", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook path: code = %d, want 200 (exempt from auth)", rec.Code)
	}
}

// rejectAuthenticator denies everything, used to verify the path exemption
// is what lets webhook requests through, not a pass-through authenticator.
type rejectAuthenticator struct{}

func (rejectAuthenticator) Identify(_ context.Context, _ *http.Request) error {
	return errors.New("unauthorized")
}

// TestWithAuthenticator_WebhookExemptionResistsTraversal pins CIC-150 N1: a
// path that merely CONTAINS the webhook prefix but decodes (after path.Clean)
// back off it -- a traversal like /api/v1/webhooks/../runs -- must NOT be
// mistaken for a webhook delivery and exempted from the user-session check. A
// reject-all authenticator therefore returns 401, not 200 or a redirect.
func TestWithAuthenticator_WebhookExemptionResistsTraversal(t *testing.T) {
	handler := withAuthenticator(rejectAuthenticator{}, slog.Default())(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
	)

	// A raw path containing the prefix but decoding to /api/v1/runs after
	// path.Clean must be authenticated, not exempted.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/webhooks/../runs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("path-traversal past the webhook prefix: code = %d, want 401 (gate must not let it through or redirect)", rec.Code)
	}

	// The exact prefix and a real sub-path stay exempt.
	for _, p := range []string{webhookPathPrefix, "/api/v1/webhooks/github"} {
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, p, nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("legit webhook path %q: code = %d, want 200 (exempt)", p, rec.Code)
		}
	}
}

// TestBearerTokenCaseInsensitiveScheme pins CIC-150 N2: the "Bearer" scheme is
// matched case-insensitively (RFC 7235), so a client that sends "bearer <t>"
// or "BEARER <t>" is accepted identically to "Bearer <t>".
func TestBearerTokenCaseInsensitiveScheme(t *testing.T) {
	secret := "topsecret42"
	a := NewStaticTokenAuthenticator(secret)
	ctx := t.Context()

	for _, scheme := range []string{"bearer ", "BEARER ", "Bearer ", "bEarEr "} {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", scheme+secret)
		if err := a.Identify(ctx, req); err != nil {
			t.Errorf("scheme %q: Identify returned %v, want nil", scheme, err)
		}
	}
}

// TestBearerTokenToleratesWhitespace pins CIC-150 N2: a presented token (and
// the configured one) is trimmed of surrounding whitespace, so a token loaded
// from a file or Kubernetes Secret with a trailing/leading newline or space
// still matches.
func TestBearerTokenToleratesWhitespace(t *testing.T) {
	// The configured secret carries a trailing newline (as a Secret mount
	// would add); the presented token carries surrounding whitespace.
	a := NewStaticTokenAuthenticator("topsecret42\n")
	ctx := t.Context()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer  topsecret42 ")
	if err := a.Identify(ctx, req); err != nil {
		t.Fatalf("whitespace-padded token: Identify returned %v, want nil", err)
	}

	// A token that differs by a character in the middle still fails (trimming
	// only removes surrounding whitespace, not interior content).
	req = httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer  topsecretXZ ")
	if err := a.Identify(ctx, req); err == nil {
		t.Fatal("differing token: Identify returned nil, want error")
	}
}

// TestWithAuthenticator_HealthProbesExempt pins that the liveness and
// readiness probes bypass the authenticator even when it rejects everything:
// an orchestrator must be able to tell "alive" from "crashed" without holding
// the operator token.
func TestWithAuthenticator_HealthProbesExempt(t *testing.T) {
	handler := withAuthenticator(rejectAuthenticator{}, slog.Default())(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
	)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code = %d, want 200 (exempt from auth)", path, rec.Code)
		}
	}
}

// TestStaticTokenAuthenticator pins CIC-150's fail-closed authenticator: a
// correct Bearer token passes, and every other shape -- missing credential,
// wrong secret, non-Bearer scheme -- is rejected.
func TestStaticTokenAuthenticator(t *testing.T) {
	const secret = "topsecret42"
	a := NewStaticTokenAuthenticator(secret)
	ctx := t.Context()

	okReq := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
	okReq.Header.Set("Authorization", "Bearer "+secret)
	if err := a.Identify(ctx, okReq); err != nil {
		t.Fatalf("valid token: Identify returned %v, want nil", err)
	}

	cases := []struct {
		name, header string
	}{
		{name: "missing"},
		{name: "wrong_token", header: "Bearer nope"},
		{name: "non_bearer_scheme", header: "Basic Zm9vOmJhcg=="},
		{name: "missing_bearer_prefix", header: "token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			if err := a.Identify(ctx, req); err == nil {
				t.Fatalf("%s: Identify returned nil, want error", c.name)
			}
		})
	}
}

// TestStaticTokenAuthenticator_RejectsSameLengthWrongToken guards the token
// compare: a token equal in length to the secret but differing in one byte must
// be rejected, and only the exact match passes. (Timing is not asserted
// behaviorally; the constant-time compare is verified by code review of the
// subtle.ConstantTimeCompare call.)
func TestStaticTokenAuthenticator_RejectsSameLengthWrongToken(t *testing.T) {
	secret := "A" + "B"
	a := NewStaticTokenAuthenticator(secret)
	ctx := t.Context()

	match := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
	match.Header.Set("Authorization", "Bearer "+secret)
	if err := a.Identify(ctx, match); err != nil {
		t.Fatalf("exact match: got %v, want nil", err)
	}

	// Same length as the secret, differing in the last byte.
	wrong := make([]rune, len(secret))
	copy(wrong, []rune(secret))
	wrong[len(wrong)-1] = 'Z'
	off := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
	off.Header.Set("Authorization", "Bearer "+string(wrong))
	if err := a.Identify(ctx, off); err == nil {
		t.Fatal("same-length wrong token: got nil, want error")
	}
}

// TestStaticTokenAuthenticatorRejectsWhitespaceBearer pins CIC-150 B3: an
// authenticator armed with a whitespace-only secret (e.g. CICADO_API_TOKEN of
// just a newline from a Secret mount) stores a trimmed-empty token, and an
// empty/whitespace presented credential must be REJECTED, not accepted via
// ConstantTimeCompare("","")==1 → 200.
func TestStaticTokenAuthenticatorRejectsWhitespaceBearer(t *testing.T) {
	ctx := t.Context()

	// Armed with a whitespace-only secret (the exact B3 scenario).
	whitespace := NewStaticTokenAuthenticator("\n")
	for _, h := range []string{"Bearer \u00a0", "bearer \u3000", "Bearer ", "Bearer", "Bearer     "} {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", h)
		if err := whitespace.Identify(ctx, req); err == nil {
			t.Fatalf("whitespace-armed authenticator accepted %q: empty credential must fail closed", h)
		}
	}

	// A properly armed non-empty token also rejects an empty presented credential.
	armed := NewStaticTokenAuthenticator("real-token")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer ")
	if err := armed.Identify(ctx, req); err == nil {
		t.Fatalf("empty credential accepted for a non-empty armed token; must fail closed")
	}
}
