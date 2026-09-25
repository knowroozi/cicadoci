// Package webhook implements the GitHub App webhook receiver for CICaDo.
//
// It performs:
//
//  1. HMAC-SHA256 signature verification (X-Hub-Signature-256)
//  2. Delivery-ID idempotency tracking (X-GitHub-Delivery header)
//     so replayed deliveries are not re-processed
//  3. Event-type dispatch (push, pull_request, check_suite, check_run)
//
// The receiver is deliberately simple for the MVP: a single-instance
// in-memory delivery tracker.  A Postgres-backed tracker (surviving
// restarts, scaling to multiple instances) is a follow-up ticket.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ErrSignatureMismatch is returned when the provided signature does not match
// the expected HMAC — either the request is unauthorised or the body was
// tampered-in-transit.
var ErrSignatureMismatch = errors.New("webhook: signature mismatch")

// ErrEmptySignature is returned when the X-Hub-Signature-256 header is absent
// or cannot be decoded.
var ErrEmptySignature = errors.New("webhook: missing or malformed signature")

// Verify checks that sig (the X-Hub-Signature-256 header value) is a valid
// HMAC-SHA256 of body using secret as the key.  It uses constant-time
// comparison to avoid timing side channels.
//
// The signature is expected in the format "sha256=<hex>".  If the prefix
// is absent, sig is treated as bare hex for forward-compat.
//
// Returns nil on success, ErrEmptySignature if sig is blank or un-decodable,
// or ErrSignatureMismatch if the MAC does not match.
func Verify(secret, sig string, body []byte) error {
	if sig == "" {
		return ErrEmptySignature
	}

	// Strip the "sha256=" prefix if present.
	const prefix = "sha256="
	hexSig := sig
	if len(sig) > len(prefix) && sig[:len(prefix)] == prefix {
		hexSig = sig[len(prefix):]
	}

	want, err := hex.DecodeString(hexSig)
	if err != nil {
		return ErrEmptySignature
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	// Constant-time comparison to prevent timing attacks.
	if !hmac.Equal(want, mac.Sum(nil)) {
		return ErrSignatureMismatch
	}

	return nil
}
