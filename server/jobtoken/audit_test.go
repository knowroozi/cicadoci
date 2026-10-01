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
