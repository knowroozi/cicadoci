package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"cicadoci.com/cicado/server/api"
	"cicadoci.com/cicado/server/logs"
)

// CIC-150: the fail-closed API gate must be installed on every /api/v1 route --
// including the per-job live log stream, the concrete "anyone reads any job's
// logs" case the ticket calls out -- while the webhook-delivery and health-probe
// exemptions stay reachable without a credential.

const sseJobUUID = "00000000-0000-0000-0000-000000000000"

func TestAPITokenFailClosed(t *testing.T) {
	const secret = "operator-secret"
	h := api.New(&fakeReader{},
		api.WithAPIToken(secret),
		api.WithSSEHandler(logs.NewSSE(logs.NewHub(8), nil)),
	)

	withBearer := func(target, token string) int {
		q := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		q.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, q)
		return rec.Code
	}

	// No credential on a read route is rejected (acceptance #1).
	if got := serve(h, http.MethodGet, "/api/v1/runs").Code; got != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/runs without token: status %d, want 401", got)
	}

	// A wrong token is rejected too.
	if got := withBearer("/api/v1/runs", "wrongtoken"); got != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/runs with wrong token: status %d, want 401", got)
	}

	// The per-job live log stream -- the headline case -- also fails closed
	// with no credential.
	if got := serve(h, http.MethodGet, "/api/v1/logs?job="+sseJobUUID).Code; got != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/logs without token: status %d, want 401", got)
	}

	// The correct token lets the read routes through.
	if got := withBearer("/api/v1/runs", secret); got != http.StatusOK {
		t.Errorf("GET /api/v1/runs with token: status %d, want 200", got)
	}

	// Operational probes stay reachable without a credential.
	if got := serve(h, http.MethodGet, "/healthz").Code; got != http.StatusOK {
		t.Errorf("GET /healthz without token: status %d, want 200 (exempt)", got)
	}
	if got := serve(h, http.MethodGet, "/readyz").Code; got != http.StatusOK {
		t.Errorf("GET /readyz without token: status %d, want 200 (exempt)", got)
	}
}

// TestAPIEmptyOrWhitespaceTokenFailsClosed pins CIC-150 B4: an empty or
// whitespace-only secret passed to WithAPIToken installs a reject-all
// authenticator instead of the no-op default, so every /api/v1 route 401s on its
// own -- the handler does not trust main's apiStartupGuard to have caught a
// whitespace-only CICADO_API_TOKEN first. --insecure-api stays open in main because
// main simply omits WithAPIToken when no real token is present; this test exercises
// the handler level in isolation, where fail-closed is the only safe default.
func TestAPIEmptyOrWhitespaceTokenFailsClosed(t *testing.T) {
	//nolint:paralleltest -- single httptest recorder per call, no shared state
	for _, secret := range []string{"", "   ", "\n", "\t\n", " \u00a0 "} {
		h := api.New(&fakeReader{}, api.WithAPIToken(secret))
		if got := serve(h, http.MethodGet, "/api/v1/runs").Code; got != http.StatusUnauthorized {
			t.Errorf("GET /api/v1/runs with empty/whitespace token %q: status %d, want 401 (fail closed)", secret, got)
		}
		// The live per-job log stream must also fail closed.
		if got := serve(h, http.MethodGet, "/api/v1/logs?job="+sseJobUUID).Code; got != http.StatusUnauthorized {
			t.Errorf("GET /api/v1/logs with empty/whitespace token %q: status %d, want 401", secret, got)
		}
	}

	// A real token still lets the read routes through (no regression on the happy path).
	{
		h := api.New(&fakeReader{}, api.WithAPIToken("operator-secret"))
		q := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/runs", nil)
		q.Header.Set("Authorization", "Bearer operator-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, q)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/v1/runs with a real token: status %d, want 200", rec.Code)
		}
	}
}
