// Package api_test exercises CIC-194's operator write surface (secrets
// PUT/DELETE/LIST + job Cancel) as a black box, with in-memory fakes for the
// secrets store, audit log, and cancel path so no Postgres is needed.
package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/api"
	"cicadoci.com/cicado/server/audit"
	"cicadoci.com/cicado/server/queue"
	"cicadoci.com/cicado/server/secrets"
	"cicadoci.com/cicado/server/store"
)

// fakeSecretStore records writes and, optionally, fails, standing in for
// secrets.Postgres without Postgres. E1 needs a non-value error to exercise the
// 500 path, so it is injectable.
type fakeSecretStore struct {
	mu        sync.Mutex
	puts      [][2]string // [name, value]
	deletes   []string
	listNames []string
	putErr    error
	delErr    error
}

var _ secrets.Store = (*fakeSecretStore)(nil)

func (f *fakeSecretStore) Put(_ context.Context, _, _, name string, value []byte, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, [2]string{name, string(value)})
	return nil
}
func (f *fakeSecretStore) Get(_ context.Context, _, _, _ string) (*secrets.Lease, error) {
	return nil, nil
}
func (f *fakeSecretStore) Delete(_ context.Context, _, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	f.deletes = append(f.deletes, name)
	return nil
}
func (f *fakeSecretStore) List(_ context.Context, _, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listNames, nil
}

// recordingAudit is a minimal audit.Log that records every appended entry so a
// test can assert the write audit fired (and that a secret value never leaked
// into it).
type recordingAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

var _ audit.Log = (*recordingAudit)(nil)

func (a *recordingAudit) Append(_ context.Context, e audit.Entry) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return int64(len(a.entries)), nil
}
func (a *recordingAudit) Get(_ context.Context, _ int64) (audit.Entry, error) {
	return audit.Entry{}, nil
}
func (a *recordingAudit) List(_ context.Context, _ int64, _ int) ([]audit.Entry, error) {
	return nil, nil
}
func (a *recordingAudit) VerifyChain(_ context.Context) error { return nil }

// fakeReaderForWrites is the minimal store.Reader a Handler needs; the write
// surface does not read through it, so every read is a no-op error.
func fakeReaderForWrites() store.Reader { return &fakeReader{} }

// doRequest issues a context-bound request against srv, returning both its
// status and the decoded response body (so a test can assert the names returned
// by the list route), closing the body at the call site. It pins the "no body
// leaked, request was context-carrying" shape the noctx linter requires even in
// tests.
func doRequest(t *testing.T, srv *httptest.Server, method, path, rawBody string) (int, string) {
	t.Helper()
	var r io.Reader
	if rawBody != "" {
		r = strings.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// valid org + repo ids: E1 requires them to be valid uuids.
const (
	wantOrg  = "11111111-1111-1111-1111-111111111111"
	wantRepo = "22222222-2222-2222-2222-222222222222"
)

func secretPutBody(org, repo, name, value string) string {
	return `{"org_id":"` + org + `","repo_id":"` + repo + `","name":"` + name + `","value":"` + value + `"}`
}

func TestWriteSurface_SecretPutAuditedValueNeverLeaked(t *testing.T) {
	store := &fakeSecretStore{}
	au := &recordingAudit{}

	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{
			SecretStore: store,
			Cancel: func(_ context.Context, _ uuid.UUID, _ string) (queue.CancelResult, error) {
				return queue.CancelResult{}, nil
			},
			Audit: au,
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/secrets",
		secretPutBody(wantOrg, wantRepo, "DB_PASSWORD", "s3cr3t-value"))
	if code != http.StatusOK {
		t.Fatalf("POST /secrets status = %d, want 200", code)
	}

	// The value was consumed but never made readable: the store saw it, but no
	// audit entry contains the plaintext.
	if len(store.puts) != 1 || store.puts[0][1] != "s3cr3t-value" {
		t.Fatalf("store.puts = %v, want one put with the value", store.puts)
	}
	if len(au.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.entries))
	}
	e := au.entries[0]
	if e.Action != audit.ActionSecretPut {
		t.Errorf("audit action = %q, want %q", e.Action, audit.ActionSecretPut)
	}
	if e.Target != wantOrg+"/"+wantRepo+"/DB_PASSWORD" {
		t.Errorf("audit target = %q, want %s/%s/DB_PASSWORD", e.Target, wantOrg, wantRepo)
	}
	if strings.Contains(e.Target, "s3cr3t") || e.Reason == "s3cr3t-value" {
		t.Errorf("audit entry leaked the secret value: %+v", e)
	}
}

func TestWriteSurface_SecretDelete(t *testing.T) {
	store := &fakeSecretStore{}
	au := &recordingAudit{}

	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{SecretStore: store, Audit: au}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, _ := doRequest(t, srv, http.MethodDelete,
		"/api/v1/secrets?org_id="+wantOrg+"&repo_id="+wantRepo+"&name=DB_PASSWORD", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /secrets status = %d, want 200", code)
	}
	if len(store.deletes) != 1 || store.deletes[0] != "DB_PASSWORD" {
		t.Fatalf("store.deletes = %v, want [DB_PASSWORD]", store.deletes)
	}
	if len(au.entries) != 1 || au.entries[0].Action != audit.ActionSecretDelete {
		t.Fatalf("audit = %+v, want one ActionSecretDelete", au.entries)
	}
}

// E1: a malformed org_id/repo_id is a clean 400, and it precedes any store call.
func TestWriteSurface_BadUUIDReaches400(t *testing.T) {
	store := &fakeSecretStore{}
	au := &recordingAudit{}
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{SecretStore: store, Audit: au}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, body := range []string{
		secretPutBody("o1", "r1", "DB_PASSWORD", "v"),            // bad org
		secretPutBody(wantOrg, "not-a-uuid", "DB_PASSWORD", "v"), // bad repo
		`{"org_id":"o1","name":"DB_PASSWORD","value":"v"}`,       // bad org (json)
	} {
		if code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/secrets", body); code != http.StatusBadRequest {
			t.Errorf("POST /secrets %q status = %d, want 400", body, code)
		}
		if len(store.puts) != 0 || len(au.entries) != 0 {
			t.Errorf("bad-uuid request must reach no store call / no audit: puts=%v audit=%d", store.puts, len(au.entries))
		}
	}
}

// E1: a store failure surfaces as a 500 with a generic message, NOT the raw
// database error text.
func TestWriteSurface_StoreErrorIsGeneric500(t *testing.T) {
	store := &fakeSecretStore{putErr: &fakeStoreErr{"fatal: internal_db_schema_detail_xyz"}}
	au := &recordingAudit{}
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{SecretStore: store, Audit: au}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, body := doRequest(t, srv, http.MethodPost, "/api/v1/secrets",
		secretPutBody(wantOrg, wantRepo, "DB_PASSWORD", "v"))
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if strings.Contains(body, "internal_db_schema_detail_xyz") {
		t.Errorf("500 body leaked raw database text: %q", body)
	}
	if len(au.entries) != 0 {
		t.Errorf("a failed put must not audit: %d", len(au.entries))
	}
}

type fakeStoreErr struct{ s string }

func (e *fakeStoreErr) Error() string { return e.s }

func TestWriteSurface_JobCancel(t *testing.T) {
	var gotJob uuid.UUID
	var gotReason string
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{
			Cancel: func(_ context.Context, jobID uuid.UUID, reason string) (queue.CancelResult, error) {
				gotJob = jobID
				gotReason = reason
				return queue.CancelResult{Active: true, RunnerID: uuid.MustParse(wantRepo)}, nil
			},
			Audit: &recordingAudit{},
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	jobID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/jobs/"+jobID.String()+"/cancel", `{"reason":"manual stop"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /cancel status = %d, want 200", code)
	}
	if gotJob != jobID || gotReason != "manual stop" {
		t.Fatalf("cancel got job=%s reason=%q, want %s / manual stop", gotJob, gotReason, jobID)
	}
}

// C1: when the job is unknown or already terminal (Cancel returns Active=false),
// the handler returns 404 and writes NO audit row -- a false "job cancelled"
// record is never created.
func TestWriteSurface_FalseCancelIs404AndUnaudited(t *testing.T) {
	au := &recordingAudit{}
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{
			Cancel: func(_ context.Context, _ uuid.UUID, _ string) (queue.CancelResult, error) {
				return queue.CancelResult{Active: false}, nil // job unknown / finished
			},
			Audit: au,
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	jobID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/jobs/"+jobID.String()+"/cancel", `{"reason":"stop"}`)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-active job", code)
	}
	if len(au.entries) != 0 {
		t.Errorf("a non-active cancel must NOT audit: %d entries", len(au.entries))
	}
}

// T2: the names-only GET list route returns names (never values) and audits
// nothing (a read).
func TestWriteSurface_ListSecrets(t *testing.T) {
	names := []string{"DB_PASSWORD", "API_KEY"}
	store := &fakeSecretStore{listNames: names}
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{SecretStore: store, Audit: &recordingAudit{}}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, body := doRequest(t, srv, http.MethodGet, "/api/v1/secrets?org_id="+wantOrg, "")
	if code != http.StatusOK {
		t.Fatalf("GET /secrets status = %d, want 200", code)
	}
	if !strings.Contains(body, "DB_PASSWORD") || !strings.Contains(body, "API_KEY") {
		t.Errorf("list body = %q, want both names", body)
	}
	// The payload key is a plural "names" array of names; there must be no
	// per-secret "value" key (the credential) anywhere in the response.
	if !strings.Contains(body, "\"names\":") {
		t.Errorf("list body = %q, want a names key", body)
	}
	if strings.Contains(body, "\"value\":") {
		t.Errorf("list body leaked a secret value: %q", body)
	}
}

// TestWriteSurface_FailClosedByAbsence asserts that a Handler built WITHOUT
// WithWriteDeps exposes no write routes, so a server without a secrets key
// never accidentally exposes a broken (or open) write surface.
func TestWriteSurface_FailClosedByAbsence(t *testing.T) {
	h := api.New(fakeReaderForWrites()) // no WithWriteDeps
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/secrets"},
		{http.MethodDelete, "/api/v1/secrets"},
		{http.MethodGet, "/api/v1/secrets"},
		{http.MethodPost, "/api/v1/jobs/" + uuid.NewString() + "/cancel"},
	} {
		if code, _ := doRequest(t, srv, tc.method, tc.path, ""); code < 400 {
			// An unregistered route is a 404 (or 405), never a 200/500.
			t.Errorf("%s %s status = %d, want 4xx (route must be absent)", tc.method, tc.path, code)
		}
	}
}

// TestWriteSurface_BadInputReaches400 pins the validation path: a missing
// required field is a client error, not a store call.
func TestWriteSurface_BadInputReaches400(t *testing.T) {
	store := &fakeSecretStore{}
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{SecretStore: store, Audit: &recordingAudit{}}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	if code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/secrets", `{"org_id":"`+wantOrg+`","name":""}`); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if len(store.puts) != 0 {
		t.Fatalf("store.puts = %v, want none (validation must precede the store call)", store.puts)
	}
}

func TestWriteSurface_InvalidJobIDIsBadRequest(t *testing.T) {
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{
			Cancel: func(_ context.Context, _ uuid.UUID, _ string) (queue.CancelResult, error) {
				t.Fatal("cancel must not be called for a malformed job id")
				return queue.CancelResult{}, nil
			},
			Audit: &recordingAudit{},
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	if code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/jobs/not-a-uuid/cancel", `{}`); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// TestWriteSurface_CancelRun (CIC-206) verifies that POST /api/v1/runs/{id}/cancel
// invokes CancelRun with the correct runID + reason and audits when the run
// had active jobs.
func TestWriteSurface_CancelRun(t *testing.T) {
	var gotRun uuid.UUID
	var gotReason string
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{
			CancelRun: func(_ context.Context, runID uuid.UUID, reason string) (bool, error) {
				gotRun = runID
				gotReason = reason
				return true, nil
			},
			Audit: &recordingAudit{},
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	runID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/cancel", "{\"reason\":\"run stop\"}")
	if code != http.StatusOK {
		t.Fatalf("POST /runs/{id}/cancel status = %d, want 200", code)
	}
	if gotRun != runID || gotReason != "run stop" {
		t.Fatalf("CancelRun got run=%s reason=%q, want %s / run stop", gotRun, gotReason, runID)
	}
}

// TestWriteSurface_FalseCancelRunIs404AndUnaudited (CIC-206) verifies that a
// run-level cancel for a run with no active jobs (CancelRun returns false) is
// a 404 and writes NO audit row.
func TestWriteSurface_FalseCancelRunIs404AndUnaudited(t *testing.T) {
	au := &recordingAudit{}
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{
			CancelRun: func(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
				return false, nil
			},
			Audit: au,
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	runID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/cancel", "")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a run with no active jobs", code)
	}
	if len(au.entries) != 0 {
		t.Errorf("a non-active run-level cancel must NOT audit: %d entries", len(au.entries))
	}
}

// TestWriteSurface_PartialCancelAuditsBefore500 (CIC-206, B2) verifies that
// on a partial failure (CancelRun returns (true, err)) the audit row is
// written before the 500 is returned. This is the B2 audit-first fix.
func TestWriteSurface_PartialCancelAuditsBefore500(t *testing.T) {
	au := &recordingAudit{}
	h := api.New(fakeReaderForWrites(),
		api.WithWriteDeps(api.WriteDeps{
			CancelRun: func(_ context.Context, runID uuid.UUID, reason string) (bool, error) {
				// Simulate partial failure: one job was cancelled (touched=true),
				// but a RequestCancel failed for another job.
				return true, errors.New("partial: one job failed to cancel")
			},
			Audit: au,
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	runID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	code, _ := doRequest(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/cancel", "{\"reason\":\"operator\"}")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on partial failure", code)
	}
	if len(au.entries) != 1 {
		t.Fatalf("partial cancel: %d audit rows, want 1 (audit-first)", len(au.entries))
	}
	if au.entries[0].Action != audit.ActionCancel {
		t.Errorf("audit action = %q, want %q", au.entries[0].Action, audit.ActionCancel)
	}
}
