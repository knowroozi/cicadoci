// Package auth mints and verifies the bearer tokens the system issues.
//
// Every bearer secret — runner registration tokens, runner credentials, job
// tokens, MCP tokens, API tokens — lives in one table, hashed. The plaintext is
// shown once at mint time and is not recoverable afterwards. That gives one
// revocation path, one expiry check, and one place to record a use.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Kind identifies what a token authorizes. It matches the tokens.kind check
// constraint in the schema.
type Kind string

const (
	KindRegistration Kind = "registration"
	KindRunner       Kind = "runner"
	KindJob          Kind = "job"
	KindMCP          Kind = "mcp"
	KindAPI          Kind = "api"
)

// Prefixes make a leaked token identifiable on sight — in a paste, a log, or a
// secret scanner — and let us reject an obviously wrong kind before spending a
// database round trip on it.
var prefixes = map[Kind]string{
	KindRegistration: "cicado_reg_",
	KindRunner:       "cicado_rnr_",
	KindJob:          "cicado_job_",
	KindMCP:          "cicado_mcp_",
	KindAPI:          "cicado_api_",
}

// ErrMalformedToken is returned when a presented token is not well formed for
// its kind. It is deliberately indistinguishable from "not found" at the API
// boundary: callers should not be able to probe which tokens exist.
var ErrMalformedToken = errors.New("auth: malformed token")

// tokenEntropyBytes is 256 bits of randomness. These are bearer secrets with no
// rate limit on the gRPC path beyond the usual, so they are sized to make
// guessing irrelevant rather than merely impractical.
const tokenEntropyBytes = 32

// Generate mints a new token of the given kind and returns the plaintext. Store
// only Hash(plaintext); the plaintext is shown to its holder once.
func Generate(kind Kind) (string, error) {
	prefix, ok := prefixes[kind]
	if !ok {
		return "", fmt.Errorf("auth: unknown token kind %q", kind)
	}
	buf := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Hash returns the SHA-256 of a token, which is what the database stores.
//
// A plain hash rather than a password KDF is correct here: these are 256-bit
// random values, not human-chosen secrets, so there is no dictionary to stretch
// against, and the lookup is on the hot path of every runner reconnect.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// KindOf reports which kind a token claims to be, from its prefix. The claim is
// advisory: the authoritative kind is the one stored in the database.
func KindOf(token string) (Kind, bool) {
	for kind, prefix := range prefixes {
		if strings.HasPrefix(token, prefix) {
			return kind, true
		}
	}
	return "", false
}

// Validate checks a token is well formed for the given kind, before any
// database lookup.
func Validate(kind Kind, token string) error {
	prefix, ok := prefixes[kind]
	if !ok {
		return fmt.Errorf("auth: unknown token kind %q", kind)
	}
	if !strings.HasPrefix(token, prefix) {
		return ErrMalformedToken
	}
	body := strings.TrimPrefix(token, prefix)
	decoded, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(decoded) != tokenEntropyBytes {
		return ErrMalformedToken
	}
	return nil
}

// Equal compares two token hashes in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
