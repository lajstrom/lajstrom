package keys

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The bearer-token signing key of /v2 (ADR 0010): a server key, kind jwt and no repo, made at
// startup when there is none, its private key sealed with the master key as a repo key is.

const (
	// KindJWT is the kind of the bearer-token signing keys in the keys table; their repo_id is
	// null.
	KindJWT = "jwt"
	// ActionDrop is the audit action of lajstrom admin key drop --token: Subject is the
	// fingerprint of the key retired without a successor.
	ActionDrop = "key.drop"

	// TokenES256 makes ECDSA P-256 keys, which sign ES256 tokens: the default of
	// keys.tokenAlgorithm.
	TokenES256 = "ES256"
	// TokenEdDSA makes Ed25519 keys, which sign EdDSA tokens.
	TokenEdDSA = "EdDSA"

	// TokenLifetime is how long a bearer token is valid (docs/spec/rbac.md §4).
	TokenLifetime = 20 * time.Minute
	// TokenLeeway is the clock skew between replicas that the verification of a token
	// tolerates.
	TokenLeeway = 60 * time.Second
	// TokenGrace is how long a retired token key keeps verifying: until every token it signed
	// has expired, the skew included. It is not keys.retireGrace, which is apt's.
	TokenGrace = TokenLifetime + TokenLeeway
)

// ErrNoTokenKey is the error of DropToken when there is no active token key to drop.
var ErrNoTokenKey = errors.New("keys: there is no active token key")

// TokenKey is a key that signs or verifies bearer tokens: its row in keys and the key itself.
// The algorithm is the key's type, so a row needs no column for it. It prints as its
// fingerprint, never with the key material.
type TokenKey struct {
	// Key is the row: Public is the PKIX DER of the public key, PrivateEnc the sealed PKCS #8
	// DER of the private key, Fingerprint the lowercase hexadecimal sha256 of Public, the kid of
	// every token the key signs. The ephemeral key has no row: its ID is 0 and PrivateEnc empty.
	Key index.Key
	// Algorithm is the JOSE algorithm of the key: TokenES256 or TokenEdDSA.
	Algorithm string
	// Public is the public key, an *ecdsa.PublicKey or an ed25519.PublicKey.
	Public crypto.PublicKey
	// Private is the private key, an *ecdsa.PrivateKey or an ed25519.PrivateKey; nil in the
	// keys TokenVerifiers returns, which only verify.
	Private crypto.Signer
}

// ID returns the fingerprint, the kid of the tokens the key signs.
func (k *TokenKey) ID() string { return k.Key.Fingerprint }

// String returns "token key <fingerprint>".
func (k *TokenKey) String() string { return "token key " + k.Key.Fingerprint }

// GoString returns the same for %#v.
func (k *TokenKey) GoString() string { return k.String() }

// LogValue logs the fingerprint only.
func (k *TokenKey) LogValue() slog.Value { return slog.StringValue(k.Key.Fingerprint) }

// verifyOnly returns a copy of k without its private key.
func (k *TokenKey) verifyOnly() *TokenKey {
	c := *k
	c.Private = nil
	return &c
}

// TokenOptions configure Tokens.
type TokenOptions struct {
	// Index holds the keys; RotateToken and DropToken also write their audit rows through it.
	Index index.Store
	// Master seals the private keys at rest; required.
	Master *Master
	// Algorithm is TokenES256 (the default when empty) or TokenEdDSA: keys.tokenAlgorithm. It
	// chooses the type of the keys made from now on; a key made before keeps its own.
	Algorithm string
	// Now is the clock of key creation and of the grace; nil is time.Now.
	Now func() time.Time
	// Log gets one line per key made, rotated and dropped, with fingerprints only, and the
	// warning for a retired key that does not decrypt; nil is slog.Default().
	Log *slog.Logger
}

// Tokens keeps the key that signs the bearer tokens of /v2: one active row of keys with kind
// jwt and repo_id null, made on first use, its private key sealed with the master key (nonce
// || AES-256-GCM, the fingerprint as additional data). A rotation retires the active key; the
// retired key verifies for TokenGrace after that, and a dropped one not at all. Retired rows
// stay in the table: the store has no delete, and a row retired longer than the grace ago is
// ignored. Tokens holds no state between calls: replicas share the key through the index, and
// a rotation by lajstrom admin key on another node counts at the next call.
type Tokens struct {
	st        index.Store
	master    *Master
	algorithm string
	now       func() time.Time
	log       *slog.Logger
}

// NewTokens returns the Tokens of o. A missing Index or Master, or an unknown Algorithm, is an
// error.
func NewTokens(o TokenOptions) (*Tokens, error) {
	t := &Tokens{st: o.Index, master: o.Master, algorithm: cmp.Or(o.Algorithm, TokenES256), now: o.Now, log: o.Log}
	switch {
	case t.st == nil:
		return nil, errors.New("keys: no index")
	case t.master == nil:
		return nil, errors.New("keys: no master key")
	case t.algorithm != TokenES256 && t.algorithm != TokenEdDSA:
		return nil, fmt.Errorf("keys: token algorithm %q is not %s or %s", t.algorithm, TokenES256, TokenEdDSA)
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.log == nil {
		t.log = slog.Default()
	}
	return t, nil
}

// TokenSigner returns the active token key, decrypted, read from the index on every call: a
// token is issued once per client operation, and a rotation on another node counts at once.
// When there is none it is made now, a key of Options.Algorithm; two replicas that make the
// first key at once both insert one, the unique index keys_active keeps one, and the replica
// whose insert fails reads that one. An active key that does not decrypt is an error that
// names its fingerprint and the way out.
func (t *Tokens) TokenSigner(ctx context.Context) (*TokenKey, error) {
	row, err := t.active(ctx)
	if err != nil {
		return nil, err
	}
	k, err := t.open(*row)
	if err != nil {
		return nil, undecryptable(err)
	}
	return k, nil
}

// TokenVerifiers returns the keys that verify tokens, without their private keys: the active
// key, when there is one, and the keys retired less than TokenGrace ago, the most recently
// retired first. It makes no key and reads the index once. Each row is decrypted all the same:
// a row the master key does not open, or whose private key is not the key of its fingerprint
// and public key, was not made by a Lajstrom that holds the master key, and verifies nothing.
// An active key that does not decrypt is an error; a retired one is left out with a warning.
func (t *Tokens) TokenVerifiers(ctx context.Context) ([]*TokenKey, error) {
	all, err := t.st.ListKeys(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("keys: the token keys: %w", err)
	}
	since := t.now().Add(-TokenGrace)
	var active, retired []index.Key
	for _, k := range all {
		switch {
		case k.Kind != KindJWT:
		case k.RetiredAt == nil:
			active = append(active, k)
		case k.RetiredAt.After(since):
			retired = append(retired, k)
		}
	}
	slices.SortFunc(retired, func(a, b index.Key) int {
		return cmp.Or(b.RetiredAt.Compare(*a.RetiredAt), cmp.Compare(b.ID, a.ID))
	})
	out := make([]*TokenKey, 0, len(active)+len(retired))
	for _, row := range active {
		k, err := t.open(row)
		if err != nil {
			return nil, undecryptable(err)
		}
		out = append(out, k.verifyOnly())
	}
	for _, row := range retired {
		k, err := t.open(row)
		if err != nil {
			t.log.WarnContext(ctx, "retired token key left out: it does not decrypt", "fingerprint", row.Fingerprint,
				"error", err.Error())
			continue
		}
		out = append(out, k.verifyOnly())
	}
	return out, nil
}

// RotateToken makes a new token key, which signs from now on, and retires the active one, in
// one transaction that also writes the audit row key.rotate (Repo empty, Subject the new
// fingerprint, Details {"retired": "<the old one>"}). The retired key verifies for TokenGrace.
// With drop it is retired one grace in the past, and so is every key that an earlier rotation
// retired less than TokenGrace ago, so that nothing they signed verifies any more: every
// outstanding token dies, for a key that may have leaked, and Details also has
// {"dropped": true}. A rotation that loses to another at the same time is an error wrapping
// index.ErrConflict.
func (t *Tokens) RotateToken(ctx context.Context, drop bool) (*TokenKey, error) {
	row, key, err := t.generate()
	if err != nil {
		return nil, err
	}
	var retired string
	err = t.st.Tx(ctx, func(tx index.Writer) error {
		retired = ""
		if drop {
			if err := t.endGrace(ctx, tx); err != nil {
				return err
			}
		}
		old, err := tx.GetKey(ctx, nil, KindJWT)
		switch {
		case err == nil:
			if err := tx.RetireKey(ctx, old.ID, t.retireAt(drop)); err != nil {
				return err
			}
			retired = old.Fingerprint
		case !errors.Is(err, index.ErrNotFound):
			return err
		}
		if err := tx.CreateKey(ctx, row); err != nil {
			return err
		}
		return tx.AppendAudit(ctx, t.event(ctx, ActionRotate, row.Fingerprint, retired, drop))
	})
	if err != nil {
		return nil, fmt.Errorf("keys: rotate the token key: %w", err)
	}
	t.log.InfoContext(ctx, "token signing key rotated", "fingerprint", row.Fingerprint, "retired", retired,
		"dropped", drop, "algorithm", key.Algorithm)
	key.Key = *row
	return key, nil
}

// DropToken retires the active token key without making a new one, one grace in the past so
// that nothing it signed verifies, and writes the audit row key.drop (Subject the fingerprint).
// It does not decrypt the key: it is the way out of an active key that does not decrypt (a
// backup restored under another master key), after which the next TokenSigner, at the next
// start of serve or the next token, makes a new key. It returns the fingerprint retired; with
// no active key it is an error wrapping ErrNoTokenKey.
func (t *Tokens) DropToken(ctx context.Context) (string, error) {
	var dropped string
	err := t.st.Tx(ctx, func(tx index.Writer) error {
		old, err := tx.GetKey(ctx, nil, KindJWT)
		if errors.Is(err, index.ErrNotFound) {
			return ErrNoTokenKey
		}
		if err != nil {
			return err
		}
		if err := tx.RetireKey(ctx, old.ID, t.retireAt(true)); err != nil {
			return err
		}
		dropped = old.Fingerprint
		return tx.AppendAudit(ctx, t.event(ctx, ActionDrop, old.Fingerprint, "", false))
	})
	if errors.Is(err, ErrNoTokenKey) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("keys: drop the token key: %w", err)
	}
	t.log.InfoContext(ctx, "token signing key dropped", "fingerprint", dropped)
	return dropped, nil
}

// ExportToken returns the public key of the active token key as a PEM block "PUBLIC KEY"
// (PKIX), for a verifier that is not Lajstrom (ADR 0010, the last consequence). The key is made
// now when there is none, and decrypted first, so that only a key the master key vouches for
// is exported.
func (t *Tokens) ExportToken(ctx context.Context) ([]byte, error) {
	k, err := t.TokenSigner(ctx)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.Key.Public}), nil
}

// endGrace moves the RetiredAt of every token key retired less than TokenGrace ago to one grace
// ago, so that the tokens it signed stop verifying with the rotation that drops.
func (t *Tokens) endGrace(ctx context.Context, tx index.Writer) error {
	all, err := tx.ListKeys(ctx, nil)
	if err != nil {
		return err
	}
	since := t.now().Add(-TokenGrace)
	for _, k := range all {
		if k.Kind == KindJWT && k.RetiredAt != nil && k.RetiredAt.After(since) {
			if err := tx.RetireKey(ctx, k.ID, t.retireAt(true)); err != nil {
				return err
			}
		}
	}
	return nil
}

// retireAt is the RetiredAt of a key retired now: now, or one grace ago when it is dropped.
func (t *Tokens) retireAt(drop bool) time.Time {
	if drop {
		return t.now().Add(-TokenGrace)
	}
	return t.now()
}

// event is the audit row of action on the token key of fingerprint, by the principal of ctx.
func (t *Tokens) event(ctx context.Context, action, fingerprint, retired string, drop bool) *index.AuditEvent {
	d := map[string]any{}
	if retired != "" {
		d["retired"] = retired
	}
	if drop {
		d["dropped"] = true
	}
	var details json.RawMessage
	if len(d) > 0 {
		details, _ = json.Marshal(d) // strings and a bool: never an error
	}
	return &index.AuditEvent{At: t.now(), Principal: auth.PrincipalFrom(ctx).String(), Action: action,
		Subject: fingerprint, Details: details}
}

// active returns the row of the active token key, made and stored when there is none.
func (t *Tokens) active(ctx context.Context) (*index.Key, error) {
	k, err := t.st.GetKey(ctx, nil, KindJWT)
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, index.ErrNotFound) {
		return nil, fmt.Errorf("keys: the token key: %w", err)
	}
	row, key, err := t.generate()
	if err != nil {
		return nil, err
	}
	err = t.st.CreateKey(ctx, row)
	switch {
	case err == nil:
		t.log.InfoContext(ctx, "token signing key made", "fingerprint", row.Fingerprint, "algorithm", key.Algorithm)
		return row, nil
	case !errors.Is(err, index.ErrConflict):
		return nil, fmt.Errorf("keys: store the token key: %w", err)
	}
	// Another replica stored the first key between the read and the insert.
	if k, err = t.st.GetKey(ctx, nil, KindJWT); err != nil {
		return nil, fmt.Errorf("keys: the token key after a concurrent insert: %w", err)
	}
	return k, nil
}

// generate makes a new token key of the service's algorithm and returns its row, not yet
// stored, with the private key sealed by the master key, and the key itself.
func (t *Tokens) generate() (*index.Key, *TokenKey, error) {
	priv := newTokenPrivate(t.algorithm)
	key, err := tokenKeyOf(priv)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("keys: serialize the token key: %w", err)
	}
	defer clear(der)
	row := key.Key
	row.PrivateEnc = t.master.seal(row.Fingerprint, der)
	row.CreatedAt = t.now().UTC()
	key.Key = row
	return &row, key, nil
}

// newTokenPrivate makes a private key for alg: Ed25519 for TokenEdDSA, ECDSA on P-256
// otherwise. Both read crypto/rand, whose Read never fails (it crashes the program instead),
// and P-256 is supported: neither returns an error.
func newTokenPrivate(alg string) crypto.Signer {
	if alg == TokenEdDSA {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		return priv
	}
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return priv
}

// open returns the key of row k with its private key opened by the master key, checked to be
// the key of k's fingerprint and public key.
func (t *Tokens) open(k index.Key) (*TokenKey, error) {
	plain, err := t.master.open(k.Fingerprint, k.PrivateEnc)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	parsed, err := x509.ParsePKCS8PrivateKey(plain)
	if err != nil {
		return nil, fmt.Errorf("keys: the private key of %s: %w", k.Fingerprint, err)
	}
	signer, _ := parsed.(crypto.Signer) // every key ParsePKCS8PrivateKey returns is one
	key, err := tokenKeyOf(signer)
	switch {
	case err != nil:
		return nil, fmt.Errorf("keys: the row of %s: %w", k.Fingerprint, err)
	case key.Key.Fingerprint != k.Fingerprint:
		return nil, fmt.Errorf("keys: the row of %s holds the key %s", k.Fingerprint, key.Key.Fingerprint)
	case !bytes.Equal(key.Key.Public, k.Public):
		return nil, fmt.Errorf("keys: the row of %s holds another public key", k.Fingerprint)
	}
	key.Key = k
	return key, nil
}

// undecryptable adds the way out to the error of an active token key that does not open.
func undecryptable(err error) error {
	return fmt.Errorf("%w; start with the master key that made it, or retire it with lajstrom admin key drop --token, "+
		"and a new key is made", err)
}

// tokenKeyOf returns the TokenKey of priv, with the row fields that follow from the key:
// kind, public key and fingerprint. A key that is neither ECDSA on P-256 nor Ed25519 is an
// error.
func tokenKeyOf(priv crypto.Signer) (*TokenKey, error) {
	var alg string
	switch p := priv.(type) {
	case *ecdsa.PrivateKey:
		if p.Curve != elliptic.P256() {
			return nil, fmt.Errorf("an ECDSA key on %s is not a token key: ES256 is P-256", p.Curve.Params().Name)
		}
		alg = TokenES256
	case ed25519.PrivateKey:
		alg = TokenEdDSA
	default:
		return nil, fmt.Errorf("a %T is not a token key", priv)
	}
	pub := priv.Public()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("keys: serialize the public token key: %w", err)
	}
	sum := sha256.Sum256(der)
	return &TokenKey{
		Key:       index.Key{Kind: KindJWT, Public: der, Fingerprint: hex.EncodeToString(sum[:])},
		Algorithm: alg, Public: pub, Private: priv,
	}, nil
}

// EphemeralTokens is the token key of a server without a master key (ADR 0010): an ECDSA P-256
// key made in memory, which signs and verifies every token of the process and is stored
// nowhere. Its tokens do not survive a restart and are not accepted by another replica.
type EphemeralTokens struct {
	key *TokenKey
}

// NewEphemeralTokens makes the ephemeral key.
func NewEphemeralTokens() *EphemeralTokens {
	k, _ := tokenKeyOf(newTokenPrivate(TokenES256)) // an ECDSA P-256 key: never an error
	k.Key.CreatedAt = time.Now().UTC()
	return &EphemeralTokens{key: k}
}

// TokenSigner returns the ephemeral key.
func (e *EphemeralTokens) TokenSigner(context.Context) (*TokenKey, error) { return e.key, nil }

// TokenVerifiers returns the ephemeral key, without its private key.
func (e *EphemeralTokens) TokenVerifiers(context.Context) ([]*TokenKey, error) {
	return []*TokenKey{e.key.verifyOnly()}, nil
}
