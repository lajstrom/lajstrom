package token

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// Lifetime is how long a token is valid (docs/spec/rbac.md §4): exp is iat plus Lifetime.
const Lifetime = keys.TokenLifetime

// Leeway is the clock skew the verification tolerates on exp, nbf and iat.
const Leeway = keys.TokenLeeway

// reloadEvery bounds the reloads of the verification keys: at most one a second, whatever the
// tokens say, so that a flood of made-up kids costs one query a second, and a key rotated or
// dropped on another node counts within a second.
const reloadEvery = time.Second

// ErrInvalidToken means a bearer token was refused: malformed, an unknown kid, a bad signature,
// an algorithm that is not its key's, expired, not yet valid, another audience or issuer, or a
// subject that is not a principal. It does not say which: the reason is logged at debug level,
// and the client gets 401 with the challenge.
var ErrInvalidToken = errors.New("token: invalid bearer token")

// Keys are the token keys: keys.Tokens over the index, or keys.EphemeralTokens.
type Keys interface {
	// TokenSigner returns the key that signs from now on, with its private key.
	TokenSigner(ctx context.Context) (*keys.TokenKey, error)
	// TokenVerifiers returns the keys that verify: the active one and those retired within
	// the grace.
	TokenVerifiers(ctx context.Context) ([]*keys.TokenKey, error)
}

// Options configure an Issuer or a Verifier.
type Options struct {
	// Keys are the token keys; required.
	Keys Keys
	// Now is the clock of iat, exp and nbf, and of the reloads; nil is time.Now.
	Now func() time.Time
	// Log gets the reasons of the refusals at debug level, with the kid and never the token;
	// nil is slog.Default().
	Log *slog.Logger
}

func (o Options) defaults() Options {
	if o.Keys == nil {
		panic("token: nil Options.Keys")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return o
}

// Claims are the claims of a token: the registered ones and access.
type Claims struct {
	jwt.RegisteredClaims
	// Access is what the token grants, sorted by type and name.
	Access []Access `json:"access"`
}

// Token is a signed token. It prints as its kid and times, never as the JWT.
type Token struct {
	// Raw is the JWT, the bearer of the client; never logged.
	Raw string
	// KeyID is the kid: the fingerprint of the key that signed it.
	KeyID string
	// IssuedAt is iat, to the second; ExpiresAt is exp, Lifetime later.
	IssuedAt, ExpiresAt time.Time
}

// String describes t without the JWT.
func (t *Token) String() string {
	return fmt.Sprintf("token kid=%s iat=%s", t.KeyID, t.IssuedAt.Format(time.RFC3339))
}

// GoString returns the same for %#v.
func (t *Token) GoString() string { return t.String() }

// LogValue logs the kid and iat only.
func (t *Token) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kid", t.KeyID), slog.Time("iat", t.IssuedAt))
}

// Issuer signs tokens. Create it with NewIssuer; it may be used concurrently.
type Issuer struct {
	keys Keys
	now  func() time.Time
}

// NewIssuer returns an Issuer over o.Keys; it panics when o.Keys is nil.
func NewIssuer(o Options) *Issuer {
	o = o.defaults()
	return &Issuer{keys: o.Keys, now: o.Now}
}

// Issue returns a token for p with access, for service: header alg the key's, kid its
// fingerprint, typ JWT; claims iss and aud service, sub p's canonical form, iat and nbf now (to
// the second), exp Lifetime later, jti 16 random bytes in base64url, and access sorted by type
// and name. The key is the active one, read for every token (Keys.TokenSigner).
func (i *Issuer) Issue(ctx context.Context, p auth.Principal, service string, access []Access) (*Token, error) {
	k, err := i.keys.TokenSigner(ctx)
	if err != nil {
		return nil, err
	}
	method, ok := signingMethods[k.Algorithm]
	if !ok {
		return nil, fmt.Errorf("token: %s has the algorithm %q, which signs no token", k, k.Algorithm)
	}
	iat := i.now().UTC().Truncate(time.Second)
	exp := iat.Add(Lifetime)
	jti := make([]byte, 16)
	_, _ = rand.Read(jti) // crypto/rand.Read never returns an error; it crashes the program instead
	sorted := append([]Access{}, access...)
	slices.SortFunc(sorted, compareAccess)
	t := jwt.NewWithClaims(method, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    service,
			Subject:   p.String(),
			Audience:  jwt.ClaimStrings{service},
			ExpiresAt: jwt.NewNumericDate(exp),
			NotBefore: jwt.NewNumericDate(iat),
			IssuedAt:  jwt.NewNumericDate(iat),
			ID:        base64.RawURLEncoding.EncodeToString(jti),
		},
		Access: sorted,
	})
	t.Header["kid"] = k.ID()
	raw, err := t.SignedString(k.Private)
	if err != nil {
		return nil, fmt.Errorf("token: sign with %s: %w", k, err)
	}
	return &Token{Raw: raw, KeyID: k.ID(), IssuedAt: iat, ExpiresAt: exp}, nil
}

// signingMethods are the methods of the key algorithms.
var signingMethods = map[string]jwt.SigningMethod{
	keys.TokenES256: jwt.SigningMethodES256,
	keys.TokenEdDSA: jwt.SigningMethodEdDSA,
}

// Grant is what a valid token says: who carries it and what it grants.
type Grant struct {
	// Principal is the principal of sub.
	Principal auth.Principal
	// Access is the access claim.
	Access []Access
}

// Repository returns the access entry of the repository name, and false when the token grants
// nothing on it.
func (g *Grant) Repository(name string) (Access, bool) {
	for _, a := range g.Access {
		if a.Type == TypeRepository && a.Name == name {
			return a, true
		}
	}
	return Access{}, false
}

// Allows reports whether the token grants action (pull, push, delete) on the repository name.
func (g *Grant) Allows(name, action string) bool {
	a, ok := g.Repository(name)
	return ok && slices.Contains(a.Actions, action)
}

// Catalog reports whether the token grants registry:catalog:*.
func (g *Grant) Catalog() bool {
	return slices.ContainsFunc(g.Access, func(a Access) bool { return a.Type == TypeRegistry && a.Name == catalogName })
}

// Verifier checks tokens. Create it with NewVerifier; it may be used concurrently. It keeps
// the verification keys in memory, by kid, and reloads them (Keys.TokenVerifiers) when they
// are older than a second at the time of a verification: a kid it lacks costs at most one
// reload a second, and a key rotated or dropped elsewhere counts within a second.
type Verifier struct {
	keys Keys
	now  func() time.Time
	log  *slog.Logger

	mu     sync.Mutex
	byID   map[string]*keys.TokenKey // nil until the first load
	loaded time.Time                 // the last load, or the last attempt
}

// NewVerifier returns a Verifier over o.Keys; it panics when o.Keys is nil.
func NewVerifier(o Options) *Verifier {
	o = o.defaults()
	return &Verifier{keys: o.Keys, now: o.Now, log: o.Log}
}

// Verify checks raw, a token for service, and returns what it grants. The key is looked up by
// the kid of the header first, and the token must then be signed with that key's algorithm and
// nothing else (never none, never HMAC); exp is required, iat must not be in the future, iss
// and aud must be service, with Leeway on the times; sub must be a principal. Every refusal is
// an error wrapping ErrInvalidToken, its reason logged at debug level; any other error is a
// failure to load the keys, which the caller answers 500.
func (v *Verifier) Verify(ctx context.Context, raw, service string) (*Grant, error) {
	kid, err := kidOf(raw)
	if err != nil {
		return nil, v.refuse(ctx, "", err.Error())
	}
	k, err := v.key(ctx, kid)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, v.refuse(ctx, kid, "no key has this kid")
	}
	var c Claims
	_, err = jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return k.Public, nil },
		jwt.WithValidMethods([]string{k.Algorithm}),
		jwt.WithAudience(service), jwt.WithIssuer(service),
		jwt.WithLeeway(Leeway), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.now))
	if err != nil {
		return nil, v.refuse(ctx, kid, err.Error())
	}
	p, err := auth.ParsePrincipal(c.Subject)
	if err != nil {
		return nil, v.refuse(ctx, kid, "the subject is not a principal")
	}
	return &Grant{Principal: p, Access: c.Access}, nil
}

// refuse logs the reason of a refusal and returns ErrInvalidToken.
func (v *Verifier) refuse(ctx context.Context, kid, reason string) error {
	v.log.DebugContext(ctx, "bearer token refused", "kid", kid, "reason", reason)
	return ErrInvalidToken
}

// kidOf returns the kid of the header of raw, which is not verified.
func kidOf(raw string) (string, error) {
	t, _, err := jwt.NewParser().ParseUnverified(raw, &Claims{})
	if err != nil {
		return "", err
	}
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return "", errors.New("the header has no kid")
	}
	return kid, nil
}

// key returns the verification key of kid, nil when there is none, reloading the keys when
// the last load is a second old or more. A reload that fails is logged, and the keys of the
// last load answer until the next; before any load succeeded it is an error.
func (v *Verifier) key(ctx context.Context, kid string) (*keys.TokenKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if age := now.Sub(v.loaded); v.byID != nil && age >= 0 && age < reloadEvery {
		return v.byID[kid], nil
	}
	v.loaded = now
	ks, err := v.keys.TokenVerifiers(ctx)
	switch {
	case err != nil && v.byID == nil:
		return nil, fmt.Errorf("token: the verification keys: %w", err)
	case err != nil:
		v.log.WarnContext(ctx, "token verification keys not reloaded; the last ones answer", "error", err.Error())
		return v.byID[kid], nil
	}
	byID := make(map[string]*keys.TokenKey, len(ks))
	for _, k := range ks {
		byID[k.ID()] = k
	}
	v.byID = byID
	return byID[kid], nil
}

// compareAccess orders access entries by type and name.
func compareAccess(a, b Access) int {
	if c := strings.Compare(a.Type, b.Type); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}
