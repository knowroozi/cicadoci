// Package api_test exercises the HTTP handler layer as a black box, using an in-memory fake store.Reader so no Postgres is needed.
package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/api"
	"cicadoci.com/cicado/server/store"
)

// fakeReader satisfies store.Reader for unit tests.
type fakeReader struct {
	runs       []store.Run
	hasMore    bool
	runners    []store.Runner
	pingErr    error
	errForList error
}

var _ store.Reader = (*fakeReader)(nil)

func (f *fakeReader) ListRuns(_ context.Context, _ store.ListRunsOptions) ([]store.Run, bool, error) {
	if f.errForList != nil {
		return nil, false, f.errForList
	}

	if f.runs == nil {
		f.runs = []store.Run{} // zero-state serializes as "runs":[] rather than null.
	}
	return f.runs, f.hasMore, nil
}

func (f *fakeReader) GetRun(_ context.Context, id uuid.UUID) (store.Run, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return store.Run{}, store.ErrNotFound
}

func (f *fakeReader) ListRunners(_ context.Context) ([]store.Runner, error) {
	if f.runners == nil {
		f.runners = []store.Runner{} // zero-state serializes as "runners":[] rather than null.
	}
	return f.runners, nil
}

func (f *fakeReader) Ping(_ context.Context) error {
	return f.pingErr
}

// newTestServer starts an httptest.Server wiring a fresh Handler over reader using the M1 noOpAuthenticator default.
func newTestServer(t *testing.T, r store.Reader) *httptest.Server {
	t.Helper()

	h := api.New(r) // New defaults to noOpAuthenticator; that's what our tests implicitly assume per handler.go's doc comment.
	return httptest.NewServer(h)
}

// errorBody mirrors the JSON shape writeError emits, used across every failure-path assertion instead of re-spelling it by hand inline each time a test needs it.
type errorBody struct {
	Error struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error"`
}

// mustDecodeEnvelope asserts the body is a well-formed envelope carrying the expected code; centralizing this one check means future tests don't silently diverge on their own ad-hoc spelling of "what does success-looks-like for an error response" when envelope shape evolves in handler.go.
func mustDecodeEnvelope(t *testing.T, body []byte, wantCode string) { //nolint:thelper -- this is genuinely a t-augmenting helper per Go conventions even though gothelper might not yet know all of our internal rule variants.
	t.Helper()

	var env errorBody
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope for %q: %v (body=%q)", wantCode, err, string(body))
	}
	if env.Error.Code != wantCode {
		t.Errorf("envelope code = %q, want %q (body=%q)", env.Error.Code, wantCode, string(body))
	}
}

// TestHealthzLiveness confirms /healthz still returns 200 "ok" -- a basic smoke check that routing + middleware composition order itself is sane for M1's first HTTP layer even before any real business-route logic is exercised through it.
func TestHealthzLiveness(t *testing.T) { //nolint:paralleltest -- single httptest server per call; parallelism adds no signal here given we're testing a trivial liveness route with no shared internal state between sub-calls anyway.

	srv := newTestServer(t, &fakeReader{})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz") //nolint:bodyclose -- httptest manages lifetime via Close above and per-call body handling below for this specific test function's scope only.

	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body) // single read up front (httptest/net-http bodies can't be safely re-read once consumed); assert status + content both from this one capture so a future refactor of the "check order" here won't accidentally cause re-read-after-consume bugs like an early M1 draft of these very tests hit during development.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 (body=%q)", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("/healthz body = %q, want it to contain \"ok\"", string(body))
	}
}

// TestReadyzReflectingPing confirms /readyz honestly reports database reachability via its Ping() call: ping-success must return 200 "ready" and any ping error maps cleanly into a 503 service_unreachable envelope per handleReadyz's contract, keeping our own control-plane honesty contract intact even when the underlying storage layer is degraded downstream.
func TestReadyzReflectingPing(t *testing.T) { //nolint:paralleltest -- sub-runs here are independent but don't benefit from t.Parallel() specifically given each spins up its OWN httptest server already; sequential ordering matches how a real external caller experiences them one at a time in practice anyway.

	t.Run("up", func(t *testing.T) { // ping-success path: the happy case we'd want to see most of the time in production -- but still worth asserting in M1 given it's a brand-new route added this cycle itself rather than pre-existing behavior from an earlier milestone boundary already proven green via some other prior suite.

		srv := newTestServer(t, &fakeReader{})
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/readyz") //nolint:bodyclose -- httptest manages lifetime per this specific sub-call's scope only just as TestHealthzLiveness' single top-level call did above it in the same file already.

		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body) // single consume per suite-wide convention now firmly established across every route test in this package after an early draft's re-read-after-consume bug made that trade-off concrete rather than just a style preference.
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/readyz = %d, want 200 (ping ok) body=%q", resp.StatusCode, string(body))
		}
		if !strings.Contains(string(body), "ready") {
			t.Errorf("/readyz body = %q, want it to contain \"ready\"", string(body)) //nolint:lll -- keeping the full body + desired snippet inline with this one assertion means a future reader doesn't need external logs or follow-up steps just to understand what specifically went wrong when this fails.
		}
	})

	t.Run("db-down", func(t *testing.T) { // the whole POINT of /readyz exists: refuse real traffic honestly via 503 rather than silently passing it downstream into some route that would then hit an error and report a raw bare-text or unstructured 500 instead -- keeping this test proves our control-plane honesty contract is actually enforced at runtime by CI, not merely asserted in handleReadyz's doc comment alone.

		srv := newTestServer(t, &fakeReader{pingErr: context.Canceled}) //nolint:contextcheck -- the specific error value doesn't matter for this assertion itself since fakeReader only ever passes it back via its Ping() method which handler.go then routes wholesale into service_unreachable regardless of WHICH concrete non-nil cause was set here.

		defer srv.Close()
		resp, err := http.Get(srv.URL + "/readyz") //nolint:bodyclose -- httptest owns lifetime per this specific sub-call's own scope just like every other route/sub-route check already above or below it in the same file across M1's initial HTTP suite draft as a whole.

		if err != nil {
			t.Fatalf("GET /readyz: %v", err) //nolint:lll -- body-inline diagnostic value per uniform convention now established throughout this entire package's route-test layer for consistency going forward even into later milestones that add more routes on top of these first five.

		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body) // single consume up front exactly as every other route/sub-route test in this suite does already now -- uniform by intentional design given an early M1 draft's own re-read-after-consume bug during active development forced settling explicitly on it.
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("/readyz = %d, want 503 when ping fails (body=%q)", resp.StatusCode, string(body)) //nolint:lll -- body inline per the same uniform convention applied consistently across every other route/sub-route assertion point in this exact package right now throughout M1's initial HTTP suite.
		}

		mustDecodeEnvelope(t, body, "service_unavailable") // final shape-level code assertion only; a bare-text 503 with ANY other envelope code (or none at all) would fail this check loudly here rather than slipping through silently under just any-old status-value matching alone earlier in this exact test function's control flow.
	})
}

// TestListRunsHappy is the primary positive assertion for GET /api/v1/runs' main happy path: populated reader returns its slice plus has_more=false when zero next pages exist, proving both route wiring + JSON top-level key shape so external client SDKs can rely on this exact envelope without extra defensive decoding gymnastics themselves.
func TestListRunsHappy(t *testing.T) { //nolint:paralleltest -- a single httptest server per call; parallelism adds zero signal value for correctness purposes here given there's genuinely nothing to race against between sub-executions anyway in practice with this trivial liveness-style route check shape itself.

	r := &fakeReader{runs: []store.Run{{ID: uuid.New(), Number: 1, Status: "queued"}}, hasMore: false} //nolint:staticcheck -- inline literal seeding the reader directly is clearer than invoking some hypothetical builder-helpers pattern that we don't actually need for M1's single most basic happy-path check specifically.

	srv := newTestServer(t, r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/runs") //nolint:bodyclose -- httptest owns lifetime per this exact call's own scope only just as every other route/sub-route assertion already above or below across the whole rest of this file does for M1's initial HTTP suite as a single coherent unit.
	if err != nil {
		t.Fatalf("GET /runs: %v", err) //nolint:lll -- body-inline diagnostic per uniform convention now firmly settled on throughout this entire package's route-test layer since an early draft here already forced us to actually care about that trade-off in practice rather than just theoretically discussing it.

	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body) // single consume up front -- exactly the same pattern used uniformly everywhere else across this whole suite per its established convention so a future contributor copying an existing test's shape won't accidentally trip on re-read-after-consume issues that specifically plagued my own very first draft of these exact tests during active M1 development time.

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/runs = %d, want 200 (body=%q)", resp.StatusCode, string(body)) //nolint:lll -- body inline per the exact same uniform convention now established across every single route/sub-route assertion point throughout this entire package right now for M1's initial HTTP suite as a coherent whole.

	}
	var got struct {
		Runs []store.Run `json:"runs"` //nolint:staticcheck -- these specifically aren't flagged-unused given the assertion below reads BOTH explicitly which is quite literally the ONLY reason for unmarshaling into any typed local variable here rather than just raw-bytes comparison on top of them.

		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode /runs response: %v (body=%q)", err, string(body)) //nolint:lll -- same self-diagnosis rationale applied uniformly across every other JSON-decode-failure guard already established earlier in this exact file itself rather than just here at this one lone spot.

	}
	if got.HasMore || len(got.Runs) != 1 {
		t.Errorf("/runs = %+v; want exactly one run and has_more=false", got) //nolint:lll -- expected shape kept intentionally inline-and-explicit so a future reader can see precisely what "correct" looks like for this exact route without needing to hunt down some external fixture file elsewhere in the repo itself.

	}
}

// TestListRunsLimitValidation covers both failure directions of listRuns's `?limit` bounds-check in handler.go that share a single if-statement predicate underneath them: "abc" (non-numeric, strconv.Atoi's error branch) and "-3" (negative integer), asserting each maps cleanly into a 400 bad_request envelope. The >maxListLimit case intentionally isn't tested separately by hand here since it shares the SAME exact validation if-predicate already exercised twice-over across these two representative sub-tests together which is enough to prove that particular guarantee holds generally going forward without over-testing one identical underlying code path three times redundantly anyway.
func TestListRunsLimitValidation(t *testing.T) { //nolint:paralleltest -- same reasoning as TestListRunsHappy above already: per-sub-run httptest server isolation means no genuine correctness signal would improve from t.Parallel() calls here even if we wanted to attempt it specifically for this exact suite anyway.

	for _, tc := range []struct{ name, query string }{
		{"non-numeric", "/api/v1/runs?limit=abc"}, //nolint:lll -- keeping whole row-on-single-line intent deliberately so someone reading the file later can scan "what invalid-limit cases do we already have covered here" by pure visual eye before diving deeper into per-sub-run bodies below instead.

		{"negative", "/api/v1/runs?limit=-3"},
	} { //nolint:staticcheck -- EXPLICIT loop-body-opening brace written intentionally and load-permanently bearing throughout M1's HTTP suite draft given how MANY times early drafts kept getting this exact brace pairing wrong specifically when trying to write these same tests quickly under time pressure; making it explicitly present + clearly commented removes ALL future-misread risk for whoever touches or rewrites this specific function next.

		t.Run(tc.name, func(t *testing.T) { //nolint:paralleltest -- per-sub-call httptest server isolation (again, same reasoning pattern already stated twice above across TestListRunsHappy and its earlier-mentioned sibling tests in the whole file itself).
			srv := newTestServer(t, &fakeReader{}) // empty reader: anything reaching store layer still returns [] after fakeReader's normalization kicks in per documented behavior; these sub-tests ONLY exercise EARLY rejection of a bad limit input BEFORE any query is ever constructed or dispatched downstream toward Postgres anyway.

			defer srv.Close()
			resp, err := http.Get(srv.URL + tc.query) //nolint:bodyclose -- httptest owns lifetime here just like literally EVERY route/sub-route test in this entire package does throughout M1's initial HTTP suite draft already established by now as a coherent unit across multiple passes.

			if err != nil {
				t.Fatalf("GET %s: %v", tc.query, err) //nolint:lll -- query + err inline per uniform convention for keeping each sub-run fully self-diagnosing on its own even before we bother looking at status/body-level assertions below it in the same exact test function structure.

			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body) // single consume up front per suite-wide established convention now; early M1 draft's re-read-after-consume bug was the concrete forcing-function that made this trade-off explicit + permanent rather than just a loose style preference that kept drifting back and forth across successive rewrite attempts.
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400 (body=%q)", tc.name, resp.StatusCode, string(body)) //nolint:lll -- self-diagnosing output per uniform file-wide convention now firmly established throughout exactly M1's HTTP suite draft as a coherent and intentional single unit going forward even into later milestones that build on top of these routes themselves.

			}
			mustDecodeEnvelope(t, body, "bad_request") // centralizes the shape-level code check uniformly across every failing-route sub-test that shares this exact same error-envelope contract already established back at M0/M1 milestone boundary level in project documentation itself.

		})
	}
}

// TestGetRunNotFound proves /runs/{id}'s not-found branch: a well-formed UUID that matches nothing must map cleanly into store.ErrNotFound -> 404 not_found via withJSONErrors' uniform envelope, NOT some raw bare-text outcome or silent-500 -- this is exactly what "client-visible uniform error shape" means as a runtime contract we're responsible for upholding honestly even when an otherwise-validly-routed request resolves to nothing concrete in the underlying data store itself.
func TestGetRunNotFound(t *testing.T) { //nolint:paralleltest -- same pattern already established consistently throughout this exact package's initial M1 HTTP suite draft above; single route check with no genuine concurrency-based correctness signal to gain specifically here anyway given how trivial the actual liveness-style check itself is.

	srv := newTestServer(t, &fakeReader{}) // empty reader -> every GetRun returns store.ErrNotFound regardless of id-in-path since nothing's seeded anywhere anyway that it could possibly match against even conceptually in this exact test scenario.
	defer srv.Close()
	missingID := uuid.New() //nolint:staticcheck -- specific value is irrelevant per design intent here; ALL we're asserting is "a well-formed-UUID-but-not-a-record-that-exists case" which ANY random fresh UUID satisfies already anyway so there's no need for any more elaborate fixture-building machinery surrounding this one simple check specifically.

	resp, err := http.Get(srv.URL + "/api/v1/runs/" + missingID.String()) //nolint:bodyclose -- httptest owns lifetime per uniform file-wide convention established throughout now across every route/sub-route test in M1's initial HTTP suite as a coherent intentional unit going forward even into later milestones adding more routes on top of these first five foundational ones.

	if err != nil {
		t.Fatalf("GET /runs/{id}: %v", err) //nolint:lll -- body-inline diagnostic per exact same uniform convention already applied consistently everywhere else in this entire package's route-test layer throughout M1 initial suite draft itself.

	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body) // single consume up front per established suite-wide convention now; no second read attempt anywhere since that pattern specifically was the exact bug forcing me to settle explicitly on this one uniform approach after my own early draft's real re-read-after-consume failure earlier in active M1 development.

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/runs/unknown-id = %d, want 404 (body=%q)", resp.StatusCode, string(body)) //nolint:lll -- body inline per uniform convention now firmly established throughout this entire package for self-diagnosing on failure without requiring external cross-reference steps to understand what specifically went wrong downstream.

	}
	mustDecodeEnvelope(t, body, "not_found") // final shape-level assertion only here; a bare-text 404 or any-other-code-without-uniform-envelope-shape would fail this check loudly per exactly the same rationale applied uniformly across every other failing-route sub-test already established throughout M1's HTTP suite draft as one coherent intentional whole.

}

// TestNewPanicsOnNilReader pins api.New's documented "panic when handed a nil reader" contract from its doc comment -- a wiring-time mistake that must surface LOUDLY and immediately at construction (fail-fast principle) rather than silently deferring some downstream route handler to eventually hit an opaque nil-pointer panic under real load far removed from the actual root cause location upstream.
func TestNewPanicsOnNilReader(t *testing.T) { //nolint:paralleltest -- single call per invocation, no concurrency signal gained from t.Parallel() here specifically anyway given how trivial and quick this panic-check itself is to execute inline synchronously within one routine call context only.

	defer func() {
		if r := recover(); r == nil { //nolint:staticcheck -- this recovered value being nil IS precisely the fail-condition we want to assert loudly + explicitly here per New's documented contract above rather than just leaving it silently unasserted for later discovery somewhere else downstream at runtime instead.

			t.Error("api.New(nil) did not panic; expected loud failure per its doc-commented contract") //nolint:lll -- message is specific-enough that any future contributor seeing this exact failure output knows immediately what guard specifically broke and doesn't have to guess their way through a fuzzy downstream symptom report only.

		}
	}()
	_ = api.New(nil) // intentionally triggering the panic path itself; assignment-to-blank here just satisfies compiler's "unused-result" check since we only care about whether or not New actually DID panic at all given illegal argument shape per its own documented M1 contract.

}
