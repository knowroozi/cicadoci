package jobtoken

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"cicadoci.com/cicado/server/audit"
)

func TestAuditLogWritesEvents(t *testing.T) {
	log := audit.NewMemoryLog()
	a := NewAuditLog(log, nil)
	ctx := context.Background()

	tok := Token{
		ID:       uuid.New(),
		Claims:   Claims{RunID: uuid.New(), JobID: uuid.New(), Issuer: "controller"},
		IssuedAt: time.Now(),
	}
	if err := a.Issue(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := a.Use(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := a.Revoke(ctx, tok.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 1 genesis + 3 event
	if got := log.Len(); got < 4 {
		t.Fatalf("expected >=4 entries, got %d", got)
	}
	// Chain must be intact
	if err := log.VerifyChain(ctx); err != nil {
		t.Fatalf("hash chain broken: %v", err)
	}
}

func TestAuditLogNewAuditLogNilLogger(t *testing.T) {
	a := NewAuditLog(audit.NewMemoryLog(), nil)
	if a == nil {
		t.Fatal("NewAuditLog returned nil")
	}
}

// TestAuditLog_PrincipalFidelity (CIC-192 A1): each jobtoken lifecycle event must
// record a meaningful principal_type + surface, not the "user"/"api" default that
// the store applies to blank fields. Use asserts the exercising principal is a token.
func TestAuditLog_PrincipalFidelity(t *testing.T) {
	log := audit.NewMemoryLog()
	a := NewAuditLog(log, nil)
	ctx := context.Background()
	tok := Token{ID: uuid.New(), Claims: Claims{RunID: uuid.New(), JobID: uuid.New(), Issuer: "controller"}}

	if err := a.Issue(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := a.Use(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := a.Revoke(ctx, tok.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	entries, _ := log.List(ctx, 0, 100)
	byAction := map[audit.Action]audit.Entry{}
	for _, e := range entries {
		byAction[e.Action] = e
	}

	checks := []struct {
		action        audit.Action
		wantPrincipal string
		wantSurface   string
	}{
		{audit.Action("jobtoken.issue"), audit.PrincipalSystem, audit.SurfaceSystem},
		{audit.Action("jobtoken.use"), audit.PrincipalToken, audit.SurfaceSystem},
		{audit.Action("jobtoken.revoke"), audit.PrincipalSystem, audit.SurfaceSystem},
	}
	for _, c := range checks {
		e, ok := byAction[c.action]
		if !ok {
			t.Errorf("no audit entry for %q", c.action)
			continue
		}
		if e.PrincipalType != c.wantPrincipal {
			t.Errorf("%s principal_type = %q, want %q",
				c.action, e.PrincipalType, c.wantPrincipal)
		}
		if e.Surface != c.wantSurface {
			t.Errorf("%s surface = %q, want %q",
				c.action, e.Surface, c.wantSurface)
		}
	}

	// CIC-192 B2: a jobtoken.use records the token itself as its principal -- the
	// Actor must be the token's own uuid, not the server-side Issuer that
	// minted it.
	useEntry, ok := byAction[audit.Action("jobtoken.use")]
	if !ok {
		t.Fatal("no audit entry for jobtoken.use; cannot check the token principal id")
	}
	if useEntry.Actor != tok.ID.String() {
		t.Errorf("jobtoken.use actor = %q, want the token id %q "+
			"(a token principal records its own id, not the Issuer)",
			useEntry.Actor, tok.ID.String())
	}
}
