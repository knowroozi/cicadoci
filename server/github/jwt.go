// jwt.go: App-level JWT minting for the GitHub App→installation leg
// (CIC-146 / CIC-151).
//
// GitHub's REST API requires a short-lived JWT signed with the App's
// private key to authenticate App-level requests, most notably
// POST /app/installations/{id}/access_tokens.
//
// RS256 only (conv-e2490b, god): GitHub App JWTs are RS256; EdDSA is
// not used. A non-RSA key produces a clear error (fail-closed).
//
// The private key bytes are NEVER logged, persisted, or sent to
// runners.
//
// CIC-146 / CIC-151: this is the real App-JWT minter.
// InstallationTokenMinter.Auth is wired to AppAuthFunc at main.go
// when --github-app-id and --github-app-key-file are both set.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// jwtMaxLifetime is the lifetime of an App JWT. 9 minutes gives a 1-minute
// buffer below GitHub's 10-minute hard cap, so a clock-skew edge case does
// not produce a token that is already expired by the time it reaches GitHub.
const jwtMaxLifetime = 9 * time.Minute

// jwtClaims is the RFC 7519 claim set for a GitHub App JWT.
type jwtClaims struct {
	Iss string `json:"iss"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// AppAuthFunc is an InstallationTokenMinter.Auth implementation.
//
// It mints a GitHub App JWT signed (RS256, PKCS#1v15 + SHA-256) with
// the App's PEM-decoded RSA private key.
// Supported PEM encodings:
//   - PKCS#1 RSA ("-----BEGIN RSA PRIVATE KEY-----", GitHub's historical format)
//   - PKCS#8 RSA ("-----BEGIN PRIVATE KEY-----", newer format)
//
// Non-RSA keys produce a clear error (fail-closed).
//
// The private key is never logged, persisted, or sent to runners.
// The returned JWT is only used internally to call the GitHub API;
// it is not exposed outside the server process.
func AppAuthFunc(_ context.Context, cfg InstallationConfig) (string, error) {
	if cfg.AppID == 0 {
		return "", fmt.Errorf("github jwt: AppID not set")
	}
	if len(cfg.AppPrivateKey) == 0 {
		return "", fmt.Errorf("github jwt: AppPrivateKey not set")
	}
	now := time.Now()
	if cfg.Now != nil {
		now = cfg.Now()
	}
	claims := jwtClaims{
		Iss: fmt.Sprintf("%d", cfg.AppID),
		Iat: now.Add(-30 * time.Second).Unix(), // clock skew tolerance
		Exp: now.Add(jwtMaxLifetime).Unix(),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("github jwt: marshal claims: %w", err)
	}

	block, _ := pem.Decode(cfg.AppPrivateKey)
	if block == nil {
		return "", fmt.Errorf("github jwt: could not decode PEM key")
	}

	// Try PKCS#8 first (supports RSA and, in principle, other key types).
	der, derErr := x509.ParsePKCS8PrivateKey(block.Bytes)
	if derErr == nil {
		rsaKey, ok := der.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("github jwt: PKCS#8 key is not RSA (RS256 required, got %T)", der)
		}
		return signRS256(claims, claimsJSON, rsaKey)
	}
	// PKCS#1 RSA (GitHub's historical format).
	rsaKey, rsaErr := x509.ParsePKCS1PrivateKey(block.Bytes)
	if rsaErr == nil {
		return signRS256(claims, claimsJSON, rsaKey)
	}
	return "", fmt.Errorf("github jwt: no supported RSA key format "+
		"(PKCS#1 '-----BEGIN RSA PRIVATE KEY-----' or "+
		"unencrypted PKCS#8 '-----BEGIN PRIVATE KEY-----'): %w", derErr)
}

// signRS256 builds and signs a compact-serialization JWT using
// RS256 (PKCS#1v15 + SHA-256). This is the only algorithm GitHub
// App JWTs support.
func signRS256(claims jwtClaims, claimsJSON []byte, key *rsa.PrivateKey) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("github jwt: marshal header: %w", err)
	}
	encHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := encHeader + "." + encClaims

	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("github jwt: RSA sign: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
