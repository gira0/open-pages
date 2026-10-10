package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signingInput returns "header.payload" for the given header and claims.
func signingInput(t *testing.T, hdr, claims any) string {
	t.Helper()
	h, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return b64(h) + "." + b64(c)
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims any) string {
	t.Helper()
	input := signingInput(t, map[string]string{"alg": "RS256", "kid": kid}, claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(sig)
}

func signES256(t *testing.T, key *ecdsa.PrivateKey, claims any) string {
	t.Helper()
	input := signingInput(t, map[string]string{"alg": "ES256"}, claims)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return input + "." + b64(sig)
}

func TestRS256RoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := signRS256(t, key, "k1", map[string]any{"sub": "u1", "exp": 1700000000})
	tok, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Alg != "RS256" || tok.Kid != "k1" {
		t.Fatalf("header = %q/%q", tok.Alg, tok.Kid)
	}
	jwk := Key{Kty: "RSA", N: b64(key.N.Bytes()), E: b64(big.NewInt(int64(key.E)).Bytes())}
	pub, err := jwk.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Verify(pub); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	claims, err := tok.Claims()
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "u1" {
		t.Fatalf("sub = %v", claims["sub"])
	}
	if _, ok := claims["exp"].(json.Number); !ok {
		t.Fatalf("exp is %T, want json.Number", claims["exp"])
	}

	// A different key must not verify, and neither must a key of the wrong type.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Verify(&other.PublicKey); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	if err := tok.Verify(&ecdsa.PublicKey{Curve: elliptic.P256()}); !errors.Is(err, ErrKey) {
		t.Fatalf("wrong key type: %v", err)
	}
}

func TestES256RoundTrip(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Parse(signES256(t, key, map[string]string{"sub": "u2"}))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		t.Fatal(err)
	}
	pub, err := Key{Kty: "EC", Crv: "P-256", X: b64(pt[1:33]), Y: b64(pt[33:])}.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Verify(pub); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Verify(&rsaKey.PublicKey); !errors.Is(err, ErrKey) {
		t.Fatalf("wrong key type: %v", err)
	}

	tok.sig = tok.sig[:63]
	if err := tok.Verify(pub); !errors.Is(err, ErrSignature) {
		t.Fatalf("short signature: %v", err)
	}
	tok.sig = make([]byte, 64)
	if err := tok.Verify(pub); !errors.Is(err, ErrSignature) {
		t.Fatalf("zero signature: %v", err)
	}
}

func TestVerifyRefusesOtherAlgorithms(t *testing.T) {
	for _, alg := range []string{"none", "HS256", "PS256", ""} {
		raw := signingInput(t, map[string]string{"alg": alg}, map[string]string{"sub": "x"}) + "." + b64([]byte("sig"))
		tok, err := Parse(raw)
		if err != nil {
			t.Fatalf("%q: %v", alg, err)
		}
		if err := tok.Verify(nil); !errors.Is(err, ErrAlg) {
			t.Errorf("alg %q: err = %v, want ErrAlg", alg, err)
		}
	}
}

func TestParseMalformed(t *testing.T) {
	good := signingInput(t, map[string]string{"alg": "RS256"}, map[string]string{}) + "." + b64([]byte("s"))
	for name, raw := range map[string]string{
		"empty":       "",
		"two parts":   "a.b",
		"four parts":  good + ".x",
		"bad base64":  "!!!.e30.AA",
		"header json": b64([]byte("not json")) + ".e30.AA",
	} {
		if _, err := Parse(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestClaimsMalformed(t *testing.T) {
	for name, payload := range map[string]string{"not json": "nope", "null": "null", "array": "[1]"} {
		tok := &Token{payload: []byte(payload)}
		if _, err := tok.Claims(); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestKeyPublicKeyRejects(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	e := b64(big.NewInt(int64(rsaKey.E)).Bytes())
	good := b64(make([]byte, 32))
	for name, k := range map[string]Key{
		"unknown type":   {Kty: "oct"},
		"rsa bad n":      {Kty: "RSA", N: "!!!", E: e},
		"rsa empty n":    {Kty: "RSA", N: "", E: e},
		"rsa bad e":      {Kty: "RSA", N: b64(rsaKey.N.Bytes()), E: "!!!"},
		"rsa small":      {Kty: "RSA", N: b64(small.N.Bytes()), E: e},
		"rsa exponent 1": {Kty: "RSA", N: b64(rsaKey.N.Bytes()), E: b64([]byte{1})},
		"ec curve":       {Kty: "EC", Crv: "P-384", X: good, Y: good},
		"ec bad x":       {Kty: "EC", Crv: "P-256", X: "!!!", Y: good},
		"ec short y":     {Kty: "EC", Crv: "P-256", X: good, Y: b64([]byte{1})},
		"ec off curve":   {Kty: "EC", Crv: "P-256", X: good, Y: good},
	} {
		if _, err := k.PublicKey(); err == nil {
			t.Errorf("%s: PublicKey succeeded", name)
		}
	}
}
