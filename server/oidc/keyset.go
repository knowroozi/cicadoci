package oidc

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"math/big"
)

// KeySet is the set of public keys a verifier trusts, in the shape a JWKS
// (RFC 7517) publishes. An issuer publishes its key(s) so a consumer can verify
// without any out-of-band channel; on rotation a newer set is published and both
// old and new keys coexist until the old keys' tokens have all expired.
type KeySet struct {
	byID map[string]JWK
}

// NewKeySet builds an empty key set.
func NewKeySet() *KeySet {
	return &KeySet{byID: make(map[string]JWK)}
}

// Add inserts a key's public JWK, indexed by its id. A key without an id is
// rejected: a key a verifier cannot address by kid is one it cannot select, so
// it is useless to the set.
func (ks *KeySet) Add(key *Key) error {
	jwk := key.JWK()
	if jwk.Kid == "" {
		return errors.New("oidc: key without id cannot be added")
	}
	ks.byID[jwk.Kid] = jwk
	return nil
}

// Get returns the public key for kid.
func (ks *KeySet) Get(kid string) (JWK, bool) {
	jwk, ok := ks.byID[kid]
	return jwk, ok
}

// Keys returns the set's JWKs. Order is unspecified (map range); consumers match
// by kid, never by position.
func (ks *KeySet) Keys() []JWK {
	out := make([]JWK, 0, len(ks.byID))
	for _, jwk := range ks.byID {
		out = append(out, jwk)
	}
	return out
}

// jwksDoc is the RFC 7517 wire shape: a small "keys" array.
type jwksDoc struct {
	Keys []JWK `json:"keys"`
}

// Marshal renders the set as a JWKS document — the body a /well-known/jwks.json
// style endpoint serves.
func (ks *KeySet) Marshal() ([]byte, error) {
	return json.Marshal(jwksDoc{Keys: ks.Keys()})
}

// LoadJWKS parses a JWKS document into a key set. It accepts only P-256 EC keys:
// anything else would verify nothing this server mints, so a key is admitted only
// if it reconstructs on P-256, and a document that yields no usable key is a
// malformed one.
func LoadJWKS(doc []byte) (*KeySet, error) {
	var in jwksDoc
	if err := json.Unmarshal(doc, &in); err != nil {
		return nil, err
	}
	ks := &KeySet{byID: make(map[string]JWK)}
	for _, jwk := range in.Keys {
		if _, err := publicKeyFromJWK(jwk); err != nil {
			// A key we cannot reconstruct on P-256 verifies nothing we mint;
			// skip it. The empty-keyset case below is the one hard error.
			continue
		}
		if jwk.Kid != "" {
			ks.byID[jwk.Kid] = jwk
		}
	}
	if len(ks.byID) == 0 {
		return nil, ErrMalformedJWKS
	}
	return ks, nil
}

// publicKeyFromJWK reconstructs a P-256 public key from a JWK. It is the inverse
// of jwkOfP256 and the heart of verification: a verifier never sees the private
// half, only this.
//
// The security-critical on-curve check uses ecdh.NewPublicKey, the only
// undeprecated route: it rejects off-curve and non-canonical points, closing the
// forgery vector, while elliptic.IsOnCurve (the old check) is deprecated. The
// point is then handed to ecdsa.Verify; building the ecdsa struct touches the
// deprecated coordinate fields — the one exemption a dependency-free leaf package
// needs. See docs/decisions/CIC-39-oidc-issuer.md.
func publicKeyFromJWK(jwk JWK) (*ecdsa.PublicKey, error) {
	if jwk.Kty != curveJWKType || jwk.Crv != curveNameJWK {
		return nil, ErrMalformedJWKS
	}
	x, err := base64URL.DecodeString(jwk.X)
	if err != nil {
		return nil, ErrMalformedJWKS
	}
	y, err := base64URL.DecodeString(jwk.Y)
	if err != nil {
		return nil, ErrMalformedJWKS
	}
	// P-256 coordinates are 32 bytes each; a longer or shorter encoding is
	// malformed.
	if len(x) != 32 || len(y) != 32 {
		return nil, ErrMalformedJWKS
	}
	// 0x04 || X || Y is the SECG uncompressed point, 65 bytes for P-256.
	uncompressed := make([]byte, 0, 65)
	uncompressed = append(uncompressed, 0x04)
	uncompressed = append(uncompressed, x...)
	uncompressed = append(uncompressed, y...)
	// ecdh validates on-curve and canonical form without a deprecated call, the
	// undeprecated replacement for the old elliptic.IsOnCurve.
	if _, err := ecdh.P256().NewPublicKey(uncompressed); err != nil {
		return nil, ErrMalformedJWKS
	}
	// ecdsa.Verify needs coordinate big.Ints; the composite literal is the one
	// place a coordinate write is unavoidable (no coordinate-free ecdsa pubkey
	// exists).
	xi := new(big.Int).SetBytes(x)
	yi := new(big.Int).SetBytes(y)
	pub := &ecdsa.PublicKey{
		Curve: curveParams,
		X:     xi, //nolint:staticcheck // SA1019: no coord-free ecdsa pubkey; see docs
		Y:     yi, //nolint:staticcheck // SA1019: no coord-free ecdsa pubkey; see docs
	}
	return pub, nil
}
