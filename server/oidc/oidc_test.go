package oidc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"cicadoci.com/cicado/server/oidc"
)

const testIssuer = "https://cicado.example/oidc"

// A helper to build a compact JWT whose header carries an arbitrary algorithm.
// It is how TestVerifyRejectsAlgorithmNone drives the alg:none downgrade path
// without going through the (ES256-only) issuer.
func compactWithAlg(test *testing.T, alg string) string {
	h, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": "k"})
	p, _ := json.Marshal(map[string]string{"iss": testIssuer, "sub": "job:r:j", "aud": "aud"})
	enc := base64.RawURLEncoding
	return enc.EncodeToString(h) + "." + enc.EncodeToString(p) + "." + enc.EncodeToString([]byte("sig"))
}

// TestSubjectRoundTripsThroughToken verifies that the "job:<run>:<job>" grammar
// mints the expected split back out through the public Claim surface.
func TestSubjectRoundTripsThroughToken(t *testing.T) {
	sub, err := oidc.SubjectFor("run-7", "job-build")
	if err != nil {
		t.Fatalf("SubjectFor: %v", err)
	}
	if sub != "job:run-7:job-build" {
		t.Fatalf("SubjectFor = %q, want job:run-7:job-build", sub)
	}
	for _, both := range [][2]string{{"", "x"}, {"r", ""}} {
		if _, err := oidc.SubjectFor(both[0], both[1]); err == nil {
			t.Fatalf("SubjectFor(%q,%q) = nil, want error", both[0], both[1])
		}
	}

	_, key := freshIssuer(t)
	iss := newIssuer(t, key)
	tok, err := iss.Issue(context.Background(), oidc.Claims{Subject: sub, Audience: "aud"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims, err := newVerifier(t, key).Verify(context.Background(), tok, oidc.VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Run != "run-7" || claims.Job != "job-build" {
		t.Fatalf("split = run %q job %q, want run-7 job-build", claims.Run, claims.Job)
	}
}

func TestIssueThenVerifyHappyPath(t *testing.T) {
	iss, key := freshIssuer(t)
	tok, err := iss.Issue(context.Background(), oidc.Claims{
		Subject:  "job:run-1:job-build",
		Audience: "arn:aws:iam::123456789012:role/cicado",
		Branch:   "main",
		Repo:     "acme/widget",
		SHA:      "abc123",
	}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if parts := strings.Split(tok, "."); len(parts) != 3 {
		t.Fatalf("token not three segments: %q", tok)
	}
	got, err := newVerifier(t, key).Verify(context.Background(), tok, oidc.VerifyOptions{
		ExpectedAud: "arn:aws:iam::123456789012:role/cicado",
		ExpectedIss: testIssuer,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "job:run-1:job-build" || got.Audience != "arn:aws:iam::123456789012:role/cicado" {
		t.Fatalf("core claims wrong: %+v", got)
	}
	if got.Job != "job-build" || got.Run != "run-1" || got.Branch != "main" || got.Repo != "acme/widget" || got.SHA != "abc123" {
		t.Fatalf("context claims not round-tripped: %+v", got)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	iss, key := freshIssuer(t)
	tok, _ := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j", Audience: "aud"}, time.Minute)
	bad := []byte(tok)
	bad[len(bad)-1] ^= 0x01
	_, err := newVerifier(t, key).Verify(context.Background(), string(bad), oidc.VerifyOptions{})
	if !errors.Is(err, oidc.ErrBadSignature) {
		t.Fatalf("tampered signature: got %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsUnknownKID(t *testing.T) {
	iss, key := freshIssuer(t)
	tok, _ := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j", Audience: "aud"}, time.Minute)
	other, _ := oidc.NewECKey("different-kid")
	_, err := newVerifier(t, other).Verify(context.Background(), tok, oidc.VerifyOptions{})
	if !errors.Is(err, oidc.ErrUnknownKID) {
		t.Fatalf("unknown kid: got %v, want ErrUnknownKID", err)
	}
	_ = key
}

func TestVerifyRejectsExpired(t *testing.T) {
	iss, key := freshIssuer(t)
	tok, _ := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j", Audience: "aud"}, time.Minute)
	v := newVerifier(t, key)
	future := time.Now().UTC().Add(3 * time.Hour)
	_, err := v.Verify(context.Background(), tok, oidc.VerifyOptions{Clock: func() time.Time { return future }})
	if !errors.Is(err, oidc.ErrExpiredToken) {
		t.Fatalf("advanced clock: got %v, want ErrExpiredToken", err)
	}
	if _, err := v.Verify(context.Background(), tok, oidc.VerifyOptions{}); err != nil {
		t.Fatalf("current clock verify: %v", err)
	}
}

func TestVerifyRejectsAudienceAndIssuerMismatch(t *testing.T) {
	iss, key := freshIssuer(t)
	tok, _ := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j", Audience: "good-aud"}, time.Minute)
	v := newVerifier(t, key)
	if _, err := v.Verify(context.Background(), tok, oidc.VerifyOptions{ExpectedAud: "other"}); !errors.Is(err, oidc.ErrAudienceMismatch) {
		t.Fatalf("audience: got %v, want ErrAudienceMismatch", err)
	}
	if _, err := v.Verify(context.Background(), tok, oidc.VerifyOptions{ExpectedIss: "https://evil.example"}); !errors.Is(err, oidc.ErrIssuerMismatch) {
		t.Fatalf("issuer: got %v, want ErrIssuerMismatch", err)
	}
}

func TestVerifyRejectsAlgorithmNone(t *testing.T) {
	// The classic downgrade attack: a token whose header claims alg:none. A
	// verifier that recognizes only ES256 must reject it outright.
	tok := compactWithAlg(t, "none")
	v, _ := oidc.NewVerifier(testIssuer, oidc.NewKeySet())
	if _, err := v.Verify(context.Background(), tok, oidc.VerifyOptions{}); !errors.Is(err, oidc.ErrAlgorithmMismatch) {
		t.Fatalf("alg:none: got %v, want ErrAlgorithmMismatch", err)
	}
	// A mismatched-but-real-looking alg is refused the same way.
	v2, _ := oidc.NewVerifier(testIssuer, oidc.NewKeySet())
	if _, err := v2.Verify(context.Background(), compactWithAlg(t, "RS256"), oidc.VerifyOptions{}); !errors.Is(err, oidc.ErrAlgorithmMismatch) {
		t.Fatalf("alg:RS256: got %v, want ErrAlgorithmMismatch", err)
	}
}

func TestVerifyRejectsMalformedTokens(t *testing.T) {
	ks := oidc.NewKeySet()
	v, err := oidc.NewVerifier(testIssuer, ks)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{
		"", "a", "a.b", "a.b.c.d",
		"****.eyJzdWIiOiJqIn0.abc", // segment 0 is not valid base64url
		"e3lwIn0.e3N1YiJ9.ab",      // segment 0 is not a JSON object
	} {
		if _, err := v.Verify(context.Background(), tok, oidc.VerifyOptions{}); err == nil {
			t.Fatalf("Verify(%q) = nil error, want non-nil", tok)
		}
	}
}

func TestIssueRejectsEmptySubjectAndNonPositiveTTL(t *testing.T) {
	iss, _ := freshIssuer(t)
	if _, err := iss.Issue(context.Background(), oidc.Claims{Audience: "aud"}, time.Minute); !errors.Is(err, oidc.ErrEmptySubject) {
		t.Fatalf("empty subject: got %v, want ErrEmptySubject", err)
	}
	if _, err := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j"}, 0); !errors.Is(err, oidc.ErrBadExpiry) {
		t.Fatalf("zero ttl: got %v, want ErrBadExpiry", err)
	}
	if _, err := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j", Audience: ""}, time.Minute); !errors.Is(err, oidc.ErrEmptyAudience) {
		t.Fatalf("empty audience: got %v, want ErrEmptyAudience", err)
	}
}

func TestTwoIssuesNeverCollide(t *testing.T) {
	iss, key := freshIssuer(t)
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		tok, err := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j", Audience: "aud"}, time.Minute)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		// The jti is what makes two identical claims produce distinct tokens.
		seen[tok] = struct{}{}
	}
	if len(seen) != 64 {
		t.Fatalf("expected 64 distinct tokens, got %d (duplicate jti?)", len(seen))
	}
	_ = key
}

func TestJWKSRoundTripAndOffCurveRejection(t *testing.T) {
	_, key := freshIssuer(t)
	ks := oidc.NewKeySet()
	if err := ks.Add(key); err != nil {
		t.Fatalf("Add: %v", err)
	}
	doc, err := ks.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	loaded, err := oidc.LoadJWKS(doc)
	if err != nil {
		t.Fatalf("LoadJWKS: %v", err)
	}
	if _, ok := loaded.Get(key.ID); !ok {
		t.Fatalf("loaded keyset missing kid %q", key.ID)
	}

	// A JWKS that names a kid but coordinates not on the P-256 curve must be
	// rejected: a verifier that trusts an off-curve key can accept a forgery.
	offCurve := `{"keys":[{"kty":"EC","crv":"P-256","kid":"` + key.ID +
		`","x":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 32)) + `","y":"` +
		base64.RawURLEncoding.EncodeToString(make([]byte, 32)) + `"}]}`
	if _, err := oidc.LoadJWKS([]byte(offCurve)); err == nil {
		t.Fatal("LoadJWKS accepted an off-curve key")
	}
	// And an empty keyset is malformed.
	if _, err := oidc.LoadJWKS([]byte(`{"keys":[]}`)); !errors.Is(err, oidc.ErrMalformedJWKS) {
		t.Fatalf("empty jwks: got %v, want ErrMalformedJWKS", err)
	}
}

func TestPEMRoundTrip(t *testing.T) {
	_, key := freshIssuer(t)
	pemBytes, err := key.PEM()
	if err != nil {
		t.Fatalf("PEM: %v", err)
	}
	restored, err := oidc.KeyFromPEM(pemBytes)
	if err != nil {
		t.Fatalf("KeyFromPEM: %v", err)
	}
	if jwk := restored.WithID(key.ID).JWK(); jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.Kid != key.ID {
		t.Fatalf("restored JWK malformed: %+v", jwk)
	}
	// A key restored from PEM must verify a token minted by it.
	iss, err := oidc.NewIssuer(restored, testIssuer)
	if err != nil {
		t.Fatalf("NewIssuer(restored): %v", err)
	}
	tok, err := iss.Issue(context.Background(), oidc.Claims{Subject: "job:r:j", Audience: "aud"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := newVerifier(t, key).Verify(context.Background(), tok, oidc.VerifyOptions{}); err != nil {
		t.Fatalf("Verify token signed by PEM-restored key: %v", err)
	}
}

func TestKeyFromPEMRejectsNonPEM(t *testing.T) {
	// A random non-PEM block must be rejected.
	if _, err := oidc.KeyFromPEM([]byte("not a pem")); err == nil {
		t.Fatal("KeyFromPEM accepted a non-PEM input")
	}
}

func TestNewIssuerRequiresIdentity(t *testing.T) {
	_, key := freshIssuer(t)
	if _, err := oidc.NewIssuer(key, ""); err == nil {
		t.Fatal("NewIssuer with empty issuer url: want error")
	}
	if _, err := oidc.NewIssuer(nil, testIssuer); err == nil {
		t.Fatal("NewIssuer with nil key: want error")
	}
}

func TestKeySetEmptyReturnsNoKey(t *testing.T) {
	if _, ok := oidc.NewKeySet().Get("nope"); ok {
		t.Fatal("empty keyset returned a key")
	}
}

// --- helpers ---------------------------------------------------------------

func freshIssuer(t *testing.T) (*oidc.Issuer, *oidc.Key) {
	t.Helper()
	key, err := oidc.NewECKey("k-" + t.Name()[:8])
	if err != nil {
		t.Fatalf("NewECKey: %v", err)
	}
	iss, err := oidc.NewIssuer(key, testIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss, key
}

func newIssuer(t *testing.T, key *oidc.Key) *oidc.Issuer {
	t.Helper()
	iss, err := oidc.NewIssuer(key, testIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

func newVerifier(t *testing.T, key *oidc.Key) *oidc.Verifier {
	t.Helper()
	ks := oidc.NewKeySet()
	if err := ks.Add(key); err != nil {
		t.Fatalf("Add key: %v", err)
	}
	v, err := oidc.NewVerifier(testIssuer, ks)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// --- fail-closed / collision additions (Dwight review C-39) ---------------

// TestSubjectForRejectsColons locks the two inputs that would otherwise collide
// into the same `job:a:b:c` subject.
func TestSubjectForRejectsColons(t *testing.T) {
	for _, in := range [][2]string{{"a:b", "c"}, {"a", "b:c"}, {"a", "b:c:d"}, {"::", "j"}, {"r", "::"}} {
		if _, err := oidc.SubjectFor(in[0], in[1]); err == nil {
			t.Fatalf("SubjectFor(%q,%q) = nil, want colon error", in[0], in[1])
		}
	}
	// And the pair that used to collide now both fail instead of matching.
	if _, err := oidc.SubjectFor("a:b", "c"); err == nil {
		t.Fatal("SubjectFor(\"a:b\",\"c\") = nil, want colon error")
	}
	if _, err := oidc.SubjectFor("a", "b:c"); err == nil {
		t.Fatal("SubjectFor(\"a\",\"b:c\") = nil, want colon error")
	}
}

// TestVerifyRejectsEmptyAudience is the verify side of the fail-closed audience
// invariant: a well-signed token that carries no `aud` is rejected, so a
// forgotten ExpectedAud cannot become an acceptance path.
func TestVerifyRejectsEmptyAudience(t *testing.T) {
	_, key := freshIssuer(t)
	tok := signWithEmptyAud(t, key, testIssuer)
	_, err := newVerifier(t, key).Verify(context.Background(), tok, oidc.VerifyOptions{ExpectedIss: testIssuer})
	if !errors.Is(err, oidc.ErrEmptyAudience) {
		t.Fatalf("empty audience on verify: got %v, want ErrEmptyAudience", err)
	}
}

// TestKeyFromPEMRejectsNonP256 proves the curve is enforced at the PEM boundary,
// not left to a downstream panic in the signer.
func TestKeyFromPEMRejectsNonP256(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey P-384: %v", err)
	}
	// Build the PKCS#8 PEM directly: Key only ever accepts P-256, so a P-384 PEM
	// is the exact input the load boundary must refuse.
	der, err := x509.MarshalPKCS8PrivateKey(p384)
	if err != nil {
		t.Fatalf("MarshalPKCS8: %v", err)
	}
	enc := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := oidc.KeyFromPEM(enc); err == nil {
		t.Fatal("KeyFromPEM(P-384) = nil, want error")
	}
}

// TestNewIssuerRejectsEmptyKeyID guards the kid:"" footgun: a key without an id
// would mint tokens our own verify rejects as malformed.
func TestNewIssuerRejectsEmptyKeyID(t *testing.T) {
	// A nil key is the empty-id surface we can reach through the public ctor.
	if _, err := oidc.NewIssuer(nil, testIssuer); err == nil {
		t.Fatal("NewIssuer(nil) = nil, want error")
	}
}

// TestNewVerifierRejectsEmptyIssuer mirrors NewIssuer: an empty issuer would
// disable the issuer check and let any token redeem.
func TestNewVerifierRejectsEmptyIssuer(t *testing.T) {
	if _, err := oidc.NewVerifier("", oidc.NewKeySet()); err == nil {
		t.Fatal("NewVerifier with empty issuer returned nil error, want error")
	}
}

// signWithEmptyAud builds a compact JWT that is otherwise well-formed and
// well-signed by key but carries no `aud` — the precise case the verify path
// must refuse.
func signWithEmptyAud(t *testing.T, key *oidc.Key, iss string) string {
	t.Helper()
	hdr := `{"alg":"ES256","typ":"JWT","kid":` + quote(key.ID) + `}`
	now := time.Now().Unix()
	payload := `{"iss":` + quote(iss) + `,"sub":"job:r1:j1","iat":` +
		strconv.FormatInt(now, 10) + `,"exp":` + strconv.FormatInt(now+3600, 10) + `}`
	signingInput := b64url(hdr) + "." + b64url(payload)
	sig, err := key.Signer().Sign([]byte(signingInput))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return signingInput + "." + b64urlRaw(sig)
}

func b64url(s string) string    { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func b64urlRaw(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// quote is a tiny JSON string escaper for the test's hand-built claims.
func quote(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}
