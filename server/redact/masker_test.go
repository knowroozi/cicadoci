package redact

import (
	"encoding/base64"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestRegisteredMasker_Scrub(t *testing.T) {
	m := NewRegisteredMasker()
	m.Register("ghs_supersecret123")
	m.Register("aws_key_abc")

	got := m.Scrub("cloning with ghs_supersecret123 at https://x-oauth-basic:ghs_supersecret123@github.com/o/r.git")
	if got == "cloning with ghs_supersecret123 at https://x-oauth-basic:ghs_supersecret123@github.com/o/r.git" {
		t.Fatal("expected redaction but got unchanged string")
	}
	if !strings.Contains(got, "**REDACTED**") {
		t.Fatalf("scrubbed = %q, want contains **REDACTED**", got)
	}
	// The actual secret should not appear.
	if strings.Contains(got, "ghs_supersecret123") {
		t.Fatal("secret still present in scrubbed output")
	}
}

func TestRegisteredMasker_LongestFirst(t *testing.T) {
	m := NewRegisteredMasker()
	m.Register("short")
	m.Register("short_secret_value")

	got := m.Scrub("the short_secret_value is here")
	if !strings.Contains(got, "**REDACTED**") {
		t.Fatalf("got = %q, want **REDACTED**", got)
	}
	// The longer secret should have been matched first, so the
	// shorter one should NOT leave a partial residue.
	if strings.Contains(got, "short") {
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

func TestNoopMasker_Scrub(t *testing.T) {
	var m Masker = NoopMasker{}
	got := m.Scrub("anything")
	if got != "anything" {
		t.Fatal("noop changed the string")
	}
}

func TestNoopMasker_Mask(t *testing.T) {
	var m Masker = NoopMasker{}
	got := m.Mask([]byte("anything"))
	if string(got) != "anything" {
		t.Fatal("noop changed the bytes")
	}
}

// TestConcurrentRegistration verifies the scrubber is safe for
// concurrent Register and that duplicates are not re-added.
func TestConcurrentRegistration(t *testing.T) {
	m := NewRegisteredMasker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Register("secret")
			// only one "secret" should be registered at the end
		}()
	}
	wg.Wait()
	m.mu.RLock()
	n := len(m.values)
	m.mu.RUnlock()
	if n != 1 {
		t.Fatalf("len(values) = %d after concurrent Register, want 1", n)
	}
}

// TestMask_Adversarial_DerivedVariants is the adversarial case: a
// runner that doesn't log a secret's raw form but logs an encoded
// form (e.g. a `ghp_...` token that was base64-embedded into a
// credential URL, or URL-encoded in a query string).
// RegisterWithVariants must scrub all of them.
//
// CVE-2025-30066-style attacks assume exfiltration via log content;
// if the runner logs the base64/URL-encoded form of a key, a naive
// scrubber that only matches the raw value leaks. This test asserts
// that all three variants are scrubbed.
func TestMask_Adversarial_DerivedVariants(t *testing.T) {
	raw := "ghp_ABCDEFGH1234567890abcdef"
	m := NewRegisteredMasker()
	m.RegisterWithVariants(raw)

	b64Std := base64.StdEncoding.EncodeToString([]byte(raw))
	b64Raw := base64.RawURLEncoding.EncodeToString([]byte(raw))
	queryEsc := url.QueryEscape(raw)

	cases := []struct {
		name string
		in   []byte
		want []string
	}{
		{"raw", []byte("secret is " + raw + " done"), []string{raw}},
		{"b64std", []byte("encoded " + b64Std + " end"), []string{b64Std}},
		{"b64uraw", []byte("encoded " + b64Raw + " end"), []string{b64Raw}},
		{"queryesc", []byte("q=" + queryEsc + "&x=1"), []string{queryEsc}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := m.Mask(c.in)
			out := string(got)
			for _, want := range c.want {
				if strings.Contains(out, want) {
					t.Fatalf("%q present in output: %q", want, out)
				}
			}
			if !strings.Contains(out, "**REDACTED**") {
				t.Fatalf("expected **REDACTED** in %q", out)
			}
		})
	}
}

// TestMask_Adversarial_LongestMatchWithVariants ensures that when a
// raw token and a longer overlapping token are both registered, the
// longer match wins so no partial residue is left.
func TestMask_Adversarial_LongestMatchWithVariants(t *testing.T) {
	m := NewRegisteredMasker()
	m.Register("short_secret")
	m.Register("short_secret_value_longer")
	got := m.Mask([]byte("here: short_secret_value_longer done"))
	if !strings.Contains(string(got), "**REDACTED**") {
		t.Fatalf("missing **REDACTED** in %q", got)
	}
	if strings.Contains(string(got), "short_secret") {
		t.Fatalf("partial residue: %q", got)
	}
}

// TestMask_Adversarial_SplitAcrossChunks is a documented limitation.
//
// Because the masker operates per-chunk, a token that straddles a
// chunk boundary is not reliably scrubbed: each chunk is scanned
// independently. The MVP chunk size is 64 KiB and real-world
// secrets are <1 KiB, so this does not happen in practice today.
//
// The test asserts the *failure mode* for adversarial awareness,
// not that the masker handles it: it shows that a 16-byte token
// split 8+8 across two chunks survives scrubbing, which is exactly
// the behavior a downstream reviewer needs to know about.
func TestMask_Adversarial_SplitAcrossChunks(t *testing.T) {
	m := NewRegisteredMasker()
	raw := "12345678ABCDEFgh" // 16 bytes
	m.Register(raw)
	first := []byte("prefix " + raw[:8])
	second := []byte(raw[8:] + " suffix")
	out1 := m.Mask(first)
	out2 := m.Mask(second)
	// Neither chunk alone contains the full token, so neither is
	// scrubbed. Joining them at the read boundary recreates the
	// token. This is expected behavior for a per-chunk scrubber.
	joined := string(out1) + string(out2)
	if strings.Contains(joined, raw) {
		t.Logf("documented split-across-chunks leak: %q", joined)
	}
	// We assert the per-chunk output does NOT scrub either half
	// (proving the gap), but we do not fail the test on that,
	// it is documented behavior, not a regression.
	if !strings.Contains(string(out1), raw[:8]) {
		t.Fatalf("first chunk unexpectedly scrubbed: %q", out1)
	}
	if !strings.Contains(string(out2), raw[8:]) {
		t.Fatalf("second chunk unexpectedly scrubbed: %q", out2)
	}
}

// TestMask_Adversarial_StructuredEventJSON smokes a JSON event that
// embeds the raw token AND its base64 form in distinct fields,
// simulating a real structured event that leaks a secret in two
// places. The masker must remove both.
func TestMask_Adversarial_StructuredEventJSON(t *testing.T) {
	raw := "AKIAEXAMPLESECRET123"
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))
	json := `{"kind":"leak","raw":"` + raw + `","encoded":"` + b64 + `"}`
	m := NewRegisteredMasker()
	m.RegisterWithVariants(raw)
	got := m.Mask([]byte(json))
	out := string(got)
	if strings.Contains(out, raw) {
		t.Fatalf("raw token present: %s", out)
	}
	if strings.Contains(out, b64) {
		t.Fatalf("encoded token present: %s", out)
	}
	if !strings.Contains(out, "**REDACTED**") {
		t.Fatalf("expected **REDACTED** in %s", out)
	}
}

// FuzzMask is the differential fuzz target. For any input, the
// output must not contain any registered token value, raw or
// encoded. We register two fixed token sets in the harness and
// check invariants inside Fuzz.
//
// The fuzz target is the registered-token set, not the input shape:
// it generates arbitrary bytes and asserts the masker never leaks
// a registered value. This catches regressions where the scan
// logic accidentally starts passing through matched bytes.
func FuzzMask(f *testing.F) {
	m := NewRegisteredMasker()
	m.RegisterWithVariants("token_ABCDEFGHIJKL")
	m.RegisterWithVariants("aws_secret_xyz123")
	f.Add([]byte("hello world, no secret here"))
	f.Add([]byte("token_ABCDEFGHIJKL embedded in text"))
	f.Add([]byte("aws_secret_xyz123 embedded"))

	f.Fuzz(func(t *testing.T, in []byte) {
		out := m.Mask(in)
		outStr := string(out)
		tokens := []string{
			"token_ABCDEFGHIJKL",
			"aws_secret_xyz123",
			base64.StdEncoding.EncodeToString([]byte("token_ABCDEFGHIJKL")),
			base64.StdEncoding.EncodeToString([]byte("aws_secret_xyz123")),
			base64.RawURLEncoding.EncodeToString([]byte("token_ABCDEFGHIJKL")),
			base64.RawURLEncoding.EncodeToString([]byte("aws_secret_xyz123")),
			url.QueryEscape("token_ABCDEFGHIJKL"),
			url.QueryEscape("aws_secret_xyz123"),
		}
		for _, tok := range tokens {
			if tok == "" {
				continue
			}
			if strings.Contains(outStr, tok) {
				t.Fatalf("token %q present in output: %q (in=%q)", tok, outStr, in)
			}
		}
	})
}
