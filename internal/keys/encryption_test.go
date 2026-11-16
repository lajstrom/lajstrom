package keys

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// decryptKMS is a keys.DecryptKMS in memory holding one RSA key under an ARN and an alias.
type decryptKMS struct {
	arn, alias string
	priv       *rsa.PrivateKey
	mu         sync.Mutex
	fail       error
	decrypts   int
}

func newDecryptKMS(t *testing.T) *decryptKMS {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	return &decryptKMS{arn: "arn:aws:kms:eu-west-1:111122223333:key/enc", alias: "alias/enc", priv: k}
}

func (k *decryptKMS) DecryptKey(_ context.Context, name string) (*KMSKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case k.fail != nil:
		return nil, k.fail
	case name != k.arn && name != k.alias:
		return nil, fmt.Errorf("NotFoundException: %s", name)
	}
	return &KMSKey{Name: k.arn, Public: &k.priv.PublicKey}, nil
}

func (k *decryptKMS) Decrypt(_ context.Context, name string, ciphertext []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.decrypts++
	switch {
	case k.fail != nil:
		return nil, k.fail
	case name != k.arn:
		return nil, fmt.Errorf("NotFoundException: %s", name)
	}
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, k.priv, ciphertext, nil)
}

func (k *decryptKMS) set(fail error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fail = fail
}

// encEnv is an Encryption over a fresh database with the repo raw/secrets of policy.
func encEnv(t *testing.T, policy string, kms DecryptKMS) (*Encryption, index.Store, index.Repo) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:enc-"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "raw", Name: "secrets", Registry: "local", Prefix: "raw/secrets", Kind: "hosted", Policy: json.RawMessage(policy)}
	must(t, st.CreateRepo(ctx, &repo))
	o := EncryptionOptions{Index: st, Master: master(t, testMasterHex), Log: slog.New(slog.DiscardHandler)}
	if kms != nil {
		o.KMS = map[string]DecryptKMS{config.KMSSchemeAWS: kms}
	}
	e, err := NewEncryption(o)
	must(t, err)
	return e, st, repo
}

func newOpts(t *testing.T) blobcrypt.Private {
	t.Helper()
	p := blobcrypt.NewPrivate()
	p.Digest = digest.Digest("sha256:" + strings.Repeat("ab", 32))
	return p
}

func sameOpts(a, b blobcrypt.Private) bool {
	return bytes.Equal(a.Key, b.Key) && bytes.Equal(a.Nonce(), b.Nonce()) && a.Digest == b.Digest
}

// TestEncryptionLocalKey: the first Wrap makes the repo's key, an RSA key of 3072 bits whose
// private key the master key seals; later ones wrap for the same key; Unwrap opens either.
func TestEncryptionLocalKey(t *testing.T) {
	e, st, repo := encEnv(t, `{"encryption":{"mode":"at-rest"}}`, nil)
	ctx := t.Context()
	p1, p2 := newOpts(t), newOpts(t)
	jwe1, fp1, err := e.Wrap(ctx, repo, p1)
	must(t, err)
	jwe2, fp2, err := e.Wrap(ctx, repo, p2)
	must(t, err)
	if fp1 != fp2 || len(fp1) != 64 {
		t.Fatalf("fingerprints %s and %s: want one key", fp1, fp2)
	}
	all, err := st.ListKeys(ctx, &repo.ID)
	must(t, err)
	if len(all) != 1 || all[0].Kind != KindEnc || all[0].Fingerprint != fp1 || bytes.Contains(all[0].PrivateEnc, all[0].Public) {
		t.Fatalf("keys %+v", all)
	}
	pub, err := publicOf(all[0])
	must(t, err)
	if pub.N.BitLen() != encBits {
		t.Errorf("an RSA key of %d bits", pub.N.BitLen())
	}
	for _, c := range []struct {
		jwe string
		p   blobcrypt.Private
	}{{jwe1, p1}, {jwe2, p2}} {
		got, err := e.Unwrap(ctx, repo, fp1, c.jwe)
		must(t, err)
		if !sameOpts(got, c.p) {
			t.Errorf("Unwrap = %v, want %v", got, c.p)
		}
	}
	if _, err := e.Unwrap(ctx, repo, strings.Repeat("0", 64), jwe1); err == nil {
		t.Error("Unwrap with a key the repo does not have: no error")
	}
	sealed := e.Seal("blob-upload:1", p1.Key)
	if got, err := e.Open("blob-upload:1", sealed); err != nil || !bytes.Equal(got, p1.Key) {
		t.Errorf("Open(Seal) = %x, %v", got, err)
	}
	if _, err := e.Open("blob-upload:2", sealed); err == nil {
		t.Error("Open of another row's seal: no error")
	}
}

// TestEncryptionRecipients: a recipient of the policy opens every new wrapping with its own key.
func TestEncryptionRecipients(t *testing.T) {
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	rc, err := blobcrypt.NewRecipient(&r.PublicKey)
	must(t, err)
	pemText, _ := json.Marshal(pemOfPublic(t, &r.PublicKey))
	e, _, repo := encEnv(t, `{"encryption":{"mode":"at-rest","recipients":[`+string(pemText)+`]}}`, nil)
	p := newOpts(t)
	jwe, _, err := e.Wrap(t.Context(), repo, p)
	must(t, err)
	got, err := blobcrypt.Unwrap(jwe, rc.KID, blobcrypt.RSAKey{Key: r})
	must(t, err)
	if !sameOpts(got, p) {
		t.Errorf("the recipient opened %v, want %v", got, p)
	}
}

// TestEncryptionKMS: a key of policy.encryption.key is the KMS's, its URI sealed; Unwrap asks the
// KMS once and then the cache; an unavailable KMS is a 503, a refusal is not.
func TestEncryptionKMS(t *testing.T) {
	kms := newDecryptKMS(t)
	e, st, repo := encEnv(t, `{"encryption":{"mode":"at-rest","key":"awskms:///alias/enc"}}`, kms)
	ctx := t.Context()
	p := newOpts(t)
	jwe, fp, err := e.Wrap(ctx, repo, p)
	must(t, err)
	if kms.decrypts != 0 {
		t.Errorf("Wrap asked the KMS to decrypt %d times", kms.decrypts)
	}
	k, err := st.GetKey(ctx, &repo.ID, KindEnc)
	must(t, err)
	plain, err := e.master.open(k.Fingerprint, k.PrivateEnc)
	must(t, err)
	if string(plain) != "awskms:///"+kms.arn {
		t.Errorf("private_enc seals %q, want the key URI of the ARN", plain)
	}
	for range 3 {
		got, err := e.Unwrap(ctx, repo, fp, jwe)
		must(t, err)
		if !sameOpts(got, p) {
			t.Fatalf("Unwrap = %v, want %v", got, p)
		}
	}
	if kms.decrypts != 1 {
		t.Errorf("the KMS decrypted %d times, want 1: the cache", kms.decrypts)
	}
	jwe2, _, err := e.Wrap(ctx, repo, newOpts(t))
	must(t, err)
	kms.set(fmt.Errorf("%w: ThrottlingException", ErrKMSUnavailable))
	_, err = e.Unwrap(ctx, repo, fp, jwe2)
	if !errors.Is(err, format.ErrUnavailable) || !errors.Is(err, ErrKMSUnavailable) {
		t.Errorf("an unavailable KMS: %v, want ErrUnavailable", err)
	}
	kms.set(errors.New("AccessDeniedException"))
	if _, err = e.Unwrap(ctx, repo, fp, jwe2); err == nil || errors.Is(err, format.ErrUnavailable) {
		t.Errorf("a refusal: %v, want an error that is not ErrUnavailable", err)
	}
	// Without a client for its scheme, a key in a KMS does not open.
	none, err := NewEncryption(EncryptionOptions{Index: st, Master: e.master, DataKeys: -1})
	must(t, err)
	if _, err := none.Unwrap(ctx, repo, fp, jwe); err == nil || !strings.Contains(err.Error(), "no client") {
		t.Errorf("no KMS client: %v", err)
	}
}

// TestEncryptionCacheBound: the cache keeps at most its bound, and forgets after its time.
func TestEncryptionCacheBound(t *testing.T) {
	c := &dataKeys{max: 2, ttl: time.Minute, m: map[dataKeyID]cached{}}
	now := time.Unix(1_800_000_000, 0)
	for i := range 3 {
		c.put(dataKeyID{repo: int64(i)}, newOpts(t), now)
	}
	if len(c.m) != 2 {
		t.Errorf("%d entries, want the bound 2", len(c.m))
	}
	c.put(dataKeyID{repo: 9}, newOpts(t), now)
	if _, ok := c.get(dataKeyID{repo: 9}, now.Add(time.Minute)); ok {
		t.Error("an entry outlived its time")
	}
	var nilCache *dataKeys
	nilCache.put(dataKeyID{}, newOpts(t), now)
	if _, ok := nilCache.get(dataKeyID{}, now); ok {
		t.Error("a nil cache cached")
	}
}

// TestEncryptionFirstKeyRace: replicas that make a repo's first key at once end with one.
func TestEncryptionFirstKeyRace(t *testing.T) {
	e, st, repo := encEnv(t, `{"encryption":{"mode":"at-rest"}}`, nil)
	var wg sync.WaitGroup
	fps := make([]string, 4)
	for i := range fps {
		wg.Go(func() {
			_, fp, err := e.Wrap(context.WithoutCancel(t.Context()), repo, newOpts(t))
			if err != nil {
				t.Error(err)
			}
			fps[i] = fp
		})
	}
	wg.Wait()
	all, err := st.ListKeys(t.Context(), &repo.ID)
	must(t, err)
	if len(all) != 1 {
		t.Fatalf("%d keys, want 1", len(all))
	}
	for _, fp := range fps {
		if fp != all[0].Fingerprint {
			t.Errorf("a wrap for %s, the key is %s", fp, all[0].Fingerprint)
		}
	}
}

// pemOfPublic is key as the PEM block PUBLIC KEY.
func pemOfPublic(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	must(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}
