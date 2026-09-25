package redact

import (
	"sync"
	"testing"
)

func TestRegisteredMasker_Scrub(t *testing.T) {
	m := NewRegisteredMasker()
	m.Register("ghs_supersecret123")
	m.Register("aws_key_abc")

	got := m.Scrub("cloning with gh s_supersecret123 at https://x-oauth-basic:ghs_supersecret123@github.com/o/r.git")
	if got == "cloning with gh s_supersecret123 at https://x-oauth-basic:ghs_supersecret123@github.com/o/r.git" {
		t.Fatal("expected redaction but got unchanged string")
	}
	if !contains(got, "**REDACTED**") {
		t.Fatalf("scrubbed = %q, want contains **REDACTED**", got)
	}
	// The actual secret should not appear.
	if contains(got, "ghs_supersecret123") {
		t.Fatal("secret still present in scrubbed output")
	}
}

func TestRegisteredMasker_LongestFirst(t *testing.T) {
	m := NewRegisteredMasker()
	m.Register("short")
	m.Register("short_secret_value")

	got := m.Scrub("the short_secret_value is here")
	if !contains(got, "**REDACTED**") {
		t.Fatalf("got = %q, want **REDACTED**", got)
	}
	// The longer secret should have been matched first.
	if contains(got, "short") {
		t.Fatalf("partial match leaked: %q", got)
	}
}

func TestRegisteredMasker_Empty(t *testing.T) {
	m := NewRegisteredMasker()
	m.Register("") // no-op
	got := m.Scrub("nothing to scrub")
	if got != "nothing to scrub" {
		t.Fatalf("got = %q, want unchanged", got)
	}
}

func TestRegisteredMasker_Duplicate(t *testing.T) {
	m := NewRegisteredMasker()
	m.Register("secret1")
	m.Register("secret1") // duplicate, no-op
	m.Register("")        // no-op
	m.Register("secret2")
	m.mu.RLock()
	n := len(m.values)
	m.mu.RUnlock()
	if n != 2 {
		t.Fatalf("len(values) = %d, want 2", n)
	}
}

func TestNoopMasker(t *testing.T) {
	var m Masker = NoopMasker{}
	got := m.Scrub("anything")
	if got != "anything" {
		t.Fatal("noop changed the string")
	}
}

func TestConcurrentRegistration(t *testing.T) {
	m := NewRegisteredMasker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.Register("secret")
			_ = i
		}(i)
	}
	wg.Wait()
	// Should not panic; only one "secret" should be registered.
	m.mu.RLock()
	n := len(m.values)
	m.mu.RUnlock()
	if n != 1 {
		t.Fatalf("len(values) = %d after concurrent Register, want 1", n)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
