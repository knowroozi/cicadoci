// jwt_test.go: tests for the App-level JWT minter (RS256 only).
//
// Covers:
//   - Zero/empty key failures
//   - Invalid PEM failure
//   - Reproducible header + claims
//   - RSA PKCS#1 successful sign
//   - RSA PKCS#8 successful sign
//   - Non-RSA key fails closed (god conv-e2490b requirement #1)
package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestAppAuthFunc_ZeroAppID(t *testing.T) {
	_, err := AppAuthFunc(context.TODO(), InstallationConfig{AppID: 0, AppPrivateKey: []byte("x")})
	if err == nil {
		t.Fatal("expected error for zero AppID")
	}
}

func TestAppAuthFunc_EmptyKey(t *testing.T) {
	_, err := AppAuthFunc(context.TODO(), InstallationConfig{AppID: 1, AppPrivateKey: nil})
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestAppAuthFunc_InvalidPEM(t *testing.T) {
	_, err := AppAuthFunc(context.TODO(), InstallationConfig{AppID: 1, AppPrivateKey: []byte("not-pem")})
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("expected PEM error, got %v", err)
	}
}

func TestAppAuthFunc_RSAKey(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemBytes := pemToPKCS1(t, key)

	m := nowFixed()
	jwt, err := AppAuthFunc(context.TODO(), InstallationConfig{
		AppID:         12345,
		AppPrivateKey: pemBytes,
		Now:           m,
	})
	if err != nil {
		t.Fatalf("AppAuthFunc: %v", err)
	}
	// Verify the JWT structure.
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}
	// Verify claims
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "12345" {
		t.Errorf("expected iss=12345, got %s", claims.Iss)
	}
	// Signature check.
	signingInput := parts[0] + "." + parts[1]
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	h := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], sigBytes); err != nil {
		t.Errorf("RSA signature verify failed: %v", err)
	}
}

func TestAppAuthFunc_RSAKeyPKCS8(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemBytes := pemToPKCS8(t, key)

	jwt, err := AppAuthFunc(context.TODO(), InstallationConfig{AppID: 99, AppPrivateKey: pemBytes})
	if err != nil {
		t.Fatalf("AppAuthFunc PKCS#8: %v", err)
	}
	if !strings.Contains(jwt, ".") {
		t.Error("expected JWT with dots")
	}
}

// TestAppAuthFunc_NonRSAKeyFailsClosed proves that a non-RSA key
// (ECDSA PKCS#8) produces a clear error, NOT a token (god
// conv-e2490b requirement #1: RS256 only, non-RSA key = error).
func TestAppAuthFunc_NonRSAKeyFailsClosed(t *testing.T) {
	// Use an ECDSA key (supported by x509.MarshalPKCS8PrivateKey) as
	// a stand-in for any non-RSA key. The AppAuthFunc must return a
	// clear error identifying "not RSA" / "RS256 required".
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ecdsa key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ecdsa pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	_, err = AppAuthFunc(context.TODO(), InstallationConfig{AppID: 1, AppPrivateKey: pemBytes})
	if err == nil {
		t.Fatal("expected error for non-RSA (ECDSA) key, got nil")
	}
	if !strings.Contains(err.Error(), "RSA") && !strings.Contains(err.Error(), "not RSA") {
		t.Errorf("expected RSA-related error, got: %v", err)
	}
}

func TestAppAuthFunc_ReproducibleHeaderClaims(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemBytes := pemToPKCS1(t, key)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jwt, err := AppAuthFunc(context.TODO(), InstallationConfig{
		AppID:         42,
		AppPrivateKey: pemBytes,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("AppAuthFunc: %v", err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(parts))
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header["alg"] != "RS256" {
		t.Errorf("expected RS256 alg, got %v", header["alg"])
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "42" {
		t.Errorf("expected iss=42, got %s", claims.Iss)
	}
	// Expiry check: now + 10 minutes.
	expectedExp := now.Add(jwtMaxLifetime).Unix()
	if claims.Exp != expectedExp {
		t.Errorf("expected exp=%d, got %d", expectedExp, claims.Exp)
	}
	// IAT check: now - 30s.
	expectedIat := now.Add(-30 * time.Second).Unix()
	if claims.Iat != expectedIat {
		t.Errorf("expected iat=%d, got %d", expectedIat, claims.Iat)
	}
}

// --- helpers ---

func nowFixed() func() time.Time {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func pemToPKCS1(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func pemToPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
