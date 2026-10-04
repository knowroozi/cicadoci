package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// authenticator resolves a request's identity from its bearer credentials -- the single point where "who is calling this" gets
// checked. In M1 it stands in for nothing real: noOpAuthenticator permits every request, and when M3 lands GitHub
// OAuth behind it that same interface starts rejecting unauthenticated calls so /api/v1 routes never silently admit
// a principal they shouldn't know about.
type authenticator interface {
	Identify(ctx context.Context, r *http.Request) error
}

// noOpAuthenticator is the --insecure-api development opt-in (CIC-150): it
// permits every request and must never guard a server anyone but the operator
// can reach. Keeping it as a swappable seam means "swap in real auth" was a
// one-line Option change when CIC-150 landed, and "swap in GitHub OAuth"
// (CIC-44) will be one too.
type noOpAuthenticator struct{}

func (noOpAuthenticator) Identify(_ context.Context, _ *http.Request) error { return nil }

// bearerToken extracts the credential from an "Authorization: Bearer <t>"
// header, returning ("", false) when the header is absent or not of the
// Bearer form so the caller can treat that as "no credential presented". The
// scheme is matched case-insensitively (RFC 7235) so a client that sends
// "bearer <t>" is accepted, and the credential is trimmed of surrounding
// whitespace so a token loaded from a file or a Kubernetes Secret with a
// trailing newline still matches the configured secret (which is trimmed the
// same way). N2.
func bearerToken(r *http.Request) (string, bool) {
	const scheme = "bearer "
	a := r.Header.Get("Authorization")
	if len(a) < len(scheme) || !strings.EqualFold(a[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(a[len(scheme):]), true
}

// staticTokenAuthenticator is CIC-150's fail-closed authenticator: every
// request must carry a bearer token equal to the configured operator secret
// (compared in constant time), or it is rejected with 401. It closes the hole
// CIC-82 opened -- any job's live log stream, plus the run/runner read routes,
// was readable by anyone who could reach the HTTP port because the only
// authenticator in the shipped binary was noOpAuthenticator.
//
// The static secret is the operator's master credential and is deliberately
// unrestricted: it authorizes the whole server. Per-principal org scoping -- a
// principal from org A must not read org B's job -- is the CIC-44 follow-up and
// needs a principal model this ticket does not introduce.
type staticTokenAuthenticator struct {
	token []byte
}

// NewStaticTokenAuthenticator builds the fail-closed authenticator for a
// non-empty operator secret. main.go builds it from --api-token and only wires
// it when the secret is non-empty; an empty secret leaves noOpAuthenticator in
// place, which the start-up guard (apiStartupGuard) forbids unless
// --insecure-api was also passed.
func NewStaticTokenAuthenticator(secret string) authenticator {
	return staticTokenAuthenticator{token: []byte(strings.TrimSpace(secret))}
}

func (a staticTokenAuthenticator) Identify(_ context.Context, r *http.Request) error {
	presented, ok := bearerToken(r)
	if !ok {
		return errors.New("missing bearer token")
	}
	// B3: an empty presented credential must never match. A whitespace-only
	// Authorization value trims to "" and must be rejected, not accepted
	// against a stored-but-(whitespace-)empty secret. Defensive in depth:
	// WithAPIToken arms only on a non-empty trimmed secret and the start-up
	// guard fails closed on a whitespace-only CICADO_API_TOKEN, but a directly
	// constructed authenticator must not accept "".
	if presented == "" {
		return errors.New("empty bearer token")
	}
	// ConstantTimeCompare returns 0 for differing-length inputs, which is
	// enough -- a wrong-length or wrong-value token fails either way.
	if subtle.ConstantTimeCompare([]byte(presented), a.token) != 1 {
		return errors.New("invalid bearer token")
	}
	return nil
}

// withAuthenticator runs each request past the resolved authenticator before it reaches the mux; today that's always the
// pass-through noOpAuthenticator, but keeping the gating logic written here now so M3/M5's follow-up can slot in its own
// rejection/audit-line middleware without reshaping the chain.
// webhookPathPrefix is the path prefix that carries GitHub's HMAC-signed
// webhook deliveries. Webhooks authenticate via signature (X-Hub-Signature-256,
// verified inside the receiver), not via a user session, so the authenticator
// must be exempt for this prefix. Without the exemption, M3's GitHub-OAuth
// wiring will reject every legitimate GitHub delivery with a 401.
const webhookPathPrefix = "/api/v1/webhooks"

// healthProbePaths are the operational liveness/readiness probes. They must
// stay reachable without a user-session credential so an orchestrator can tell
// "alive and ready" apart from "crashed" even when API auth is armed, mirroring
// the webhook exemption just above.
var healthProbePaths = map[string]bool{"/healthz": true, "/readyz": true}

// pathIsWebhook reports whether the request path is a GitHub webhook
// delivery. It compares against the path.Clean-ed form so a raw path that
// merely CONTAINS the prefix (e.g. "/api/v1/webhooks/../runs" -- a traversal
// that decodes back to /api/v1/runs) is not mistaken for a webhook delivery and
// exempted from the user-session check. N1.
func pathIsWebhook(p string) bool {
	p = path.Clean(p)
	return p == webhookPathPrefix || strings.HasPrefix(p, webhookPathPrefix+"/")
}

func withAuthenticator(a authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Path-based exemption: webhook deliveries authenticate via
			// signature inside the receiver, not via the user-session
			// authenticator. See PR #23 review (handler.go:124).
			// Operational probes are exempt: a liveness/readiness scraper must
			// reach them even when API auth is armed.
			if healthProbePaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			if pathIsWebhook(r.URL.Path) {
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
