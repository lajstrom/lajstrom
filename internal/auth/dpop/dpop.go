// Package dpop checks the DPoP proofs (RFC 9449) of a web UI's sign-in (docs/spec/rbac.md §10,
// docs/spec/http-routing.md §4): the proof of POST /api/v1/login, whose key the token is bound
// to, and the proof that comes with that token on every request to /api/. Verify checks a proof
// against its request; Spend makes sure it is used once, on any replica, through the index.
package dpop

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Algorithm is the one JWS algorithm of a proof that Lajstrom takes, ECDSA P-256, which every
// browser makes non-extractable with WebCrypto; the algs of a DPoP challenge (RFC 9449 §7.1).
const Algorithm = "ES256"

// Window is how far the iat of a proof may be from the server's clock, before or after (RFC 9449
// §11.1). A proof's jti is kept as long: until iat and Window.
const Window = 5 * time.Minute

// Bounds of a proof, in bytes: the header DPoP (a proof of a P-256 key is about 600) and its jti.
const (
	MaxProofLen = 4096
	maxJTILen   = 256
)

// sweepEvery is how often at most a Verifier deletes the proofs that expired.
const sweepEvery = time.Minute

// typ is the typ of a proof's JOSE header (RFC 9449 §4.2).
const typ = "dpop+jwt"

// ErrInvalidProof means the DPoP proof of a request is missing, malformed, or does not hold for
// the request or its token, or was used already. An error that wraps it says why in words for
// the error_description of a challenge (Why).
var ErrInvalidProof = errors.New("dpop: the proof is refused")

// proofError is an error wrapping ErrInvalidProof; why is fixed text, never the client's.
type proofError struct{ why string }

func (e *proofError) Error() string        { return ErrInvalidProof.Error() + ": " + e.why }
func (e *proofError) Is(target error) bool { return target == ErrInvalidProof }

// refuse returns the error of a proof refused for why.
func refuse(why string) error { return &proofError{why: why} }

// Why returns why err refused a proof, for the error_description of a challenge and the message
// of an answer; "" when err does not wrap ErrInvalidProof. It is fixed text: it holds nothing of
// the proof, so it needs no escaping in a quoted string.
func Why(err error) string {
	var pe *proofError
	if errors.As(err, &pe) {
		return pe.why
	}
	return ""
}

// Proof is a proof that Verify accepted.
type Proof struct {
	// JKT is the JWK SHA-256 thumbprint of the proof's key (RFC 7638), base64url without
	// padding: the cnf.jkt of a token bound to it (RFC 9449 §6.1), tokens.dpop_jkt.
	JKT string
	// JTI is the proof's jti, IssuedAt its iat.
	JTI      string
	IssuedAt time.Time
}

// Replays keeps the proofs used (index.Store, docs/spec/index-schema.md, dpop_proofs).
type Replays interface {
	// UseDPoPProof stores key until expiresAt; a key stored that expires at now or later wraps
	// index.ErrConflict, and nothing changes.
	UseDPoPProof(ctx context.Context, key string, expiresAt, now time.Time) error
	// ExpireDPoPProofs deletes the keys that expired before before.
	ExpireDPoPProofs(ctx context.Context, before time.Time) (int, error)
}

// Verifier checks the DPoP proofs of requests. Create it as a struct literal and use it by
// pointer; Replays is required. Its methods may be called concurrently.
type Verifier struct {
	// ExternalURL is server.externalURL, the scheme and host of the htu of every proof: the URL
	// the browser sees. Empty, as server.insecureHTTP allows, they are the request's: its Host,
	// over https when the request came over TLS.
	ExternalURL string
	// Replays keeps the proofs used, for every replica; required.
	Replays Replays
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log gets the failures to delete the expired proofs, at debug; nil is slog.Default().
	Log *slog.Logger

	// swept is when the expired proofs were last deleted, in Unix nanoseconds.
	swept atomic.Int64
}

// Verify checks the DPoP proof of r (RFC 9449 §4.3) and returns it: one header DPoP of at most
// MaxProofLen bytes; a JWT whose header has typ dpop+jwt, alg ES256 and jwk, a public P-256
// key, and whose signature verifies with that key; htm the method of r; htu its URI without the
// query (target); iat within Window of the clock; a jti of 1 to 256 bytes; and, when token is
// not "", ath the base64url SHA-256 of token. A nonce claim is ignored: Lajstrom gives none
// (RFC 9449 §9). It does not record the jti: Spend does, once the request is otherwise
// accepted. Every refusal wraps ErrInvalidProof.
func (v *Verifier) Verify(r *http.Request, token string) (*Proof, error) {
	values := r.Header.Values("DPoP")
	switch {
	case len(values) == 0:
		return nil, refuse("the request has no DPoP header")
	case len(values) > 1:
		return nil, refuse("the request has more than one DPoP header")
	case len(values[0]) > MaxProofLen:
		return nil, refuse(fmt.Sprintf("the proof is longer than %d bytes", MaxProofLen))
	}
	var (
		c   claims
		jkt string
	)
	_, err := jwt.NewParser(
		jwt.WithValidMethods([]string{Algorithm}),
		jwt.WithStrictDecoding(),
		jwt.WithoutClaimsValidation(),
	).ParseWithClaims(values[0], &c, func(t *jwt.Token) (any, error) {
		if got, _ := t.Header["typ"].(string); got != typ {
			return nil, refuse("the proof's typ is not dpop+jwt")
		}
		key, thumbprint, err := publicKey(t.Header["jwk"])
		if err != nil {
			return nil, err
		}
		jkt = thumbprint
		return key, nil
	})
	if err != nil {
		if why := Why(err); why != "" {
			return nil, refuse(why)
		}
		switch {
		case errors.Is(err, jwt.ErrECDSAVerification):
			return nil, refuse("the proof's signature does not verify with its jwk")
		case errors.Is(err, jwt.ErrTokenSignatureInvalid), errors.Is(err, jwt.ErrTokenUnverifiable):
			return nil, refuse("the proof's alg is not ES256")
		}
		return nil, refuse("the proof is not a JWT")
	}
	return v.check(r, token, &c, jkt)
}

// claims are the claims of a proof that Verify reads.
type claims struct {
	jwt.RegisteredClaims        // jti, iat
	HTM                  string `json:"htm"`
	HTU                  string `json:"htu"`
	ATH                  string `json:"ath"`
}

// check checks the claims c of the proof of r, whose key has the thumbprint jkt.
func (v *Verifier) check(r *http.Request, token string, c *claims, jkt string) (*Proof, error) {
	now := v.now()
	want, ok := Normalize(target(v.ExternalURL, r))
	switch {
	case c.ID == "" || c.HTM == "" || c.HTU == "" || c.IssuedAt == nil:
		return nil, refuse("the proof lacks one of jti, htm, htu and iat")
	case len(c.ID) > maxJTILen:
		return nil, refuse(fmt.Sprintf("the proof's jti is longer than %d bytes", maxJTILen))
	case c.HTM != r.Method:
		return nil, refuse("the proof's htm is not the method of the request")
	case !ok:
		return nil, errors.New("dpop: the URI of the request does not parse") // a Host the server accepted
	}
	if got, ok := Normalize(c.HTU); !ok || got != want {
		return nil, refuse("the proof's htu is not the URI of the request")
	}
	if d := now.Sub(c.IssuedAt.Time); d > Window || d < -Window {
		return nil, refuse("the proof's iat is not within 5 minutes of the server's clock")
	}
	if token != "" && subtle.ConstantTimeCompare([]byte(c.ATH), []byte(TokenHash(token))) != 1 {
		return nil, refuse("the proof's ath is not the hash of the token")
	}
	return &Proof{JKT: jkt, JTI: c.ID, IssuedAt: c.IssuedAt.Time}, nil
}

// Spend records p as used, until its iat and Window, for every replica (Replays): a proof used
// already, here or on another replica, is an error wrapping ErrInvalidProof. Any other error is
// a failure of the index. At most once every sweepEvery, it deletes the proofs that expired
// first; a failure of that is logged at debug and does not refuse p.
func (v *Verifier) Spend(ctx context.Context, p *Proof) error {
	now := v.now()
	if last := v.swept.Load(); now.UnixNano()-last >= int64(sweepEvery) && v.swept.CompareAndSwap(last, now.UnixNano()) {
		if _, err := v.Replays.ExpireDPoPProofs(ctx, now); err != nil {
			v.log().DebugContext(ctx, "dpop: the expired proofs are not deleted", "error", err.Error())
		}
	}
	err := v.Replays.UseDPoPProof(ctx, ReplayKey(p.JKT, p.JTI), p.IssuedAt.Add(Window), now)
	switch {
	case errors.Is(err, index.ErrConflict):
		return refuse("the proof was used already")
	case err != nil:
		return fmt.Errorf("dpop: record the proof: %w", err)
	}
	return nil
}

// ReplayKey is what dpop_proofs keeps of a proof: the sha256 hex of its key's thumbprint, a
// space, and its jti, so that the row has a fixed size and two keys never share a jti.
func ReplayKey(jkt, jti string) string {
	sum := sha256.Sum256([]byte(jkt + " " + jti))
	return hex.EncodeToString(sum[:])
}

// TokenHash is the ath of a proof of token: the base64url SHA-256 of its ASCII (RFC 9449 §4.2).
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// target is the URI that the htu of a proof of r names: the scheme and host of externalURL, or
// of r without it, and the path of r as it came, without the query.
func target(externalURL string, r *http.Request) string {
	base := strings.TrimSuffix(externalURL, "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + r.URL.EscapedPath()
}

// publicKey returns the public key of the jwk of a proof's header and its thumbprint (RFC
// 7638): an EC key on P-256, its x and y 32 bytes of base64url each and a point of the curve,
// without a private member.
func publicKey(v any) (*ecdsa.PublicKey, string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, "", refuse("the proof's header has no jwk")
	}
	for _, private := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
		if _, ok := m[private]; ok {
			return nil, "", refuse("the proof's jwk holds a private key")
		}
	}
	kty, _ := m["kty"].(string)
	crv, _ := m["crv"].(string)
	if kty != "EC" || crv != "P-256" {
		return nil, "", refuse("the proof's jwk is not a P-256 key")
	}
	x, okX := coordinate(m["x"])
	y, okY := coordinate(m["y"])
	if !okX || !okY {
		return nil, "", refuse("the proof's jwk has no x and y of 32 bytes")
	}
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return nil, "", refuse("the proof's jwk is not a point of P-256")
	}
	return key, Thumbprint(x, y), nil
}

// coordinate decodes x or y of a P-256 JWK: 32 bytes of base64url without padding.
func coordinate(v any) ([]byte, bool) {
	s, _ := v.(string)
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return b, err == nil && len(b) == 32
}

// Thumbprint is the JWK SHA-256 thumbprint (RFC 7638 §3) of the P-256 key with the coordinates
// x and y, base64url without padding: the SHA-256 of the required members in lexicographic
// order, without whitespace.
func Thumbprint(x, y []byte) string {
	b, _ := json.Marshal(struct { // fields in the order of RFC 7638; strings of base64url need no escaping
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{"P-256", "EC", base64.RawURLEncoding.EncodeToString(x), base64.RawURLEncoding.EncodeToString(y)})
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidThumbprint reports whether s has the shape of a JWK SHA-256 thumbprint: 43 characters of
// base64url.
func ValidThumbprint(s string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) log() *slog.Logger {
	if v.Log != nil {
		return v.Log
	}
	return slog.Default()
}
