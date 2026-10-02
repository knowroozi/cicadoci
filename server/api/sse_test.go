package api_test

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/api"
	"cicadoci.com/cicado/server/archive"
	"cicadoci.com/cicado/server/logs"
)

// CIC-82: the per-job live log stream at GET /api/v1/logs.

// denyAll is an authenticator that rejects every request.
type denyAll struct{}

func (denyAll) Identify(context.Context, *http.Request) error { return errors.New("no credentials") }

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequestWithContext(context.Background(), method, target, nil))
	return rr
}

func TestSSERoute(t *testing.T) {
	sse := logs.NewSSE(logs.NewHub(8), nil)

	// Wired: the request reaches the SSE handler, which rejects a bad job id.
	if got := serve(api.New(&fakeReader{}, api.WithSSEHandler(sse)), http.MethodGet, "/api/v1/logs?job=nope").Code; got != http.StatusBadRequest {
		t.Errorf("wired, bad job id: status %d, want 400", got)
	}
	// Not wired: the route does not exist.
	if got := serve(api.New(&fakeReader{}), http.MethodGet, "/api/v1/logs?job=nope").Code; got != http.StatusNotFound {
		t.Errorf("not wired: status %d, want 404", got)
	}
	// Only GET is the stream; other methods take the regular chain and 405/404.
	if got := serve(api.New(&fakeReader{}, api.WithSSEHandler(sse)), http.MethodPost, "/api/v1/logs").Code; got == http.StatusOK || got == http.StatusBadRequest {
		t.Errorf("POST /api/v1/logs reached the SSE handler (status %d)", got)
	}
}

// Both chains authenticate: the SSE chain must not become a way around auth.
func TestSSERouteAndRegularRoutesRequireAuth(t *testing.T) {
	h := api.New(&fakeReader{}, api.WithAuthenticator(denyAll{}), api.WithSSEHandler(logs.NewSSE(logs.NewHub(8), nil)))

	for _, target := range []string{"/api/v1/logs?job=" + uuid.NewString(), "/api/v1/runs"} {
		if got := serve(h, http.MethodGet, target).Code; got != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated: status %d, want 401", target, got)
		}
	}
}

// A published entry reaches a connected client while the stream is open, and
// the stream outlives the per-request timeout. That needs the SSE chain to
// flush through withLogging and to skip withRequestTimeout/withJSONErrors.
func TestSSEStreamsPastRequestTimeout(t *testing.T) {
	hub := logs.NewHub(8)
	h := api.New(&fakeReader{}, api.WithTimeout(50*time.Millisecond), api.WithSSEHandler(logs.NewSSE(hub, nil)))
	srv := httptest.NewServer(h)
	defer srv.Close()

	job := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/logs?job="+job.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The SSE handler sends its headers with the first event, so Do returns
	// only once something is published. Publish from the side, after the
	// request timeout has passed and the handler has subscribed.
	go func() {
		time.Sleep(150 * time.Millisecond)
		for hub.SubscriberCount(job) == 0 && ctx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		hub.Publish(job, archive.Entry{Seq: 1, Kind: "chunk", Data: []byte("hello")})
	}()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var got []string
	for line := range lines {
		got = append(got, line)
		if line == "data: hello" {
			return
		}
	}
	t.Fatalf("stream ended before the event arrived; read %q", strings.Join(got, "\\n"))
}
