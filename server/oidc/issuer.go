package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The fixed identifiers a JWK or PEM block carries, named once so call sites
// cannot drift.
const (
	curveNameJWK = "P-256"
	curveJWKType = "EC"
	pemBlockType = "PRIVATE KEY" // PKCS#8, as produced by x509.MarshalPKCS8PrivateKey
)

// curveParams is the NIST P-256 curve every key in this package lives on. A JWK
// that names another curve is rejected at parse time, so verifying a token is
// safe even if a forged JWK offers a different one.
var curveParams = elliptic.P256()

// Signer signs a JWT signing input. It is a tiny seam so the package can add
// RS256 or EdDSA later without touching the token or verifier; only ES256 exists
// in MVP.
type Signer interface {
	// Alg is the JOSE algorithm this signer applies.
	Alg() Algorithm
	// Sign returns the IEEE P-1363 signature bytes (fixed-width r || s, 64
	// bytes for P-256) over the input — the exact form JWT and AWS's OIDC trust
	// expect.
	Sign(signingInput []byte) ([]byte, error)
	// Public is the verifying half of the key, exported as a JWK.
	Public() JWK
}

// ecdsaSigner signs with NIST P-256. ecdsa.Sign is not deprecated and yields the
// P-1363 form JWT requires without any curve-library dependency — which matters:
// ADR-005 forbids third-party code in the control plane and ADR-008 budgets the
// dependency set, so pulling filippo.io/nistec for a few coordinate reads would
// be a worse sin than the few staticcheck exemptions below.
type ecdsaSigner struct {
	key *ecdsa.PrivateKey
	kid string
}

func (s *ecdsaSigner) Alg() Algorithm { return AlgoES256 }

func (s *ecdsaSigner) Sign(input []byte) ([]byte, error) {
	digest := sha256.Sum256(input)
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return nil, err
	}
	// P-256 has a 32-byte field, so r and s each occupy exactly 32 bytes,
	// zero-padded on the left: a stable 64-byte P-1363 signature.
	out := make([]byte, 64)
	r.FillBytes(out[0:32])
	ss.FillBytes(out[32:64])
	return out, nil
}

func (s *ecdsaSigner) Public() JWK {
	jwk := jwkOfP256(&s.key.PublicKey)
	jwk.Kid = s.kid
	return jwk
}

// Key is a signing key pair plus its id. The id is the `kid` a token carries so
// a verifier selects the right key; it survives rotation because old keys stay
// verifiable until their tokens' expiries pass.
type Key struct {
	ID string
	ec *ecdsa.PrivateKey
}

// NewECKey generates a fresh NIST P-256 key. Operators persist the value from
// Key.PEM and rotate on a fixed schedule (see docs/decisions/CIC-39-oidc-issuer
// .md). An id is required: a key with no operator-assigned id mints an ambiguous
// kid.
func NewECKey(id string) (*Key, error) {
	if id == "" {
		return nil, errors.New("oidc: key id is required")
	}
	ec, err := ecdsa.GenerateKey(curveParams, rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Key{ID: id, ec: ec}, nil
}

// PEM encodes the private key as a PKCS#8 PEM block. This is the only thing an
// operator stores and the only form of the key that ever leaves the process. An
// EC key round-trips through PKCS#8 cleanly, and x509 stays current, so there is
// no deprecated-API cost here.
func (k *Key) PEM() ([]byte, error) {
	raw, err := x509.MarshalPKCS8PrivateKey(k.ec)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemBlockType, Bytes: raw}), nil
}

// KeyFromPEM loads a key pair from a PKCS#8 PEM block, the inverse of Key.PEM.
// It does not assign an id (the id travels with the JWKS, not the PEM); restore
// it with Key.WithID before handing the key to a verifier.
func KeyFromPEM(pemBytes []byte) (*Key, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != pemBlockType {
		return nil, errors.New("oidc: not a PKCS#8 PRIVATE KEY PEM")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := priv.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("oidc: PEM is not an EC private key")
	}
	// Enforce P-256 here at the load boundary: a P-384/P-521 key mints a 48/66-byte
	// signature that ecdsaSigner.Sign silently truncates to 32 via big.Int.
	// FillBytes, which truncates or panics on overflow. Rejecting the curve at load
	// is the seam that makes "this package only ever mints P-256 tokens" true for a
	// PEM an operator may have generated the wrong way.
	if ec.Curve != curveParams { //nolint:staticcheck // SA1019: ecdh exposes no curve()
		return nil, errors.New("oidc: key is not P-256")
	}
	return &Key{ec: ec}, nil
}

// WithID sets the key's id (its kid) and returns the key, for chaining.
func (k *Key) WithID(id string) *Key {
	k.ID = id
	return k
}

// Signer returns the Signer backed by this key pair.
func (k *Key) Signer() Signer { return &ecdsaSigner{key: k.ec, kid: k.ID} }

// JWK is a JSON Web Key: the public half of a key, in the shape a JWKS
// (RFC 7517) publishes and a verifier consumes.
type JWK struct {
	Kty string `json:"kty"` // "EC"
	Crv string `json:"crv"` // "P-256"
	X   string `json:"x"`   // base64url of the X coordinate
	Y   string `json:"y"`   // base64url of the Y coordinate
	Kid string `json:"kid,omitempty"`
}

// jwkOfP256 exports the public half of a P-256 key as a JWK carrying no id.
// A JWK must carry the bare coordinates, and the standard library's blessed
// replacement for ecdsa — crypto/ecdh — does not expose them, so the only
// dependency-free route to a leaf package is ecdsa's own coordinates. They read
// deprecated in Go 1.26, so each access carries a targeted, documented exemption
// rather than a third-party curve library (ADR-005/ADR-008, documented in
// docs/decisions/CIC-39-oidc-issuer.md).
func jwkOfP256(pub *ecdsa.PublicKey) JWK {
	xx := make([]byte, 32)
	yy := make([]byte, 32)
	pub.X.FillBytes(xx) //nolint:staticcheck // SA1019: ecdh exposes no coords; see docs
	pub.Y.FillBytes(yy) //nolint:staticcheck // SA1019: ecdh exposes no coords; see docs
	return JWK{
		Kty: curveJWKType,
		Crv: curveNameJWK,
		X:   base64URL.EncodeToString(xx),
		Y:   base64URL.EncodeToString(yy),
	}
}

// JWK builds the JWK this key would publish, carrying its own id.
func (k *Key) JWK() JWK {
	jwk := jwkOfP256(&k.ec.PublicKey)
	jwk.Kid = k.ID
	return jwk
}

// Issuer mints job-identity tokens. It holds one signing key and the server's
// issuer identity; call Issue for each job that declares a cloud identity.
type Issuer struct {
	issuer string
	signer Signer
}

// NewIssuer constructs an issuer that signs with key and stamps every token with
// issuerURL as its `iss`.
func NewIssuer(key *Key, issuerURL string) (*Issuer, error) {
	if issuerURL == "" {
		return nil, errors.New("oidc: issuer url is required")
	}
	if key == nil || key.ec == nil {
		return nil, errors.New("oidc: nil key")
	}
	if key.ID == "" {
		// A key with no id mints every token with kid:""; our own decode rejects an
		// empty kid as ErrMalformedToken, so the issuer would silently mint tokens
		// nothing (not even itself) can verify. KeySet.Add already refuses an empty
		// id; NewIssuer must too.
		return nil, errors.New("oidc: key id is required")
	}
	return &Issuer{issuer: issuerURL, signer: key.Signer()}, nil
}

// Issue mints a token for c, valid for ttl from now. It stamps the standard
// claims and a fresh random jti, then signs. ttl must be positive: a
// non-positive ttl mints a token that is already dead, which the happy path
// cannot detect.
func (is *Issuer) Issue(ctx context.Context, c Claims, ttl time.Duration) (string, error) {
	return is.issuedAt(ctx, c, ttl, now())
}

// issuedAt is Issue with an injectable clock, the seam that makes expiry
// deterministic under test.
func (is *Issuer) issuedAt(ctx context.Context, c Claims, ttl time.Duration, t time.Time) (string, error) {
	if ttl <= 0 {
		return "", ErrBadExpiry
	}
	if c.Subject == "" {
		return "", ErrEmptySubject
	}
	if c.Audience == "" {
		// An empty audience is fail-open: a verifier that forgets ExpectedAud would
		// accept it. Reject at mint time (mirrored on the verify side).
		return "", ErrEmptyAudience
	}

	jc := jwtClaims{
		// Always the issuer's own identity: a per-token override would let a bug or
		// a compromised caller mint tokens attributed to another issuer that shares
		// the key, defeating the "issuer binds the token to this server" rationale.
		Issuer:   is.issuer,
		Subject:  c.Subject,
		Audience: c.Audience,
		Issued:   t.Unix(),
		Expires:  t.Add(ttl).Unix(),
		// A fresh random jti every call: two tokens for the same job never
		// collide, which matters when a run is re-issued and an old token is
		// not yet stale.
		JTI: uuid.NewString(),
		// A "job:<run>:<job>" subject already carries run/job; surface them in
		// fields too so a consumer that has the subject still gets the parts.
		Job: jobOfSubject(c.Subject),
		Run: runOfSubject(c.Subject),
		// Caller-supplied context claims pass straight through.
		Branch: c.Branch,
		Repo:   c.Repo,
		SHA:    c.SHA,
	}

	hdr := jwtHeader{
		Alg: string(is.signer.Alg()),
		Typ: "JWT",
		Kid: is.signer.Public().Kid,
	}
	cd, err := serialize(hdr, jc)
	if err != nil {
		return "", err
	}
	signingInput := cd.Header + "." + cd.Payload
	sig, err := is.signer.Sign([]byte(signingInput))
	if err != nil {
		return "", err
	}
	_ = ctx // reserved: a rate-limited or audited Issue would record here
	return signingInput + "." + base64URL.EncodeToString(sig), nil
}

// jobOfSubject / runOfSubject re-derive the job/run from a "job:<run>:<job>"
// subject when a caller did not fill the convenience fields. A malformed subject
// leaves them empty, which is fine: a verifier keys off `sub`, not these.
func jobOfSubject(sub string) (job string) {
	_, job, ok := parseSubject(sub)
	if !ok {
		return ""
	}
	return job
}

func runOfSubject(sub string) (run string) {
	run, _, ok := parseSubject(sub)
	if !ok {
		return ""
	}
	return run
}

// Verifier checks tokens against a published key set and the server's identity.
// It is the half a consumer runs: AWS runs an equivalent check, and CICaDo's own
// callers use it before trusting a token they did not just mint.
type Verifier struct {
	issuer string
	keys   *KeySet
	clock  func() time.Time
}

// NewVerifier builds a verifier over the given key set, expecting tokens to
// originate from issuerURL.
func NewVerifier(issuerURL string, keys *KeySet) (*Verifier, error) {
	// Mirror NewIssuer: an empty issuer would disable the issuer check, so any
	// token from any server (or hand-crafted) could redeem here.
	if issuerURL == "" {
		return nil, errors.New("oidc: issuer url is required for verifier")
	}
	if keys == nil {
		return nil, errors.New("oidc: nil key set")
	}
	return &Verifier{issuer: issuerURL, keys: keys, clock: now}, nil
}

// VerifyOptions narrow a verification. ExpectedAud / ExpectedIss, when set, must
// equal the token's `aud` / `iss`; Clock overrides the time used for exp/iat (a
// test seam).
type VerifyOptions struct {
	ExpectedAud string
	ExpectedIss string
	Clock       func() time.Time
}

// Verify confirms token's signature and its time, audience, and issuer claims,
// and returns the decoded claims. Any non-nil error means "do not honour this
// token"; callers need not branch on which one.
func (v *Verifier) Verify(ctx context.Context, token string, opts VerifyOptions) (Claims, error) {
	clock := opts.Clock
	if clock == nil {
		clock = v.clock
	}
	hdr, jc, signingIn, err := decode(token)
	if err != nil {
		return Claims{}, err
	}
	if hdr.Alg != string(AlgoES256) {
		// Recognizing only ES256 is what closes the alg:none downgrade attack.
		return Claims{}, ErrAlgorithmMismatch
	}
	// decode already rejects an empty kid as ErrMalformedToken, so the key is
	// present here; selecting it is a straight lookup.
	jwk, ok := v.keys.Get(hdr.Kid)
	if !ok {
		return Claims{}, ErrUnknownKID
	}
	if err := verifyES256(jwk, signingIn, token); err != nil {
		return Claims{}, ErrBadSignature
	}

	t := clock().UTC()
	// A missing exp (0) is a hard error too: AWS rejects tokens without it, and a
	// key holder could otherwise mint an immortal token.
	if jc.Expires == 0 || t.Unix() >= jc.Expires {
		return Claims{}, ErrExpiredToken
	}
	if jc.Issued != 0 && jc.Issued > t.Unix() {
		return Claims{}, ErrNotYetValid
	}
	if iss := opts.ExpectedIss; iss != "" && jc.Issuer != iss {
		return Claims{}, ErrIssuerMismatch
	}
	// Fail closed on audience, like the mint side: a token minted with no aud is
	// rejected by every verifier, so a forgotten ExpectedAud cannot accept it.
	if jc.Audience == "" {
		return Claims{}, ErrEmptyAudience
	}
	if aud := opts.ExpectedAud; aud != "" && jc.Audience != aud {
		return Claims{}, ErrAudienceMismatch
	}
	if v.issuer != "" && jc.Issuer != v.issuer {
		return Claims{}, ErrIssuerMismatch
	}

	claims := Claims{
		Subject:  jc.Subject,
		Audience: jc.Audience,
		Job:      jc.Job,
		Run:      jc.Run,
		Branch:   jc.Branch,
		Repo:     jc.Repo,
		SHA:      jc.SHA,
	}
	_ = ctx // reserved for a future audited / rate-limited verify
	return claims, nil
}

// verifyES256 checks a token's signature against a P-256 JWK. The signature is
// base64url P-1363 r||s, exactly 64 bytes; we recover r/s and verify.
func verifyES256(jwk JWK, signingInput, token string) error {
	pub, err := publicKeyFromJWK(jwk)
	if err != nil {
		return err
	}
	// A compact JWT's signature never contains '.', so splitting into three is
	// exact.
	seg := strings.SplitN(token, ".", 3)
	if len(seg) != 3 {
		return errors.New("oidc: bad signature segment count")
	}
	sig, err := base64URL.DecodeString(seg[2])
	if err != nil {
		return err
	}
	if len(sig) != 64 {
		return errors.New("oidc: bad signature length")
	}
	r := new(big.Int).SetBytes(sig[0:32])
	s := new(big.Int).SetBytes(sig[32:64])
	digest := sha256.Sum256([]byte(signingInput))
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return errors.New("oidc: signature verify failed")
	}
	return nil
}
