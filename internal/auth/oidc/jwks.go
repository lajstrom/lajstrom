package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// The JWKS cache (docs/spec/rbac.md §8): a kid the set does not have reads it again, at most once
// per keysRefetchEvery, so that a flood of made-up kids costs one request to the IdP per period
// and a rotated key counts within it; the set is read again when it is keysMaxAge old. A read
// that fails keeps the last set.
const (
	keysRefetchEvery = 30 * time.Second
	keysMaxAge       = time.Hour
	// minRSABits is the smallest RSA modulus a key may have.
	minRSABits = 2048
)

// jwk is a JSON Web Key (RFC 7517) as a JWKS has it; only public members are read.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	// RSA (RFC 7518 §6.3.1)
	N string `json:"n"`
	E string `json:"e"`
	// EC (RFC 7518 §6.2.1) and OKP (RFC 8037 §2)
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey is a signing key of the IdP: its public key and the algorithm its JWK pins, "" when
// it pins none.
type publicKey struct {
	pub crypto.PublicKey
	alg string
}

// keySet caches the IdP's signing keys by kid.
type keySet struct {
	c *Client

	mu      sync.Mutex
	keys    map[string]publicKey
	at      time.Time // when keys were read
	tried   time.Time // the last read, whether it succeeded or not
	skipped int       // keys of the last read that were not usable
}

// key returns the key of kid: from the set when it has it and is not keysMaxAge old, else from a
// new read, no sooner than keysRefetchEvery after the last one. A token without a kid takes the
// set's only key. An unknown kid wraps ErrRefused; a set that never loaded wraps ErrUnavailable.
func (s *keySet) key(ctx context.Context, jwksURI, kid string) (publicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.c.cfg.Now()
	k, ok := s.find(kid)
	if ok && now.Sub(s.at) < keysMaxAge {
		return k, nil
	}
	if s.keys == nil || now.Sub(s.tried) >= keysRefetchEvery {
		s.tried = now
		keys, skipped, err := s.read(ctx, jwksURI)
		switch {
		case err == nil:
			s.keys, s.at, s.skipped = keys, now, skipped
			s.c.cfg.Log.DebugContext(ctx, "oidc: signing keys loaded", "keys", len(keys), "skipped", skipped)
		case s.keys == nil:
			return publicKey{}, err
		default:
			s.c.cfg.Log.WarnContext(ctx, "oidc: the signing keys did not load again; the last ones stay", "error", err.Error())
		}
		k, ok = s.find(kid)
	}
	if !ok {
		return publicKey{}, fmt.Errorf("%w: no signing key of kid %q in the IdP's JWKS", ErrRefused, kid)
	}
	return k, nil
}

// find returns the key of kid, or the only key for kid "".
func (s *keySet) find(kid string) (publicKey, bool) {
	if kid == "" && len(s.keys) == 1 {
		for _, k := range s.keys {
			return k, true
		}
	}
	k, ok := s.keys[kid]
	return k, ok
}

// read reads the JWKS and returns its signing keys by kid and how many keys it skipped: an
// encryption key, a kty or curve Lajstrom does not verify with, a malformed or weak one.
func (s *keySet) read(ctx context.Context, jwksURI string) (map[string]publicKey, int, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := s.c.getJSON(ctx, jwksURI, &set); err != nil {
		return nil, 0, err
	}
	keys, skipped := map[string]publicKey{}, 0
	for _, j := range set.Keys {
		if j.Use != "" && j.Use != "sig" {
			skipped++
			continue
		}
		pub, err := j.public()
		if err != nil {
			s.c.cfg.Log.DebugContext(ctx, "oidc: a key of the JWKS is skipped", "kid", j.Kid, "reason", err.Error())
			skipped++
			continue
		}
		keys[j.Kid] = publicKey{pub: pub, alg: j.Alg}
	}
	if len(keys) == 0 {
		return nil, skipped, fmt.Errorf("%w: the JWKS at %s has no signing key Lajstrom can use", ErrUnavailable, jwksURI)
	}
	return keys, skipped, nil
}

// public decodes the public key of j: RSA of at least minRSABits, EC on P-256, P-384 or P-521
// (the point checked to be on its curve), or Ed25519.
func (j *jwk) public() (crypto.PublicKey, error) {
	switch j.Kty {
	case "RSA":
		return j.rsa()
	case "EC":
		return j.ec()
	case "OKP":
		x, err := b64(j.X)
		if j.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("an OKP key that is not Ed25519 of %d bytes", ed25519.PublicKeySize)
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("the kty %q", j.Kty)
}

// rsa decodes an RSA key: an odd public exponent of 3 or more that fits an int, and a modulus of
// at least minRSABits.
func (j *jwk) rsa() (*rsa.PublicKey, error) {
	n, err := bigInt(j.N)
	if err != nil {
		return nil, fmt.Errorf("n: %w", err)
	}
	e, err := bigInt(j.E)
	if err != nil {
		return nil, fmt.Errorf("e: %w", err)
	}
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
		return nil, errors.New("e is not an odd exponent of 3 or more")
	}
	if n.BitLen() < minRSABits {
		return nil, fmt.Errorf("an RSA key of %d bits, less than %d", n.BitLen(), minRSABits)
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// curves are the curves of the EC keys Lajstrom verifies with, by their JWK name.
var curves = map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384(), "P-521": elliptic.P521()}

// ec decodes an EC key: x and y of the curve's size, a point on the curve.
func (j *jwk) ec() (*ecdsa.PublicKey, error) {
	curve, ok := curves[j.Crv]
	if !ok {
		return nil, fmt.Errorf("the curve %q", j.Crv)
	}
	size := (curve.Params().BitSize + 7) / 8
	x, errX := b64(j.X)
	y, errY := b64(j.Y)
	if errX != nil || errY != nil || len(x) != size || len(y) != size {
		return nil, fmt.Errorf("x and y are not %d bytes each", size)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
	if err != nil {
		return nil, fmt.Errorf("the point: %w", err)
	}
	return pub, nil
}

// b64 decodes base64url without padding (RFC 7515 §2).
func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// bigInt decodes an unsigned big-endian integer in base64url.
func bigInt(s string) (*big.Int, error) {
	b, err := b64(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("not base64url")
	}
	return new(big.Int).SetBytes(b), nil
}
