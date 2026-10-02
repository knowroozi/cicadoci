package secretexpr

import (
	"reflect"
	"testing"
)

// ── Refs (extraction) ────────────────────────────────────────────────────────

func TestRefs_BareForm(t *testing.T) {
	refs := Refs(map[string]string{
		"DEPLOY_ROLE": "secrets.PROD_DEPLOY_ROLE",
		"PLAIN":       "just a value",
		"EMPTY":       "",
	})
	if got, want := refs, []string{"PROD_DEPLOY_ROLE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Refs() = %v, want %v", got, want)
	}
}

func TestRefs_TemplateForm(t *testing.T) {
	refs := Refs(map[string]string{
		"A": "  ${{ secrets.PROD_DB_PASSWORD }}  ", // surrounding whitespace
		"B": "${{ secrets.API_KEY }}",
		"C": "plain value",
	})
	if got, want := refs, []string{"API_KEY", "PROD_DB_PASSWORD"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Refs() = %v, want %v", got, want)
	}
}

func TestRefs_DistinctAndSorted(t *testing.T) {
	refs := Refs(map[string]string{
		"A": "secrets.ZEBRA",
		"B": "secrets.alpha",
		"C": "  ${{ secrets.ZEBRA }}  ", // duplicate of A via a different form
		"D": "plain",
	})
	if got, want := refs, []string{"ZEBRA", "alpha"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Refs() = %v, want %v", got, want)
	}
}

func TestRefs_EmbeddedTemplateMatched(t *testing.T) {
	// A `${{ secrets.TOKEN }}` template embedded in a larger string IS a
	// reference (CIC-135): its name must be captured so the server delivers
	// the secret and Resolve can substitute it in place.
	refs := Refs(map[string]string{
		"URL": "pre-" + "${{ secrets.TOKEN }}-post",
		"X":   "prefix secrets.SUFFIX suffix", // bare-embedded: NOT a reference.
	})
	if got, want := refs, []string{"TOKEN"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Refs() = %v, want %v", got, want)
	}
}

func TestRefs_BareEmbeddedNotMatched(t *testing.T) {
	// A bare `secrets.X` that is only part of a larger value (not a template,
	// not the whole value) is NOT treated as a reference — it cannot be
	// unambiguously resolved.
	refs := Refs(map[string]string{
		"X": "prefix secrets.SUFFIX suffix",
	})
	if refs != nil {
		t.Errorf("Refs() = %v, want nil (bare-fragment not a reference)", refs)
	}
}

func TestRefs_EmptyAndNil(t *testing.T) {
	if got := Refs(nil); got != nil {
		t.Errorf("Refs(nil) = %v, want nil", got)
	}
	if got := Refs(map[string]string{"A": "b"}); got != nil {
		t.Errorf("Refs(no-refs) = %v, want nil", got)
	}
}

func TestRefs_InvalidNameNotMatched(t *testing.T) {
	for _, v := range []string{
		"secrets.",
		"secrets.x y",
		"${{ secrets. }}",
		"secrets.WITH!BANG",
		"prefix secrets.SUFFIX suffix", // bare-embedded, not a reference
	} {
		if got := Refs(map[string]string{"A": v}); got != nil {
			t.Errorf("Refs(%q) = %v, want nil (invalid or empty name)", v, got)
		}
	}
}

// ── Resolve (substitution) ───────────────────────────────────────────────────

func TestResolve_BareForm(t *testing.T) {
	in := map[string]string{
		"DEPLOY_ROLE": "secrets.PROD_DEPLOY_ROLE",
		"PLAIN":       "keep me",
	}
	got := Resolve(in, map[string]string{"PROD_DEPLOY_ROLE": "arn:aws:iam::123:role/x"})
	if got["DEPLOY_ROLE"] != "arn:aws:iam::123:role/x" {
		t.Errorf("Resolve DEPLOY_ROLE = %q, want the secret value", got["DEPLOY_ROLE"])
	}
	if got["PLAIN"] != "keep me" {
		t.Errorf("Resolve PLAIN = %q, want unchanged", got["PLAIN"])
	}
}

func TestResolve_TemplateForm(t *testing.T) {
	// A template that is the whole value is substituted in place, returning the
	// bare reference text replaced by its secret value.
	in := map[string]string{"A": "${{ secrets.API_KEY }}"}
	got := Resolve(in, map[string]string{"API_KEY": "s3cr3t"})
	if got["A"] != "s3cr3t" {
		t.Errorf("Resolve A = %q, want s3cr3t", got["A"])
	}

	// Minimal CIC-135 behaviour pin (Dwight #108 minor #1): a WHOLE-value
	// template surrounded by spaces resolves IN PLACE and preserves the padding
	// (matching GitHub Actions). The literal is built by concatenation so the
	// surrounding source never keeps `$` adjacent to ` {{` — a naive textual
	// pre-processor would corrupt the token into `$ {{ ... }}` and the parser
	// would reject it; Go string literals are not shell-expanded, so this is a
	// source-hygiene precaution only. The resolved value keeps the surrounding
	// spaces: "   padding-templated-value   ".
	padValue := "   " + "${{ secrets.PAD }}" + "   "
	got = Resolve(map[string]string{"P": padValue}, map[string]string{"PAD": "v"})
	if got["P"] != "   v   " {
		t.Errorf("padded whole-value template = %q, want %q (padding preserved, GHA parity)", got["P"], "   v   ")
	}
}

func TestResolve_UnresolvedLeftVerbatim(t *testing.T) {
	in := map[string]string{"A": "secrets.MISSING"}
	got := Resolve(in, map[string]string{}) // MISSING absent
	if got["A"] != "secrets.MISSING" {
		t.Errorf("Resolve unresolved = %q, want verbatim reference text", got["A"])
	}
}

func TestResolve_DoesNotMutateInputs(t *testing.T) {
	in := map[string]string{"A": "secrets.X"}
	sec := map[string]string{"X": "v"}
	out := Resolve(in, sec)
	if out["A"] != "v" || out["X"] != "" {
		t.Errorf("unexpected result: %#v", out)
	}
	// Ensure inputs are unchanged.
	if in["A"] != "secrets.X" {
		t.Errorf("Resolve mutated env: env[A] now %q", in["A"])
	}
	if sec["X"] != "v" {
		t.Errorf("Resolve mutated secrets map")
	}
}

func TestResolve_NilHandling(t *testing.T) {
	if got := Resolve(nil, map[string]string{"X": "v"}); got != nil {
		t.Errorf("Resolve(nil,_) = %v, want nil", got)
	}
	if got := Resolve(map[string]string{}, map[string]string{}); got != nil {
		t.Errorf("Resolve(empty,_) = %v, want nil", got)
	}
	if got := Resolve(map[string]string{"A": "keep"}, nil); got == nil {
		t.Errorf("Resolve(env,nil) = %v, want unchanged map", got)
	}
}

func TestResolve_EmbeddedTemplateSubstituted(t *testing.T) {
	// An embedded `${{ secrets.TOK }}` IS a reference (CIC-135) and is
	// substituted in place, leaving the surrounding literal text intact.
	// The source literal is assembled by concatenation so a naive textual
	// pre-processor never sees `$` directly adjacent to ` {{` (which it might
	// corrupt into `$ {{ ... }}`, a string the parser rejects). Go string
	// literals are never shell-expanded, so this is a source-hygiene
	// precaution only — not a runtime necessity — and the resulting string is
	// `pre-${{ secrets.TOK }}-post`.
	in := map[string]string{"URL": "pre-" + "${{ secrets.TOK }}-post"}
	got := Resolve(in, map[string]string{"TOK": "v"})
	if got["URL"] != "pre-v-post" {
		t.Errorf("Resolve embedded = %q, want pre-v-post", got["URL"])
	}
}

func TestResolve_MultipleEmbedded(t *testing.T) {
	in := map[string]string{"URL": "user=" + "${{ secrets.U }} pw=" + "${{ secrets.P }}"}
	got := Resolve(in, map[string]string{"U": "bob", "P": "pw"})
	if got["URL"] != "user=bob pw=pw" {
		t.Errorf("Resolve multi-embedded = %q, want user=bob pw=pw", got["URL"])
	}
}

func TestResolve_UnresolvedEmbeddedStaysLiteral(t *testing.T) {
	// An embedded reference with no matching secret is left as its literal
	// template text (never a half-substituted string, never leaked).
	v := "pre-" + "${{ secrets.MISSING }}-post"
	got := Resolve(map[string]string{"URL": v}, map[string]string{})
	if got["URL"] != v {
		t.Errorf("Resolve unresolved-embedded = %q, want literal template %q", got["URL"], v)
	}
}

// ── Valid name edge cases ─────────────────────────────────────────────────────

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"A", true},
		{"PROD_DEPLOY_ROLE", true},
		{"with space", false},
		{"WITH.DOT", false},
		{"with-dash", true},
		{"_underscore", true},
		{"9numeric", true},
		{"café", false},
	}
	for _, c := range cases {
		if got := validName(c.name); got != c.want {
			t.Errorf("validName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
