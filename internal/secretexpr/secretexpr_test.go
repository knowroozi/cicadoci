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
		"A": "${{ secrets.PROD_DB_PASSWORD }}",
		"B": "  ${{ secrets.API_KEY }}  ", // surrounding whitespace
		"C": "plain value",
	})
	if !reflect.DeepEqual(refs, []string{"API_KEY", "PROD_DB_PASSWORD"}) {
		t.Errorf("Refs() = %v, want [API_KEY PROD_DB_PASSWORD]", refs)
	}
}

func TestRefs_DistinctAndSorted(t *testing.T) {
	refs := Refs(map[string]string{
		"A": "secrets.ZEBRA",
		"B": "secrets.alpha",
		"C": "${{ secrets.ZEBRA }}", // duplicate of A via a different form
		"D": "plain",
	})
	if !reflect.DeepEqual(refs, []string{"ZEBRA", "alpha"}) {
		t.Errorf("Refs() = %v, want [ZEBRA alpha]", refs)
	}
}

func TestRefs_EmbeddedFragmentNotMatched(t *testing.T) {
	// A `secrets.FOO` embedded in a larger string must NOT be treated as a
	// reference (it cannot be unambiguously resolved by Resolve).
	refs := Refs(map[string]string{
		"URL": "https://example.com/${{ secrets.TOKEN }}/path",
		"X":   "prefix secrets.SUFFIX suffix",
	})
	if refs != nil {
		t.Errorf("Refs() = %v, want nil (no clean reference)", refs)
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
		"secrets.",
		"${{ secrets. }}",
		"secrets.WITH SPACES",
		"secrets.WITH!BANG",
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
	in := map[string]string{"A": "  ${{ secrets.API_KEY }}  "}
	got := Resolve(in, map[string]string{"API_KEY": "s3cr3t"})
	if got["A"] != "s3cr3t" {
		t.Errorf("Resolve A = %q, want s3cr3t", got["A"])
	}
}

func TestResolve_UnresolvedLeftVerbatim(t *testing.T) {
	in := map[string]string{"A": "secrets.MISSING"}
	got := Resolve(in, map[string]string{ /* MISSING absent */ })
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

func TestResolve_EmbedIsNotTouched(t *testing.T) {
	// An embedded fragment is not a reference, so it passes through unchanged.
	in := map[string]string{"URL": "https://x/$${{ secrets.TOK }}/y"}
	got := Resolve(in, map[string]string{"TOK": "v"})
	if got["URL"] != "https://x/$${{ secrets.TOK }}/y" {
		t.Errorf("Resolve embedded = %q, want unchanged", got["URL"])
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
