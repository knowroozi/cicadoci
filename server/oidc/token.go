// Package oidc issues and verifies the job-identity JWTs CICaDo federates to a
// cloud identity provider (M5, the OIDC issuer). A job that declares
// `identity: aws` runs its credential exchange like this: the server signs a
// short-lived, self-contained JWT describing *this run and this job*; the runner
// presents it to AWS `sts:AssumeRoleWithWebIdentity`; AWS verifies it against a
// JWKS the server publishes; AWS returns short-lived credentials for the target
// role.
//
// This is how CICaDo honours the project's "no long-lived cloud keys anywhere"
// invariant: a deploy job assumes a role with zero stored AWS keys. AWS holds an
// OIDC provider that trusts this server's key set; we hold no AWS secret.
//
// The package is a leaf: it depends only on the standard library and google/uuid
// (an existing project dependency). It performs no IO, knows nothing about AWS,
// and is safe to call inside the control plane on the hot path of a run.
package oidc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Algorithm names the signature algorithm a token was signed with. Only ES256
// (NIST P-256, SHA-256) is supported in MVP; the Signer seam leaves room for
// RS256 or EdDSA later without changing the wire format.
type Algorithm string

const (
	// AlgoES256 is the MVP signature algorithm: ECDSA P-256 with SHA-256.
	AlgoES256 = "ES256"
)

// The errors below wrap every JWT failure. Callers should use errors.Is to
// distinguish only when it aids a message; otherwise any of them means "do not
// mint / do not redeem". They are not user-facing diagnostics.
var (
	// ErrMalformedToken is returned when the compact serialization is not a
	// three-segment, base64url-encoded JWT.
	ErrMalformedToken = errors.New("oidc: malformed token")
	// ErrExpiredToken is returned when the token's `exp` is in the past.
	ErrExpiredToken = errors.New("oidc: token expired")
	// ErrNotYetValid is returned when the token's `iat` is in the future.
	ErrNotYetValid = errors.New("oidc: token not yet valid")
	// ErrAudienceMismatch is returned when `aud` does not match the expected
	// audience (for AWS this is the federated identity's client id).
	ErrAudienceMismatch = errors.New("oidc: audience mismatch")
	// ErrIssuerMismatch is returned when `iss` does not match this server's
	// issuer identity.
	ErrIssuerMismatch = errors.New("oidc: issuer mismatch")
	// ErrUnknownKID is returned when the token's `kid` is not in the local key
	// set (the key rotated out, or this is the wrong server).
	ErrUnknownKID = errors.New("oidc: unknown kid")
	// ErrAlgorithmMismatch is returned when the header `alg` is one we refuse to
	// verify. This is how the `alg:none` downgrade attack is closed: a verifier
	// recognizes only the one algorithm this issuer signs with.
	ErrAlgorithmMismatch = errors.New("oidc: unsupported algorithm")
	// ErrBadSignature is returned when no known key verified the token.
	ErrBadSignature = errors.New("oidc: bad signature")
	// ErrEmptySubject is returned at mint time when the claims name no job.
	ErrEmptySubject = errors.New("oidc: claims subject is empty")
	// ErrEmptyAudience is returned at mint time when the claims name no
	// audience. An empty audience is rejected on both mint and verify
	// paths: a token with no `aud` is accepted by any verifier that
	// forgets to check it, which is fail-open. Fail-closed instead.
	ErrEmptyAudience = errors.New("oidc: claims audience is empty")
	// ErrBadExpiry is returned at mint time when the TTL is non-positive, which
	// mints a token that is already expired or never valid.
	ErrBadExpiry = errors.New("oidc: ttl must be positive")
	// ErrMalformedJWKS is returned when a JWKS document does not describe a usable
	// P-256 key.
	ErrMalformedJWKS = errors.New("oidc: malformed jwks")
)

// base64URL is base64url without padding, the encoding JWTs (RFC 7515) use.
var base64URL = base64.RawURLEncoding

// SubjectFor builds the canonical `sub` for a job-identity token,
// "job:<run>:<job>". A subject ties credentials to this run and this job, never
// to an org or repo wholesale — that scoping is what lets one signing key serve
// every job on the server without widening any role's blast radius.
//
// Both ids are required and may not contain a colon: run and job ids are UUIDs
// (colon-free by construction), and a colon in either would let
// SubjectFor("a:b","c") and SubjectFor("a","b:c") both yield "job:a:b:c",
// colliding two unrelated jobs into one `sub` — an AWS trust condition on
// `sub` could no longer tell them apart. An empty part is also rejected: a
// token that names no job authorizes nothing, a footgun not a default.
func SubjectFor(runID, jobID string) (string, error) {
	if runID == "" || jobID == "" {
		return "", errors.New("oidc: subject requires both run and job ids")
	}
	if strings.ContainsAny(runID, ":") || strings.ContainsAny(jobID, ":") {
		return "", errors.New("oidc: run and job ids must not contain a colon")
	}
	return "job:" + runID + ":" + jobID, nil
}

// parseSubject splits a "job:<run>:<job>" subject back into its run and job. It
// is the inverse of SubjectFor and lets a verifier re-derive run/job scope from
// a token a caller did not hand it. SubjectFor guarantees neither id carries a
// colon, so the split on the first colon after "job:" is unambiguous: the run is
// the segment before the second colon and the job is the remainder. A malformed
// subject (no "job:" prefix, an empty half, or a colon in an id) is reported not
// parsed, so a verifier keys off `sub` and never off a half-split.
func parseSubject(sub string) (run, job string, ok bool) {
	rest, ok := strings.CutPrefix(sub, "job:")
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	// Defend the invariant even for a token from another issuer or a hand-
	// crafted one: a colon in either half means it was not produced by
	// SubjectFor, so do not half-split it.
	if strings.ContainsAny(parts[0], ":") || strings.ContainsAny(parts[1], ":") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// jwtClaims is the raw claim set serialized as JSON. Standard claims use the
// RFC 7519 register (iss/sub/aud/iat/exp/jti); every CICaDo-specific name is a
// lowercase word without a dash so it cannot collide with a future registered
// claim. (A leading dash is the JWT convention for "not a registered claim";
// the run-context fields a verifier may key off carry no dash.)
type jwtClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Issued   int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	JTI      string `json:"jti"`
	// CICaDo run/context claims. Optional and read-only to a verifier; a consumer
	// such as AWS ignores the ones it does not use.
	Job    string `json:"job,omitempty"`
	Run    string `json:"run,omitempty"`
	Branch string `json:"branch,omitempty"`
	Repo   string `json:"repo,omitempty"`
	SHA    string `json:"sha,omitempty"`
}

// Claims is the human-facing input to Issuer.Issue. A caller fills the run
// context; the issuer stamps iss/iat/exp/jti and the subject.
type Claims struct {
	// Subject is the `sub`, typically from SubjectFor(run, job). Required.
	Subject string
	// Audience is the `aud`: the identity this token federates into. For AWS this
	// is the OIDC client id the role's trust policy names. Required: a token
	// minted with no audience would be accepted by any verifier that forgets to
	// check it; the mint and verify paths both reject it.
	Audience string
	// `iss` is the issuer's own identity and is stamped by the issuer, not
	// supplied per token: there is no legitimate reason for one issuer process to
	// mint a token claiming a different `iss`, and a per-token override would let
	// a bug or a compromised caller mint tokens attributed to another issuer that
	// shares the key. Job/Run are derived from Subject in the issuer; the caller
	// only supplies run context.
	// The run context a consumer (or a human reading the audit log) reads back.
	Job    string
	Run    string
	Branch string
	Repo   string
	SHA    string
}

// jwtHeader is the JOSE header. `kid` selects the verifying key; `alg` is fixed
// to the issuer's algorithm so a verifier is never talked into another one.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// compact is the serialized (unsigned) part of a JWT.
type compact struct {
	Header  string
	Payload string
}

// serialize renders a header + claim set into the "h.p" signing input: the
// base64url(header) and base64url(payload) joined by '.', exactly what the
// signature covers and what a JWT is on the wire minus its signature.
func serialize(header jwtHeader, c jwtClaims) (compact, error) {
	hb, err := json.Marshal(header)
	if err != nil {
		return compact{}, err
	}
	pb, err := json.Marshal(c)
	if err != nil {
		return compact{}, err
	}
	return compact{
		Header:  base64URL.EncodeToString(hb),
		Payload: base64URL.EncodeToString(pb),
	}, nil
}

// decode splits and base64url-decodes a compact token into its header and claims.
// It validates structure only; the caller checks the signature and the claims.
func decode(serial string) (jwtHeader, jwtClaims, string, error) {
	parts := strings.Split(serial, ".")
	if len(parts) != 3 {
		return jwtHeader{}, jwtClaims{}, "", ErrMalformedToken
	}
	hb, err := base64URL.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, jwtClaims{}, "", ErrMalformedToken
	}
	pb, err := base64URL.DecodeString(parts[1])
	if err != nil {
		return jwtHeader{}, jwtClaims{}, "", ErrMalformedToken
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return jwtHeader{}, jwtClaims{}, "", ErrMalformedToken
	}
	if hdr.Alg == "" || hdr.Kid == "" {
		return jwtHeader{}, jwtClaims{}, "", ErrMalformedToken
	}
	var c jwtClaims
	if err := json.Unmarshal(pb, &c); err != nil {
		return jwtHeader{}, jwtClaims{}, "", ErrMalformedToken
	}
	return hdr, c, parts[0] + "." + parts[1], nil
}

// now is the production clock. VerifyOptions.Clock overrides it under test so
// expiry is deterministic.
func now() time.Time { return time.Now().UTC() }
