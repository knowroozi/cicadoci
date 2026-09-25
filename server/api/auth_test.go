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
