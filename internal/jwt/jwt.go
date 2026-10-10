// Package jwt does minimal JWS verification for OIDC ID tokens: RS256 and ES256 only,
// against keys from the provider's JWKS. Every other algorithm, "none" included, is refused.
package jwt

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Errors returned by this package. Their text is fixed and safe to log.
var (
	ErrMalformed = errors.New("malformed token")
	ErrAlg       = errors.New("unsupported token algorithm")
	ErrKey       = errors.New("no matching signing key")
	ErrSignature = errors.New("invalid token signature")
)

const minRSABits = 2048

// Key is one key of a JWKS document.
type Key struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// PublicKey converts the JWK to a *rsa.PublicKey or *ecdsa.PublicKey.
func (k Key) PublicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64Int(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64Int(k.E)
		if err != nil {
			return nil, err
		}
		if n.BitLen() < minRSABits || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return nil, errors.New("unacceptable RSA key")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, errors.New("unsupported curve")
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != 32 {
			return nil, errors.New("bad EC key")
		}
		y, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil || len(y) != 32 {
			return nil, errors.New("bad EC key")
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append([]byte{4}, append(x, y...)...))
		if err != nil {
			return nil, errors.New("bad EC key")
		}
		return pub, nil
	}
	return nil, errors.New("unsupported key type")
}

func b64Int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad key parameter")
	}
	return new(big.Int).SetBytes(b), nil
}

// Token is a parsed compact JWS that has not been verified yet.
type Token struct {
	// Alg and Kid come from the unverified JOSE header.
	Alg, Kid string

	payload      []byte
	signingInput []byte
	sig          []byte
}

// Parse splits a compact JWS into its header and parts. It checks the structure only;
// call Verify before trusting Claims.
func Parse(raw string) (*Token, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	h, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	payload, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(h, &hdr) != nil {
		return nil, ErrMalformed
	}
	return &Token{Alg: hdr.Alg, Kid: hdr.Kid, payload: payload, signingInput: []byte(parts[0] + "." + parts[1]), sig: sig}, nil
}

// Verify checks the token's signature with key, using the algorithm named in its header.
// Only RS256 (with an *rsa.PublicKey) and ES256 (with an *ecdsa.PublicKey) are accepted.
func (t *Token) Verify(key crypto.PublicKey) error {
	return verifySignature(t.Alg, key, t.signingInput, t.sig)
}

// verifySignature checks sig over signingInput with key for the named algorithm.
func verifySignature(alg string, key crypto.PublicKey, signingInput, sig []byte) error {
	digest := sha256.Sum256(signingInput)
	switch alg {
	case "RS256":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return ErrKey
		}
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) != nil {
			return ErrSignature
		}
		return nil
	case "ES256":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return ErrKey
		}
		if len(sig) != 64 {
			return ErrSignature
		}
		der, err := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
		if err != nil {
			return fmt.Errorf("encode signature: %w", err)
		}
		if !ecdsa.VerifyASN1(pub, digest[:], der) {
			return ErrSignature
		}
		return nil
	}
	return ErrAlg
}

// Claims parses the payload, keeping numbers exact (as json.Number). Only trust the result
// after Verify succeeded.
func (t *Token) Claims() (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(t.payload))
	dec.UseNumber()
	var claims map[string]any
	if err := dec.Decode(&claims); err != nil || claims == nil {
		return nil, ErrMalformed
	}
	return claims, nil
}
