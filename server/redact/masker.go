// Package redact provides log-redaction for CICaDo. Secret values
// (installation access tokens, API keys, etc.) are registered with
// a Masker before the process that may emit them starts. Any string
// that passes through the Masker is scrubbed of registered values
// before it reaches a log file, stdout, or stderr.
//
// This package seeds CIC-31 (M4 Log Redaction).
package redact

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Replacement is the byte sequence written in place of a matched
// secret value. It is a constant so the fuzzer can assert "every
// registered value is absent from the output" without needing to
// know the original length.
const Replacement = "**REDACTED**"

// Masker scrubs registered secret values from log bytes. It is safe
// for concurrent use.
//
// Both string- and bytes-oriented operations exist so the ingest
// path (which sees []byte chunks and JSON event payloads) and the
// string-oriented call sites (e.g. a slog scrubber) can share the
// implementation.
type Masker interface {
	// Scrub returns s with all registered secret values replaced
	// by Replacement.
	Scrub(s string) string
	// Mask returns a copy of b with all registered secret values
	// replaced by Replacement. The returned slice may or may not
	// alias b.
	Mask(b []byte) []byte
}

// RegisteredMasker is a simple in-memory masker backed by a set of
// secret values. Each registered value is matched as a byte
// substring.
//
// For performance, long values are matched first so the common
// "replace longest match" behavior works correctly when one secret
// is a substring of another.
type RegisteredMasker struct {
	mu     sync.RWMutex
	values [][]byte // longest-first; matched as raw byte substrings
}

// NewRegisteredMasker returns a new empty masker.
func NewRegisteredMasker() *RegisteredMasker {
	return &RegisteredMasker{}
}

// Register adds a secret value to the scrubber. It is a no-op if
// the value is empty or already registered.
//
// Register is byte-oriented so callers can hand in non-UTF-8
// values (a base64-encoded raw blob, for example).
func (m *RegisteredMasker) Register(secret string) {
	m.RegisterBytes([]byte(secret))
}

// RegisterBytes is the byte-oriented counterpart of Register.
func (m *RegisteredMasker) RegisterBytes(secret []byte) {
	if len(secret) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.values {
		if string(v) == string(secret) {
			return // already registered
		}
	}
	m.values = append(m.values, append([]byte(nil), secret...))
	// Keep longest-first so overlapping matches are handled correctly.
	sort.Slice(m.values, func(i, j int) bool {
		return len(m.values[i]) > len(m.values[j])
	})
}

// RegisterWithVariants registers the raw value together with its
// base64 (StdEncoding), base64 (RawURLEncoding), and URL-quoted
// encodings. This is the call site operators use when they only
// know the raw secret but a runner might log an encoded form (e.g.
// a token that was base64-embedded into a credential URL).
func (m *RegisteredMasker) RegisterWithVariants(raw string) {
	if raw == "" {
		return
	}
	m.Register(raw)
	for _, enc := range m.derivations(raw) {
		m.RegisterBytes([]byte(enc))
	}
}

// derivations returns the derived encodings of raw. The order is
// deterministic (base64 std, base64 raw-URL, url-escaped) so tests
// can assert against it without depending on iteration order.
func (m *RegisteredMasker) derivations(raw string) []string {
	var out []string
	if b := base64.StdEncoding.EncodeToString([]byte(raw)); b != "" {
		out = append(out, b)
	}
	if b := base64.RawURLEncoding.EncodeToString([]byte(raw)); b != "" {
		out = append(out, b)
	}
	if b := url.QueryEscape(raw); b != "" {
		out = append(out, b)
	}
	return out
}

// Scrub returns s with all registered secret values replaced by
// Replacement.
//
// The scan is longest-match-first so an overlapping registration
// cannot leave a partial token behind.
func (m *RegisteredMasker) Scrub(s string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.values) == 0 {
		return s
	}
	out := s
	for _, v := range m.values {
		out = strings.ReplaceAll(out, string(v), Replacement)
	}
	return out
}

// Mask returns a copy of b with all registered secret values
// replaced by Replacement.
//
// The implementation converts to string, runs the same longest-
// match-first scan as Scrub, and converts back. For the MVP this
// is correct and simple; a fast-path that scans []byte directly
// can be added later if profiling shows the conversion is a hot
// spot.
func (m *RegisteredMasker) Mask(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return []byte(m.Scrub(string(b)))
}

// NoopMasker is a Masker that returns its input unchanged.
type NoopMasker struct{}

// Scrub returns the input unchanged.
func (NoopMasker) Scrub(s string) string { return s }

// Mask returns the input unchanged.
func (NoopMasker) Mask(b []byte) []byte { return b }

var _ Masker = (*RegisteredMasker)(nil)
var _ Masker = NoopMasker{}
