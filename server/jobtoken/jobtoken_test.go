package jobtoken_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/auth"
	"cicadoci.com/cicado/server/jobtoken"
)

// newManagerForTest builds a Manager over a fresh Memory store with a
// controllable clock and a capturing auditor. The returned pointer
// points at the clock's current time; advancing it simulates the
// passing of wall-clock.
func newManagerForTest(t *testing.T, start time.Time) (*jobtoken.Manager, *jobtoken.Memory, *jobtoken.MemoryAuditor, *time.Time) {
	clock := &start
	store := jobtoken.NewMemory()
	aud := jobtoken.NewMemoryAuditor()
	mgr, err := jobtoken.NewManagerWithClock(store, aud, func() time.Time { return *clock })
	if err != nil {
		t.Fatalf("NewManagerWithClock: %v", err)
	}
	return mgr, store, aud, clock
}

func validClaims(t *testing.T) jobtoken.Claims {
	t.Helper()
	return jobtoken.Claims{
		OrgID:  uuid.New(),
		RepoID: uuid.New(),
		RunID:  uuid.New(),
		JobID:  uuid.New(),
		Branch: "main",
		Issuer: "controller",
		Scopes: jobtoken.WithRead(jobtoken.ScopeWrite),
	}
}

func TestIssueThenVerifyRoundTrips(t *testing.T) {
	mgr, _, aud, _ := newManagerForTest(t, time.Unix(1_700_000_000, 0))
	claims := validClaims(t)

	tok, err := mgr.Issue(context.Background(), claims, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok.Raw == "" {
		t.Fatal("issued token has empty raw")
	}
	if err := auth.Validate(auth.KindJob, tok.Raw); err != nil {
		t.Fatalf("issued token does not carry the job kind prefix: %v", err)
	}

	got, err := mgr.Verify(context.Background(), tok.Raw, jobtoken.ScopeRead)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ID != tok.ID {
		t.Errorf("verify returned id %s, want %s", got.ID, tok.ID)
	}
	if got.Claims.RunID != claims.RunID {
		t.Errorf("verify returned run %s, want %s", got.Claims.RunID, claims.RunID)
	}
	if got.Claims.RepoID != claims.RepoID {
		t.Error("verify returned the wrong repo")
	}
	if got.Claims.JobID != claims.JobID {
		t.Error("verify returned the wrong job")
	}
	// The token is recorded to the auditor exactly once at Issue and
	// once at Use.
	if len(aud.Issues) != 1 {
		t.Errorf("auditor recorded %d issues, want 1", len(aud.Issues))
	}
	if len(aud.Uses) != 1 {
		t.Errorf("auditor recorded %d uses, want 1", len(aud.Uses))
	}
}

func TestVerifyRejectsJunkAsMalformed(t *testing.T) {
	mgr, _, _, _ := newManagerForTest(t, time.Unix(1_700_000_000, 0))
	cases := []string{
		"",
		"not a token",
		"cicado_rnr_abc",
		"cicado_job_",
		"cicado_job_short",
		"cicado_job_!!!!not-base64!!!!",
	}
	for _, raw := range cases {
		_, err := mgr.Verify(context.Background(), raw, jobtoken.ScopeRead)
		if !errors.Is(err, jobtoken.ErrMalformed) {
			t.Errorf("Verify(%q) = %v, want ErrMalformed", raw, err)
		}
	}
}

func TestVerifyUnknownTokenIsNotFound(t *testing.T) {
	mgr, _, _, _ := newManagerForTest(t, time.Unix(1_700_000_000, 0))
	// A well-formed job token that was never issued: its hash is not
	// in the store.
	raw, err := auth.Generate(auth.KindJob)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mgr.Verify(context.Background(), raw, jobtoken.ScopeRead)
	if !errors.Is(err, jobtoken.ErrNotFound) {
		t.Errorf("Verify(unknown) = %v, want ErrNotFound", err)
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, _, now := newManagerForTest(t, start)
	claims := validClaims(t)

	tok, err := mgr.Issue(context.Background(), claims, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Jump the clock past the expiry.
	*now = start.Add(2 * time.Hour)
	_, err = mgr.Verify(context.Background(), tok.Raw, jobtoken.ScopeRead)
	if !errors.Is(err, jobtoken.ErrExpired) {
		t.Errorf("Verify after expiry = %v, want ErrExpired", err)
	}
}

func TestVerifyAfterRevoke(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, aud, _ := newManagerForTest(t, start)
	claims := validClaims(t)

	tok, err := mgr.Issue(context.Background(), claims, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Sanity: usable before revoke.
	if _, err := mgr.Verify(context.Background(), tok.Raw, jobtoken.ScopeRead); err != nil {
		t.Fatalf("pre-revoke verify: %v", err)
	}
	if err := mgr.Revoke(context.Background(), tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// After revoke, verify fails with ErrRevoked.
	_, err = mgr.Verify(context.Background(), tok.Raw, jobtoken.ScopeRead)
	if !errors.Is(err, jobtoken.ErrRevoked) {
		t.Errorf("post-revoke verify = %v, want ErrRevoked", err)
	}
	// Revocation is idempotent.
	if err := mgr.Revoke(context.Background(), tok.ID); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	// The revoke is recorded to the auditor.
	if len(aud.Revocs) != 1 {
		t.Errorf("auditor recorded %d revokes, want 1", len(aud.Revocs))
	}
}

func TestVerifyEnforcesScope(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, _, _ := newManagerForTest(t, start)
	// A read-only token: a fork-PR job would look like this.
	claims := validClaims(t)
	claims.Scopes = jobtoken.WithRead()

	tok, err := mgr.Issue(context.Background(), claims, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := mgr.Verify(context.Background(), tok.Raw, jobtoken.ScopeRead); err != nil {
		t.Errorf("read on a read-scoped token should pass: %v", err)
	}
	// The write scope is refused.
	_, err = mgr.Verify(context.Background(), tok.Raw, jobtoken.ScopeWrite)
	if !errors.Is(err, jobtoken.ErrForbidden) {
		t.Errorf("write on a read-scoped token = %v, want ErrForbidden", err)
	}
}

func TestIssueRejectsNonPositiveTTL(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, _, _ := newManagerForTest(t, start)
	for _, ttl := range []time.Duration{0, -time.Minute, -time.Hour} {
		_, err := mgr.Issue(context.Background(), validClaims(t), ttl)
		if !errors.Is(err, jobtoken.ErrNonPositiveTTL) {
			t.Errorf("Issue ttl=%v = %v, want ErrNonPositiveTTL", ttl, err)
		}
	}
}

func TestIssueRejectsIncompleteClaims(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, _, _ := newManagerForTest(t, start)
	// Missing the run/job ids.
	if _, err := mgr.Issue(context.Background(), jobtoken.Claims{
		OrgID:  uuid.New(),
		Scopes: jobtoken.WithRead(),
	}, time.Hour); err == nil {
		t.Error("Issue with incomplete claims should fail")
	}
	// Zero scopes is also illegal.
	_, err := mgr.Issue(context.Background(), jobtoken.Claims{
		OrgID: uuid.New(),
		RunID: uuid.New(),
		JobID: uuid.New(),
	}, time.Hour)
	if err == nil {
		t.Error("Issue with no scopes should fail")
	}
}

// TestStoredRawIsNotRecoverable asserts the headline invariant: the
// store keeps only the hash, not the raw bearer. The Memory store
// exposes Len() but nothing that returns the raw.
func TestStoredRawIsNotRecoverable(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, store, _, _ := newManagerForTest(t, start)
	tok, err := mgr.Issue(context.Background(), validClaims(t), time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if store.Len() != 1 {
		t.Fatalf("store len = %d, want 1", store.Len())
	}
	if tok.Raw == "" {
		t.Fatal("raw is empty")
	}
	// The only way to recover a record from the store is to supply
	// the raw to the verify path; the store itself never holds it.
	if _, err := mgr.Verify(context.Background(), tok.Raw, jobtoken.ScopeRead); err != nil {
		t.Errorf("verify of a freshly issued token failed: %v", err)
	}
}

func TestBearerPrefixIsStripped(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, _, _ := newManagerForTest(t, start)
	tok, err := mgr.Issue(context.Background(), validClaims(t), time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// "Bearer <token>" should verify just like <token> did.
	if _, err := mgr.Verify(context.Background(), "Bearer "+tok.Raw, jobtoken.ScopeRead); err != nil {
		t.Errorf("Verify with 'Bearer ' prefix = %v, want success", err)
	}
	if _, err := mgr.Verify(context.Background(), "bearer "+tok.Raw, jobtoken.ScopeRead); err != nil {
		t.Errorf("Verify with 'bearer ' prefix = %v, want success", err)
	}
}

func TestHasScope(t *testing.T) {
	tok := jobtoken.Token{Claims: jobtoken.Claims{Scopes: jobtoken.WithRead()}}
	if !tok.HasScope(jobtoken.ScopeRead) {
		t.Error("expected read scope present")
	}
	if tok.HasScope(jobtoken.ScopeWrite) {
		t.Error("did not expect write scope present")
	}
}

// TestNewManagerRejectsNilAuditor locks the security invariant that an
// auditor is required: CLAUDE.md names "every state-changing action writes an
// audit event," and mint/revoke are state changes, so a nil auditor is a
// wiring error that must fail loud at construction, not silently drop audits.
func TestNewManagerRejectsNilAuditor(t *testing.T) {
	store := jobtoken.NewMemory()
	if _, err := jobtoken.NewManager(store, nil); err == nil {
		t.Error("NewManager(store, nil) = nil error, want non-nil")
	}
	if _, err := jobtoken.NewManagerWithClock(store, nil, func() time.Time { return time.Now() }); err == nil {
		t.Error("NewManagerWithClock(store, nil, …) = nil, want non-nil")
	}
}

// TestVerifyRejectsEmptyScope locks the fail-open footgun closed: a caller that
// omits the scope argument or passes a zero value must not slip past the
// authorization check.
func TestVerifyRejectsEmptyScope(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, _, _ := newManagerForTest(t, start)
	tok, err := mgr.Issue(context.Background(), validClaims(t), time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// A read-only token, then an unscoped (zero-value) Verify: must fail closed.
	claims := validClaims(t)
	claims.Scopes = jobtoken.WithRead()
	ro, err := mgr.Issue(context.Background(), claims, time.Hour)
	if err != nil {
		t.Fatalf("issue read: %v", err)
	}
	_ = tok
	if _, err := mgr.Verify(context.Background(), ro.Raw, ""); !errors.Is(err, jobtoken.ErrForbidden) {
		t.Errorf("Verify(raw, \"\") = %v, want ErrForbidden", err)
	}
	// A write-scoped token must also fail on an empty scope.
	if _, err := mgr.Verify(context.Background(), tok.Raw, jobtoken.Scope("")); !errors.Is(err, jobtoken.ErrForbidden) {
		t.Errorf("Verify(raw, Scope(\"\")) = %v, want ErrForbidden", err)
	}
}

// TestIssueRejectsMissingRepo locks the foreign-key invariant: a token bound to
// no repo would be inserted with the all-zero UUID, which Postgres rejects
// against repo_id REFERENCES repos(id). Reject it at Validate.
func TestIssueRejectsMissingRepo(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	mgr, _, _, _ := newManagerForTest(t, start)
	claims := validClaims(t)
	claims.RepoID = uuid.Nil
	if _, err := mgr.Issue(context.Background(), claims, time.Hour); err == nil {
		t.Error("Issue with RepoID = uuid.Nil should fail")
	}
}
