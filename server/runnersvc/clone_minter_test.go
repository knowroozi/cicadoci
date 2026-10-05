// clone_minter_test.go: CIC-151 / CIC-146 — unit tests for the per-job
// GitHub installation token minter built by NewCloneMinterFunc.
//
// Test groups:
//  1. Nil/unconfigured path (backwards compat).
//  2. B5: no installation_id in repos table → ("", nil), anonymous clone.
//  3. Invalid installation_id (non-numeric) still fails.
//  4. B5: installation_id="0" → ("", nil), sentinel is "not set".
//  5. Fork-run calls resolver and attempts contents:read (B1 / conv-e2490b).
//  6. ClonePermissionsRead constant sanity.
package runnersvc_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"cicadoci.com/cicado/internal/testdb"
	"cicadoci.com/cicado/server/redact"
	"cicadoci.com/cicado/server/runnersvc"
)

// TestNewCloneMinterFunc_NilWhenUnconfigured proves the minter returns
// nil when appID is 0, keyBytes is empty, or the pool is nil.
func TestNewCloneMinterFunc_NilWhenUnconfigured(t *testing.T) {
	ctx := context.Background()
	if f := runnersvc.NewCloneMinterFunc(ctx, nil, 0, []byte("x"), nil); f != nil {
		t.Fatal("appID=0: expected nil minter")
	}
	if f := runnersvc.NewCloneMinterFunc(ctx, nil, 1, nil, nil); f != nil {
		t.Fatal("empty keyBytes: expected nil minter")
	}
	if f := runnersvc.NewCloneMinterFunc(ctx, nil, 1, []byte("x"), nil); f != nil {
		t.Fatal("nil pool: expected nil minter")
	}
}

// TestNewCloneMinterFunc_B5NoInstallationID (B5): when
// repos.installation_id is empty (not yet set by a webhook install),
// the minter returns ("", nil) — anonymous clone, no hard error.
// This prevents a job from hard-failing just because the App is not
// yet installed on a public repository.
func TestNewCloneMinterFunc_B5NoInstallationID(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()

	var orgID, repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('clone_b5', 'Clone B5') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name)
			 VALUES ($1, 'cloneb5', 'cloneb5')
			 RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	f := runnersvc.NewCloneMinterFunc(
		ctx, pool, 99, []byte("not-pem"), redact.NewRegisteredMasker())
	if f == nil {
		t.Fatal("expected non-nil minter even with a bad key, because pool is non-nil")
	}
	// B5: no installation_id → no token, no error.
	token, err := f(ctx, orgID.String(), repoID.String(), false)
	if err != nil {
		t.Fatalf("B5: expected nil error when installation_id is empty, got %v", err)
	}
	if token != "" {
		t.Fatalf("B5: expected empty token, got %q", token)
	}
}

// TestNewCloneMinterFunc_InvalidInstallationID proves that a non-numeric
// installation_id in the repos table produces a parse error (not a B5 skip).
func TestNewCloneMinterFunc_InvalidInstallationID(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()

	var orgID, repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('clone_bad', 'Clone Bad') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name, installation_id)
			 VALUES ($1, 'clonebad', 'clonebad', 'not-a-number')
			 RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	f := runnersvc.NewCloneMinterFunc(
		ctx, pool, 99, []byte("not-pem"), redact.NewRegisteredMasker())
	if f == nil {
		t.Fatal("expected non-nil minter")
	}
	_, err := f(ctx, orgID.String(), repoID.String(), false)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "installation id") &&
		!strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

// TestNewCloneMinterFunc_ForkRunCallsResolver proves that for isFork=true
// the minter still calls the resolver (B1 / conv-e2490b #2: fork runs get
// a contents:read token). The HTTP call fails on the bad key, but the
// error must NOT mention "fork" — that would mean the fork path was
// treated as a guard.
func TestNewCloneMinterFunc_ForkRunCallsResolver(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()

	var orgID, repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('clone_fork', 'Clone Fork') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name, installation_id)
			 VALUES ($1, 'clonefork', 'clonefork', '555555')
			 RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	f := runnersvc.NewCloneMinterFunc(
		ctx, pool, 99, []byte("not-pem"), redact.NewRegisteredMasker())
	if f == nil {
		t.Fatal("expected non-nil minter")
	}

	// isFork=true: resolver returns 555555 (valid ID); then JWT signing
	// fails because the key is not a real PEM.
	_, err := f(ctx, orgID.String(), repoID.String(), true)
	if err == nil {
		t.Error("expected an error (not-a-PEM key), got nil")
	}
	if strings.Contains(err.Error(), "unexpected fork") ||
		strings.Contains(err.Error(), "fork-run") {
		t.Errorf("fork guard should NOT fire — expected resolver success + auth failure, got: %v", err)
	}
}

// TestNewCloneMinterFunc_B5ZeroInstallationID (B5): installation_id="0"
// is the "not set" sentinel value. The minter returns ("", nil) — no
// hard error; anonymous clone is the correct behavior.
func TestNewCloneMinterFunc_B5ZeroInstallationID(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	pool := st.Pool()

	var orgID, repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (slug, name) VALUES ('clone_b5z', 'Clone B5Z') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (org_id, owner, name, installation_id)
			 VALUES ($1, 'cloneb5z', 'cloneb5z', '0')
			 RETURNING id`,
		orgID).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	f := runnersvc.NewCloneMinterFunc(
		ctx, pool, 99, []byte("not-pem"), redact.NewRegisteredMasker())
	if f == nil {
		t.Fatal("expected non-nil minter")
	}
	// B5: "0" is the "not set" sentinel → empty token, no error.
	token, err := f(ctx, orgID.String(), repoID.String(), false)
	if err != nil {
		t.Fatalf("B5: expected nil error for installation_id='0', got %v", err)
	}
	if token != "" {
		t.Fatalf("B5: expected empty token, got %q", token)
	}
}
