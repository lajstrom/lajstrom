package keys

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

const (
	// KindEnc is the kind of the keys of encrypted repos in the keys table (ADR 0009).
	KindEnc = "enc"
	// encBits is the size of a key that Encryption makes.
	encBits = 3072
	// DefaultDataKeys bounds the number of data keys the cache of Unwrap holds.
	DefaultDataKeys = 4096
	// DefaultDataKeyTTL is how long the cache of Unwrap holds a data key.
	DefaultDataKeyTTL = 15 * time.Minute
)

// EncryptionOptions configure an Encryption.
type EncryptionOptions struct {
	// Index holds the keys; required.
	Index index.Store
	// Master seals the private keys and the data keys of uploads; required.
	Master *Master
	// KMS are the key management services that hold decryption keys, by the scheme of their key
	// URIs: config.KMSSchemeAWS, config.KMSSchemeGCP. A row of a scheme without one does not open.
	KMS map[string]DecryptKMS
	// Now is the clock of key creation and of the cache; nil is time.Now.
	Now func() time.Time
	// Log gets one line per key made, with its fingerprint; nil is slog.Default().
	Log *slog.Logger
	// DataKeys and DataKeyTTL bound the cache of Unwrap; zero is DefaultDataKeys and
	// DefaultDataKeyTTL, a negative DataKeys no cache.
	DataKeys   int
	DataKeyTTL time.Duration
}

// Encryption keeps the keys of encrypted repos (docs/spec/manifest-profile.md §11,
// index-schema.md keys): one active RSA key of kind enc per repo, made on the repo's first
// encrypted write from policy.encryption.key, an RSA key of 3072 bits made here, its PKCS #8
// private key sealed with the master key, or an RSA decryption key in a KMS, its key URI sealed
// instead. Wrap wraps with the public key alone; Unwrap opens with the private key, or asks the
// KMS, and caches the data key it opened. It implements format.Encryption. Replicas share the
// keys through the index; the cache is per replica.
type Encryption struct {
	st     index.Store
	master *Master
	kms    map[string]DecryptKMS
	now    func() time.Time
	log    *slog.Logger
	cache  *dataKeys
}

var _ format.Encryption = (*Encryption)(nil)

// NewEncryption returns the Encryption of o. A missing Index or Master is an error.
func NewEncryption(o EncryptionOptions) (*Encryption, error) {
	switch {
	case o.Index == nil:
		return nil, errors.New("keys: no index")
	case o.Master == nil:
		return nil, errors.New("keys: no master key")
	}
	e := &Encryption{st: o.Index, master: o.Master, kms: o.KMS, now: o.Now, log: cmp.Or(o.Log, slog.Default())}
	if e.now == nil {
		e.now = time.Now
	}
	if o.DataKeys >= 0 {
		e.cache = &dataKeys{max: cmp.Or(o.DataKeys, DefaultDataKeys), ttl: cmp.Or(o.DataKeyTTL, DefaultDataKeyTTL),
			m: map[dataKeyID]cached{}}
	}
	return e, nil
}

// Wrap wraps opts for the repo's active key, made now when there is none, and for the recipients
// of policy.encryption.
func (e *Encryption) Wrap(ctx context.Context, repo index.Repo, opts blobcrypt.Private) (string, string, error) {
	pol, err := format.EncryptionOf(repo)
	if err != nil {
		return "", "", err
	}
	row, err := e.active(ctx, repo, pol)
	if err != nil {
		return "", "", unavailable(err)
	}
	pub, err := publicOf(*row)
	if err != nil {
		return "", "", err
	}
	recipients := []blobcrypt.Recipient{{Key: pub, KID: row.Fingerprint}}
	for i, text := range pol.Recipients {
		r, err := blobcrypt.ParseRecipient(text)
		if err != nil {
			return "", "", fmt.Errorf("keys: %s: policy.encryption.recipients[%d]: %w", repoName(repo), i, err)
		}
		recipients = append(recipients, r)
	}
	jwe, err := blobcrypt.Wrap(opts, recipients)
	if err != nil {
		return "", "", fmt.Errorf("keys: %s: %w", repoName(repo), err)
	}
	return jwe, row.Fingerprint, nil
}

// Unwrap opens jwe with the repo's enc key of fingerprint, active or retired: its private key
// opened with the master key, or the KMS that holds it, which is asked to decrypt the
// recipient's encrypted key alone. A KMS that does not answer is an error wrapping
// ErrKMSUnavailable and format.ErrUnavailable.
func (e *Encryption) Unwrap(ctx context.Context, repo index.Repo, fingerprint, jwe string) (blobcrypt.Private, error) {
	id := dataKeyID{repo: repo.ID, fingerprint: fingerprint, jwe: sha256.Sum256([]byte(jwe))}
	if p, ok := e.cache.get(id, e.now()); ok {
		return p, nil
	}
	row, err := e.keyOf(ctx, repo, fingerprint)
	if err != nil {
		return blobcrypt.Private{}, err
	}
	dec, err := e.decrypter(ctx, *row)
	if err != nil {
		return blobcrypt.Private{}, err
	}
	p, err := blobcrypt.Unwrap(jwe, fingerprint, dec)
	if err != nil {
		return blobcrypt.Private{}, unavailable(fmt.Errorf("keys: %s: %w", repoName(repo), err))
	}
	e.cache.put(id, p, e.now())
	return p, nil
}

// Seal encrypts the data key of a facade upload with the master key, aad as the additional data.
func (e *Encryption) Seal(aad string, key []byte) []byte { return e.master.Seal(aad, key) }

// Open decrypts what Seal returned for aad.
func (e *Encryption) Open(aad string, sealed []byte) ([]byte, error) {
	return e.master.Open(aad, sealed)
}

// active returns the row of the repo's active enc key, made from pol.Key and stored when there is
// none. Two replicas that make it at once both insert; keys_active keeps one, and the replica
// whose insert fails reads that one.
func (e *Encryption) active(ctx context.Context, repo index.Repo, pol format.EncryptionPolicy) (*index.Key, error) {
	k, err := e.st.GetKey(ctx, &repo.ID, KindEnc)
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, index.ErrNotFound) {
		return nil, fmt.Errorf("keys: the encryption key of %s: %w", repoName(repo), err)
	}
	row, err := e.newKey(ctx, repo, pol.Key)
	if err != nil {
		return nil, err
	}
	err = e.st.CreateKey(ctx, row)
	switch {
	case err == nil:
		e.log.InfoContext(ctx, "encryption key made", "repo", repoName(repo), "fingerprint", row.Fingerprint,
			"source", cmp.Or(pol.Key, Local))
		return row, nil
	case !errors.Is(err, index.ErrConflict):
		return nil, fmt.Errorf("keys: store the encryption key of %s: %w", repoName(repo), err)
	}
	if k, err = e.st.GetKey(ctx, &repo.ID, KindEnc); err != nil {
		return nil, fmt.Errorf("keys: the encryption key of %s after a concurrent insert: %w", repoName(repo), err)
	}
	return k, nil
}

// newKey makes a new enc key for repo from source, "" for an RSA key made here or the key URI of
// a decryption key in a KMS, and returns its row, not yet stored.
func (e *Encryption) newKey(ctx context.Context, repo index.Repo, source string) (*index.Key, error) {
	var pub *rsa.PublicKey
	var priv []byte // what private_enc seals: the PKCS #8 key, or the canonical key URI
	if source == "" {
		k, err := rsa.GenerateKey(rand.Reader, encBits)
		if err != nil {
			return nil, fmt.Errorf("keys: make an encryption key for %s: %w", repoName(repo), err)
		}
		if priv, err = x509.MarshalPKCS8PrivateKey(k); err != nil {
			return nil, fmt.Errorf("keys: the encryption key for %s: %w", repoName(repo), err)
		}
		defer clear(priv)
		pub = &k.PublicKey
	} else {
		key, err := config.ParseKMSKey(source)
		if err != nil {
			return nil, fmt.Errorf("keys: policy.encryption.key of %s: %w", repoName(repo), err)
		}
		kms, err := e.kmsOf(key)
		if err != nil {
			return nil, err
		}
		desc, err := kms.DecryptKey(ctx, key.Name)
		if err != nil {
			return nil, fmt.Errorf("keys: the KMS key %s: %w", key, err)
		}
		rsaPub, ok := desc.Public.(*rsa.PublicKey)
		if !ok || rsaPub.N.BitLen() < 2048 {
			return nil, fmt.Errorf("keys: the KMS key %s: %w: not an RSA key of 2048 bits or more", key, ErrKMSKeyUnsupported)
		}
		pub, priv = rsaPub, []byte(config.KMSKey{Scheme: key.Scheme, Name: desc.Name}.String())
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("keys: the encryption key for %s: %w", repoName(repo), err)
	}
	fp, err := blobcrypt.Fingerprint(pub)
	if err != nil {
		return nil, err
	}
	id := repo.ID
	return &index.Key{RepoID: &id, Kind: KindEnc, Public: der, PrivateEnc: e.master.seal(fp, priv), Fingerprint: fp,
		CreatedAt: e.now().UTC().Truncate(time.Microsecond)}, nil
}

// keyOf returns the row of the repo's enc key of fingerprint.
func (e *Encryption) keyOf(ctx context.Context, repo index.Repo, fingerprint string) (*index.Key, error) {
	all, err := e.st.ListKeys(ctx, &repo.ID)
	if err != nil {
		return nil, fmt.Errorf("keys: the keys of %s: %w", repoName(repo), err)
	}
	for i := range all {
		if all[i].Kind == KindEnc && all[i].Fingerprint == fingerprint {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("keys: %s has no encryption key %s", repoName(repo), fingerprint)
}

// decrypter returns what decrypts with the key of row k: its private key, opened with the master
// key and checked against the row's public key, or its KMS with ctx.
func (e *Encryption) decrypter(ctx context.Context, k index.Key) (blobcrypt.KeyDecrypter, error) {
	plain, err := e.master.open(k.Fingerprint, k.PrivateEnc)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	if len(plain) > 0 && plain[0] == 0x30 { // DER: a SEQUENCE, a PKCS #8 key; a key URI starts with a letter
		key, err := x509.ParsePKCS8PrivateKey(plain)
		if err != nil {
			return nil, fmt.Errorf("keys: the private key of %s: %w", k.Fingerprint, err)
		}
		priv, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("keys: the row of %s holds a %T", k.Fingerprint, key)
		}
		der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil || !bytes.Equal(der, k.Public) {
			return nil, fmt.Errorf("keys: the row of %s holds another key", k.Fingerprint)
		}
		return blobcrypt.RSAKey{Key: priv}, nil
	}
	key, err := config.ParseKMSKey(string(plain))
	if err != nil {
		return nil, fmt.Errorf("keys: the row of %s: %w", k.Fingerprint, err)
	}
	kms, err := e.kmsOf(key)
	if err != nil {
		return nil, err
	}
	return kmsDecrypter{ctx: ctx, kms: kms, name: key.Name}, nil
}

// kmsOf returns the client of the KMS of key.
func (e *Encryption) kmsOf(key config.KMSKey) (DecryptKMS, error) {
	if k := e.kms[key.Scheme]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("keys: %s: this server has no client for %s", key, key.Scheme)
}

// publicOf returns the RSA public key of an enc row.
func publicOf(k index.Key) (*rsa.PublicKey, error) {
	key, err := x509.ParsePKIXPublicKey(k.Public)
	if err != nil {
		return nil, fmt.Errorf("keys: the public key of %s: %w", k.Fingerprint, err)
	}
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("keys: the public key of %s is a %T", k.Fingerprint, key)
	}
	return pub, nil
}

// kmsDecrypter is a blobcrypt.KeyDecrypter over a key in a KMS, with the context of the call.
type kmsDecrypter struct {
	ctx  context.Context //nolint:containedctx // the context of one Unwrap, which go-jose's callback does not pass
	kms  DecryptKMS
	name string
}

func (k kmsDecrypter) DecryptOAEP(encryptedKey []byte) ([]byte, error) {
	return k.kms.Decrypt(k.ctx, k.name, encryptedKey)
}

// dataKeys caches what Unwrap opened, by repo, key and the hash of the wrapping, for ttl, max
// entries at most. A nil cache caches nothing.
type dataKeys struct {
	max int
	ttl time.Duration
	mu  sync.Mutex
	m   map[dataKeyID]cached
}

type dataKeyID struct {
	repo        int64
	fingerprint string
	jwe         [sha256.Size]byte
}

type cached struct {
	p       blobcrypt.Private
	expires time.Time
}

func (c *dataKeys) get(id dataKeyID, now time.Time) (blobcrypt.Private, bool) {
	if c == nil {
		return blobcrypt.Private{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[id]
	if !ok || !now.Before(v.expires) {
		return blobcrypt.Private{}, false
	}
	return v.p, true
}

// put adds p; a full cache drops its expired entries first, then any.
func (c *dataKeys) put(id dataKeyID, p blobcrypt.Private, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		for k, v := range c.m {
			if !now.Before(v.expires) {
				delete(c.m, k)
			}
		}
		for k := range c.m {
			if len(c.m) < c.max {
				break
			}
			delete(c.m, k)
		}
	}
	c.m[id] = cached{p: p, expires: now.Add(c.ttl)}
}

// CanOpen reports whether the repo's enc keys open: the seal of each with the master key, and a
// client for the KMS of a key held in one. It asks no KMS anything.
func (e *Encryption) CanOpen(ctx context.Context, repo index.Repo) error {
	all, err := e.st.ListKeys(ctx, &repo.ID)
	if err != nil {
		return fmt.Errorf("keys: the keys of %s: %w", repoName(repo), err)
	}
	for _, k := range all {
		if k.Kind != KindEnc {
			continue
		}
		if _, err := e.decrypter(ctx, k); err != nil {
			return err
		}
	}
	return nil
}
