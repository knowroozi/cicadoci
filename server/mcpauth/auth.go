// Package mcpauth implements the scoped token + per-invocation audit
// layer for the MCP server (CIC-56).
//
// MCP tokens carry `read` scope only in MVP. Every tool invocation
// is audited with its principal. No shared credentials with human
// sessions.
package mcpauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Scope is a tool-access scope.
type Scope string

// Known MCP scopes.
const (
	// ScopeRead is the only scope in MVP.
	ScopeRead Scope = "read"
)

// Token is an MCP access token.
type Token struct {
	// ID is the opaque token ID.
	ID string

	// Principal is the identity the token represents.
	Principal string

	// Scopes are the allowed scopes.
	Scopes []Scope

	// Expiry is the absolute time the token stops being valid.
	Expiry time.Time

	// CreatedAt is when the token was issued.
	CreatedAt time.Time
}

// Store is the token store.
type Store interface {
	// Create mints a new token.
	Create(ctx context.Context, principal string, scopes []Scope, ttl time.Duration) (Token, error)

	// Resolve looks up a token by its raw value.
	// Returns ErrNotFound if the token is unknown.
	Resolve(ctx context.Context, raw string) (Token, error)

	// Revoke disables a token.
	Revoke(ctx context.Context, tokenID string) error
}

// ErrNotFound is returned when a token is not found.
var ErrNotFound = fmt.Errorf("mcpauth: token not found")

// Memory is an in-memory token store for testing.
type Memory struct {
	tokens map[string]Token
}

// NewMemory creates a new in-memory store.
func NewMemory() *Memory {
	return &Memory{tokens: map[string]Token{}}
}

// Create mints a new token.
func (m *Memory) Create(_ context.Context, principal string, scopes []Scope, ttl time.Duration) (Token, error) {
	h := sha256.Sum256([]byte(principal + time.Now().Format(time.RFC3339Nano)))
	raw := "mcpt_" + hex.EncodeToString(h[:16])
	tok := Token{
		ID:        raw,
		Principal: principal,
		Scopes:    scopes,
		Expiry:    time.Now().Add(ttl),
		CreatedAt: time.Now(),
	}
	m.tokens[raw] = tok
	return tok, nil
}

// Resolve looks up a token by raw value.
func (m *Memory) Resolve(_ context.Context, raw string) (Token, error) {
	tok, ok := m.tokens[raw]
	if !ok {
		return Token{}, ErrNotFound
	}
	if time.Now().After(tok.Expiry) {
		return Token{}, fmt.Errorf("mcpauth: token expired")
	}
	return tok, nil
}

// Revoke deletes a token from the store.
func (m *Memory) Revoke(_ context.Context, tokenID string) error {
	delete(m.tokens, tokenID)
	return nil
}

// Authorizer checks whether a token has a given scope.
type Authorizer struct {
	store Store
}

// NewAuthorizer creates an authorizer backed by a store.
func NewAuthorizer(s Store) *Authorizer {
	return &Authorizer{store: s}
}

// Authenticate resolves a raw token and checks that it hasn't expired.
func (a *Authorizer) Authenticate(ctx context.Context, raw string) (Token, error) {
	// The raw token may be prefixed with "Authorization: Bearer ".
	// Handle that here so callers can pass the header value directly.
	raw = strings.TrimPrefix(raw, "Bearer ")
	return a.store.Resolve(ctx, raw)
}

// HasScope reports whether a token has the given scope.
func (t Token) HasScope(s Scope) bool {
	for _, sc := range t.Scopes {
		if sc == s {
			return true
		}
	}
	return false
}

// AuditEntry is a structured record of a tool invocation.
type AuditEntry struct {
	Principal string
	ToolName  string
	Arg       string
	Timestamp time.Time
}

// Auditor records tool invocations.
type Auditor interface {
	// Record logs an invocation.
	Record(ctx context.Context, entry AuditEntry) error
}

// MemoryAuditor is an in-memory auditor.
type MemoryAuditor struct {
	entries []AuditEntry
}

// NewMemoryAuditor creates a new in-memory auditor.
func NewMemoryAuditor() *MemoryAuditor {
	return &MemoryAuditor{}
}

// Record logs an invocation.
func (m *MemoryAuditor) Record(_ context.Context, entry AuditEntry) error {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	m.entries = append(m.entries, entry)
	return nil
}

// Entries returns a copy of recorded entries.
func (m *MemoryAuditor) Entries() []AuditEntry {
	out := make([]AuditEntry, len(m.entries))
	copy(out, m.entries)
	return out
}

// AuditCallback is the function signature for the Server's audit field.
// It records (principal, tool, arg).
type AuditCallback func(principal, tool, arg string)

// CallbackAdapter adapts a MemoryAuditor to the AuditCallback signature.
func CallbackAdapter(a *MemoryAuditor, ctx context.Context) AuditCallback {
	return func(principal, toolName, arg string) {
		_ = a.Record(ctx, AuditEntry{
			Principal: principal,
			ToolName:  toolName,
			Arg:       arg,
		})
	}
}
