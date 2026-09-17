package auth_test

import (
	"errors"
	"strings"
	"testing"

	"cicadoci.com/cicado/server/auth"
)

func TestGenerateProducesValidDistinctTokens(t *testing.T) {
	seen := make(map[string]bool, 256)
	for i := 0; i < 256; i++ {
		token, err := auth.Generate(auth.KindRunner)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.HasPrefix(token, "cicado_rnr_") {
			t.Fatalf("token %q lacks its kind prefix", token)
		}
		if err := auth.Validate(auth.KindRunner, token); err != nil {
			t.Fatalf("generated token failed validation: %v", err)
		}
		if seen[token] {
			t.Fatal("generate returned a duplicate token")
		}
		seen[token] = true
	}
}

func TestValidateRejectsWrongKindAndJunk(t *testing.T) {
	regToken, err := auth.Generate(auth.KindRegistration)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// A registration token must not pass as a runner credential.
	if err := auth.Validate(auth.KindRunner, regToken); !errors.Is(err, auth.ErrMalformedToken) {
		t.Errorf("validating a registration token as a runner credential gave %v, want ErrMalformedToken", err)
	}

	for _, bad := range []string{
		"",
		"cicado_rnr_",
		"cicado_rnr_short",
		"cicado_rnr_!!!not-base64!!!",
		strings.Repeat("a", 200),
	} {
		if err := auth.Validate(auth.KindRunner, bad); !errors.Is(err, auth.ErrMalformedToken) {
			t.Errorf("Validate(%q) = %v, want ErrMalformedToken", bad, err)
		}
	}
}

func TestHashIsStableAndDistinct(t *testing.T) {
	a, _ := auth.Generate(auth.KindJob)
	b, _ := auth.Generate(auth.KindJob)

	if !auth.Equal(auth.Hash(a), auth.Hash(a)) {
		t.Error("hashing the same token twice gave different results")
	}
	if auth.Equal(auth.Hash(a), auth.Hash(b)) {
		t.Error("distinct tokens hashed to the same value")
	}
	if len(auth.Hash(a)) != 32 {
		t.Errorf("hash length = %d, want 32", len(auth.Hash(a)))
	}
}

func TestKindOf(t *testing.T) {
	token, _ := auth.Generate(auth.KindMCP)
	kind, ok := auth.KindOf(token)
	if !ok || kind != auth.KindMCP {
		t.Errorf("KindOf = %q, %v; want mcp, true", kind, ok)
	}
	if _, ok := auth.KindOf("github_pat_whatever"); ok {
		t.Error("KindOf accepted a token that is not ours")
	}
}
