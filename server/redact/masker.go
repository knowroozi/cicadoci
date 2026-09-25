// Package redact provides log-redaction for CICaDo. Secret values
// (installation access tokens, API keys, etc.) are registered with
// a Masker before the process that may emit them starts. Any string
// that passes through the Masker is scrubbed of registered values
// before it reaches a log file, stdout, or stderr.
//
// This package seeds CIC-31 (M4 Log Redaction).
package redact

import (
	"sort"
	"strings"
	"sync"
)

// Masker scrubs registered secret values from a string.
// It is safe for concurrent use.
type Masker interface {
	// Scrub returns a copy of s with all registered secret values
	// replaced by **REDACTED**.
	Scrub(s string) string
}

// RegisteredMasker is a simple in-memory masker backed by a set of
// secret values. Each registered value is matched as a substring.
//
// For performance, long values (>= 8 chars) are matched first so the
// common "replace longest match" behavior works correctly when one
// secret is a substring of another.
type RegisteredMasker struct {
	mu     sync.RWMutex
	values []string
}

// NewRegisteredMasker returns a new empty masker.
func NewRegisteredMasker() *RegisteredMasker {
	return &RegisteredMasker{}
}

// Register adds a secret value to the scrubber. It is a no-op if the
// value is empty or already registered.
func (m *RegisteredMasker) Register(secret string) {
	if secret == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.values {
		if v == secret {
			return
			// already registered
		}
	}
	m.values = append(m.values, secret)
	// Keep longest-first so overlapping matches are handled correctly.
	sort.Slice(m.values, func(i, j int) bool {
		return len(m.values[i]) > len(m.values[j])
	})
}

// Scrub returns s with all registered secret values replaced.
func (m *RegisteredMasker) Scrub(s string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.values) == 0 {
		return s
	}
	for _, v := range m.values {
		s = strings.ReplaceAll(s, v, "**REDACTED**")
	}
	return s
}

// NoopMasker is a Masker that returns its input unchanged.
type NoopMasker struct{}

// Scrub returns the input unchanged.
func (NoopMasker) Scrub(s string) string { return s }

var _ Masker = (*RegisteredMasker)(nil)
var _ Masker = NoopMasker{}
