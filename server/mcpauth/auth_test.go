package mcpauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreCreateResolve(t *testing.T) {
	ms := NewMemory()
	tok, err := ms.Create(context.Background(), "user-1", []Scope{ScopeRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Principal != "user-1" {
		t.Fatalf("expected principal user-1, got %q", tok.Principal)
	}
	if len(tok.Scopes) != 1 || tok.Scopes[0] != ScopeRead {
		t.Fatalf("expected read scope, got %+v", tok.Scopes)
	}
	_, err = ms.Resolve(context.Background(), tok.ID)
	if err != nil {
		t.Fatalf("expected to resolve token, got %v", err)
	}
}

func TestMemoryStoreNotFound(t *testing.T) {
	ms := NewMemory()
	_, err := ms.Resolve(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStoreExpired(t *testing.T) {
	ms := NewMemory()
	tok, _ := ms.Create(context.Background(), "user-1", []Scope{ScopeRead}, -time.Hour)
	_, err := ms.Resolve(context.Background(), tok.ID)
	if err == nil {
		t.Fatal("expected expired error, got nil")
	}
}

func TestRevoke(t *testing.T) {
	ms := NewMemory()
	tok, _ := ms.Create(context.Background(), "user-1", []Scope{ScopeRead}, time.Hour)
	err := ms.Revoke(context.Background(), tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ms.Resolve(context.Background(), tok.ID)
	if err == nil {
		t.Fatal("expected error after revoke")
	}
}

func TestHasScope(t *testing.T) {
	tok := Token{Scopes: []Scope{ScopeRead}}
	if !tok.HasScope(ScopeRead) {
		t.Error("expected ScopeRead")
	}
	if tok.HasScope("write") {
		t.Error("did not expect write scope")
	}
}

func TestAuthenticateBearerPrefix(t *testing.T) {
	ms := NewMemory()
	tok, _ := ms.Create(context.Background(), "user-1", []Scope{ScopeRead}, time.Hour)
	auth := NewAuthorizer(ms)
	_, err := auth.Authenticate(context.Background(), "Bearer "+tok.ID)
	if err != nil {
		t.Fatalf("expected Bearer-prefixed token to resolve, got %v", err)
	}
}

func TestMemoryAuditor(t *testing.T) {
	au := NewMemoryAuditor()
	_ = au.Record(context.Background(), AuditEntry{
		Principal: "user-1",
		ToolName:  "classify_failure",
		Arg:       "step-0",
	})
	entries := au.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Principal != "user-1" {
		t.Fatalf("expected user-1, got %q", entries[0].Principal)
	}
}

func TestCallbackAdapter(t *testing.T) {
	au := NewMemoryAuditor()
	cb := CallbackAdapter(au, context.Background())
	cb("user-1", "list_runs", "status=queued")
	entries := au.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ToolName != "list_runs" {
		t.Fatalf("expected list_runs, got %q", entries[0].ToolName)
	}
}
