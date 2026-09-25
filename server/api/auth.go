package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// authenticator resolves a request's identity from its bearer credentials -- the single point where "who is calling this" gets
// checked. In M1 it stands in for nothing real: noOpAuthenticator permits every request, and when M3 lands GitHub
// OAuth behind it that same interface starts rejecting unauthenticated calls so /api/v1 routes never silently admit
// a principal they shouldn't know about.
type authenticator interface {
	Identify(ctx context.Context, r *http.Request) error
}

// noOpAuthenticator is the M1 stand-in -- identity resolution doesn't exist yet (that arrives with M3's GitHub OAuth), but
// wiring its seam now means "swap in real auth" on that day becomes a one-line Option change rather than architectural.
type noOpAuthenticator struct{}

func (noOpAuthenticator) Identify(_ context.Context, _ *http.Request) error { return nil }

// withAuthenticator runs each request past the resolved authenticator before it reaches the mux; today that's always the
// pass-through noOpAuthenticator, but keeping the gating logic written here now so M3/M5's follow-up can slot in its own
// rejection/audit-line middleware without reshaping the chain.
// webhookPathPrefix is the path prefix that carries GitHub's HMAC-signed
// webhook deliveries. Webhooks authenticate via signature (X-Hub-Signature-256,
// verified inside the receiver), not via a user session, so the authenticator
// must be exempt for this prefix. Without the exemption, M3's GitHub-OAuth
// wiring will reject every legitimate GitHub delivery with a 401.
const webhookPathPrefix = "/api/v1/webhooks"

func withAuthenticator(a authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Path-based exemption: webhook deliveries authenticate via
			// signature inside the receiver, not via the user-session
			// authenticator. See PR #23 review (handler.go:124).
			if strings.HasPrefix(r.URL.Path, webhookPathPrefix) {
				next.ServeHTTP(w, r)
				return
			}
			if err := a.Identify(r.Context(), r); err != nil {
				writeError(w, log, http.StatusUnauthorized, errCodeUnauthorized, "authentication failed", err)
				return
			}
			next.ServeHTTP(w, r) // M3 attaches the resolved principal to context in place of this bare pass-through.
		})
	}
}
