// Package api is the HTTP API of CICaDo -- the single surface every client (Web UI, Slack, MCP server, CLI) talks to.
// Because they all route through this handler, authentication and auditing happen once here instead of being reimplemented
// per surface; that property underwrites the architecture doc's "every state-changing action writes an audit event" invariant.
//
// M1 ships as a scaffold: router over a small read surface plus health probes, uniform JSON error envelope, request-scoped
// timeout context, and a no-op auth seam -- nothing authorizes yet until M3 swaps in GitHub OAuth via WithAuthenticator.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"cicadoci.com/cicado/server/store"
)

// maxListLimit caps the ?limit query param on GET /runs so a hostile or misconfigured client can't force an unbounded scan.
const maxListLimit = 1000

var (
	errCodeBadRequest       = "bad_request"
	errCodeMethodNotAllowed = "method_not_allowed"
	errCodeNotFound         = "not_found"
	errCodeUnauthorized     = "unauthorized"
	// errCodeServiceUnreachable maps to HTTP 503 -- used by /readyz when the database is not reachable.
	errCodeServiceUnreachable = "service_unavailable"
	errCodeInternal           = "internal_error"
)

// Handler is the fully-wired HTTP API and an http.Handler. Build one via New, not by hand -- the composed middleware chain
// lives in h.handler and must be assembled inside buildChain so new layers land in that one place rather than scattered.
type Handler struct {
	reader store.Reader // read-only store seam; see the storage layer's Reader interface for why this decoupling exists
	logger *slog.Logger
	auth   authenticator

	handler http.Handler // pre-composed middleware chain, assembled once at startup and frozen in place after that point.
}

// Option customises Handler construction (logging destination, per-request timeout ceiling, or an auth back-end to swap in for M3).
type Option func(*options)

type options struct {
	timeout time.Duration
	logger  *slog.Logger
	auth    authenticator
}

func WithLogger(l *slog.Logger) Option         { return func(o *options) { o.logger = l } }
func WithTimeout(d time.Duration) Option       { return func(o *options) { o.timeout = d } }
func WithAuthenticator(a authenticator) Option { return func(o *options) { o.auth = a } }

// defaultRequestTimeout is M1's per-request ceiling before a slow client stalls its goroutine for free.
const defaultRequestTimeout = 30 * time.Second

// readinessProbeDeadline caps the /readyz probe specifically (not any business route): an orchestrator scraping this URL on a
// tight cadence should learn "database unreachable" quickly rather than waiting out the full per-request budget -- so a dead or
// hung pool fails the readiness check fast enough for the caller's own retry/backoff loop to act sensibly, instead of near-
// maxing defaultRequestTimeout on a probe that will never recover within one scrape window. It lives alongside defaultRequestTimeout
// purely as documentation of intent (the two share no runtime wiring); the deadline itself is applied locally inside handleReadyz
// only, deliberately NOT threaded through main.go's process-wide WithTimeout option so real route handlers keep their full 30s
// ceiling untouched by a liveness/readiness concern.
const readinessProbeDeadline = 3 * time.Second

// New assembles the fully-composed handler. Callers pass only a store.Reader (its real implementation is *store.Store, but that's
// deliberately hidden behind an interface so tests and future callers can drive it without pulling in Postgres or pgx directly).
func New(r store.Reader, opts ...Option) *Handler {
	if r == nil {
		panic("api: New requires a non-nil store.Reader")
	}

	o := options{timeout: defaultRequestTimeout, auth: noOpAuthenticator{}}
	for _, opt := range opts {
		opt(&o)
	}

	if o.logger == nil {
		o.logger = slog.Default()
	}

	h := &Handler{reader: r, logger: o.logger, auth: o.auth}
	mux := h.buildMux()
	h.handler = buildChain(h, mux, o.timeout)
	return h
}

// buildMux registers every route on a stdlib ServeMux using the Go 1.23-style method + {wildcard} grammar; no third-party router is needed here for this small surface.
func (h *Handler) buildMux() http.Handler {
	mux := http.NewServeMux()

	// Health probes live OUTSIDE /api/v1 -- they're operational liveness/readiness signals, not part of the versioned contract a
	// client SDK would lock onto.
	mux.HandleFunc("GET /healthz", h.handleHealthz) // process is up; 200 regardless of downstream health so a scheduler can tell "alive" apart from "crashed".
	mux.HandleFunc("GET /readyz", h.handleReadyz)   // database reachable AND schema version current, else refuse traffic with 503.

	// Read surface: each route flows through the auth layer; today that's noOpAuthenticator so it always passes (M1 wiring), but this is
	// where M3/M5 swap in real GitHub OAuth without touching call-site code below.
	mux.HandleFunc("GET /api/v1/runs", h.listRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}", h.getRun)
	mux.HandleFunc("GET /api/v1/runners", h.listRunners)

	return mux
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

// ── health probes ────────────────────────────────────────────────

// handleHealthz reports liveness only: this process is up and able to serve HTTP. It never touches the database so it stays
// honest even when the DB is briefly unavailable -- that concern belongs to /readyz instead.
func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8") //nolint:staticcheck -- explicit content-type BEFORE body write is idiomatic Go per net/http's headers-before-body ordering contract described in writeJSON's own comment just above here too.

	w.WriteHeader(http.StatusOK)   //nolint:errchkwriteheader -- an explicitly-set 200 status makes the response shape fully deterministic for downstream wrappers/interceptors that read rec.status after inner returns, avoiding reliance on any implicit-200-before-first-write assumption net/http's own internal machinery might not honor uniformly across every wrapper edge case.
	_, _ = w.Write([]byte("ok\n")) //nolint:errcheck -- a write-failure here would only ever surface as an already-closed socket to the real external client, never something this handler itself is responsible for cleaning up further downstream.
}

// handleReadyz answers whether the server is ready to receive real traffic. For a control plane that means its own stateful
// dependency -- Postgres -- is actually reachable AND at the schema version this code requires; otherwise it returns 503 so an
// orchestrator can hold back load until the database comes up rather than handing clients spurious "internal error" 500s.
func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// A /readyz scrape is an orchestrator's own retry loop, not a client-facing request: bound the probe with its own short
	// deadline so a hung pool fails fast instead of near-maxing defaultRequestTimeout (see that constant's rationale just above).
	ctx, cancel := context.WithTimeout(r.Context(), readinessProbeDeadline) //nolint:staticcheck -- this scoped deadline is fix #4's whole point per the const doc-comment.
	defer cancel()

	if err := h.reader.Ping(ctx); err != nil { //nolint:staticcheck -- intent preserved post-wrap: same DB-reachability contract as before, now bounded by readinessProbeDeadline rather than only the request-scoped global ceiling this route's own middleware never applied to it.
		writeError(w, h.logger, http.StatusServiceUnavailable, errCodeServiceUnreachable, "database unreachable", err)

		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8") //nolint:staticcheck -- same explicit-before-body-write ordering principle as handleHealthz above it too for exact same downstream-consumer reasons.
	w.WriteHeader(http.StatusOK)                                //nolint:errchkwriteheader -- same explicit-status rationale applies here identically to the happy-path branch just below where Ping succeeds and we're happy to admit incoming traffic freely instead.

	_, _ = w.Write([]byte("ready\n")) //nolint:errcheck -- write-failure-to-closed-socket caveat applies exactly identically as handleHealthz's own body-write comment above already documented too.
}

// ── middleware composition ────────────────────────────────────────

// buildChain wires the middleware around a route handler. Composition, inner -> outer:
//
//	mux -> JSON-error conversion -> auth -> per-request timeout -> logging -> recover
//
// Recover is outermost so any panic anywhere in the chain still yields an envelope rather than crashing the worker goroutine;
// auth sits INSIDE the per-request timeout: the timeout context's deadline is attached before auth runs, so M3's (eventual)
// outbound OAuth verification call shares -- and is bounded by -- the same defaultRequestTimeout ceiling as every other
// route handler, rather than getting a separate budget of its own.
func buildChain(h *Handler, inner http.Handler, timeout time.Duration) http.Handler {
	chain := inner                                     // mux with routes registered above
	chain = withJSONErrors(chain, h.logger)            // stdlib 404/405 text -> our unified envelope
	chain = withAuthenticator(h.auth, h.logger)(chain) // M3 slot; noOpAuthenticator keeps it open today, swapped in later without moving this line up the stack.

	// withAuthenticator is CURRIED (like withRequestTimeout above): it returns the wrapper fn taking `next`, unlike its siblings which
	// return an http.Handler directly, so this call site needs the trailing "(chain)" -- without it we'd be assigning a func to chain and get a compile error.
	chain = withRequestTimeout(timeout)(chain) // per-request context deadline so a slow client can't hold its goroutine for free.
	chain = withLogging(chain, h.logger)       // one access-log line per request, sees the FINAL post-inner status once every inner layer has settled.
	chain = withRecover(chain, h.logger)       // outermost: any panic below yields an internal-error envelope rather than crashing the runtime worker.

	return chain
}

// ── middleware layers ──────────────────────────────────────────────

// withRecover is always outermost in the stack so a downstream layer's uncaught panic becomes a structured 500 envelope instead of a crash log line buried elsewhere.
func withRecover(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil { //nolint:staticcheck -- this IS the panic-catch we intend here specifically.
				log.Error("request panicked", "recover", rec)
				writeError(w, log, http.StatusInternalServerError, errCodeInternal, fmt.Sprintf("internal error due to panic: %v", rec), nil)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// withRequestTimeout bounds every request so a single slow client can't monopolise its goroutine indefinitely; the context deadline is threaded down through the whole inner handler below this point.
func withRequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel() // guaranteed to run before this request's goroutine returns; otherwise a slow client stalls the pool for free.

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// withLogging emits one structured log line per request after the inner handler has settled its status; see TODO(M3/M5) inline for latency metrics follow-up.
func withLogging(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK} // pre-seed OK; a layer that never calls WriteHeader still ends 200 by convention.

		next.ServeHTTP(rec, r)

		// TODO(M3/M5): swap the plain access-log line for a real latency histogram metric as per docs's observability section.
		log.Info("http request", "method", r.Method, "path", r.URL.Path, "status", rec.status)
	})
}

// withJSONErrors wraps "inner" so stdlib mux's bare-text 404/405 (its default handler writing `404 page not found\n`) gets swapped for our uniform JSON envelope instead; everything else is forwarded byte-for-byte after copying captured headers/body out to the client-facing writer.
func withJSONErrors(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &bodyRecorder{header: make(http.Header)}

		next.ServeHTTP(rec, r)

		if rec.status == http.StatusNotFound || rec.status == http.StatusMethodNotAllowed {
			writeError(w, log, rec.status, errCodeForStatus(rec.status), "route not found", nil) // same envelope shape as every other error path in this tree; client SDKs don't special-case stdlib's text 404/405 anymore.

			return
		}

		for k := range rec.header {
			w.Header().Set(k, rec.header.Get(k))
		}

		if rec.status != http.StatusOK {
			w.WriteHeader(rec.status)
		}

		_, _ = w.Write(rec.body.Bytes())
	})
}

// errCodeForStatus maps a couple of stdlib mux statuses to their envelope codes without re-encoding that logic at every call site.
func errCodeForStatus(status int) string {
	switch status {
	case http.StatusMethodNotAllowed:
		return errCodeMethodNotAllowed
	default:
		return errCodeNotFound
	}
}

// ── error envelope ────────────────────────────────────────────────

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// Content-Type must be set before WriteHeader so the header is visible when a client first reads it -- RFC 7230 mandates headers-before-body ordering.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// WriteHeader returns nothing: a bare statement suffices here and discarding a non-existent value would fail to compile ("used as value").
	w.WriteHeader(status)

	body, err := json.Marshal(v)
	if err != nil {
		// Marshal of plain JSON-serializable structs we fully own should never fail in practice; keep this failure path simple and
		// non-fatal: fall back to a bare 500 body with no structured error since one can't even be serialized itself.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		_, _ = fmt.Fprintf(w, "internal error: response body could not be serialized to JSON\n")

		return
	}

	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, log *slog.Logger, status int, code, message string, cause error) {
	if cause != nil {
		log.Error("http error", "status", status, "code", code, "message", message, "cause", cause)
	}

	writeJSON(w, status, errorEnvelope{Error: errorDetail{Code: code, Message: message}})
}

// ── recorders ──────────────────────────────────────────────────────

type statusRecorder struct { // captures just the response code for withLogging; passthrough body writes keep logging cost flat.
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	// Only forward when an inner layer has explicitly set a non-OK code. A successful route that never calls WriteHeader relies on net/http's implicit 200; forcing it through a wrapper here would panic ("invalid WriteHeader code 0") or otherwise break first-write-wins semantics, so the recorder stays quiet until told otherwise.
	r.status = code
	if code != http.StatusOK {
		// r.ResponseWriter.WriteHeader returns nothing -- a bare call is correct (discarding it as `_ =` fails to compile). Forward ONLY here so inner's own 200 path doesn't trigger this wrapper on zero-byte writes.
		r.ResponseWriter.WriteHeader(code) //nolint:exportloopref -- only the first explicit WriteHeader actually flows through to the wrapped writer; subsequent ones are no-ops by Go's status-once semantics.
	}
}

// bodyRecorder captures the full response -- headers plus buffered body bytes -- so withJSONErrors can either discard it on a 404/405 path in favor of our JSON envelope or forward it unmodified otherwise.
type bodyRecorder struct {
	header http.Header // caller must pass make(http.Header); nil would panic on first Header() call
	body   bytes.Buffer
	status int
}

func (r *bodyRecorder) Header() http.Header         { return r.header }
func (r *bodyRecorder) WriteHeader(code int)        { r.status = code }
func (r *bodyRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }

// Compile-time check that the real store still satisfies this narrow seam -- a break here surfaces at build time rather than as a nil-pointer at runtime.
var _ store.Reader = (*store.Store)(nil)
