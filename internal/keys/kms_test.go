package keys

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// fakeKMS is a KMS in memory: keys by name and by alias, each with its private key, and the
// failures a test sets.
type fakeKMS struct {
	mu      sync.Mutex
	keys    map[string]*fakeKMSKey // by canonical name and by alias
	signErr error                  // what Sign answers, when set
	keyErr  error                  // what Key answers, when set
	tamper  bool                   // Sign answers a signature by another key
	signs   int
}

type fakeKMSKey struct {
	name    string
	priv    crypto.Signer
	created time.Time
}

func newFakeKMS() *fakeKMS { return &fakeKMS{keys: map[string]*fakeKMSKey{}} }

// add holds priv as the key of the canonical name and its aliases, made at created.
func (f *fakeKMS) add(name string, priv crypto.Signer, created time.Time, aliases ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := &fakeKMSKey{name: name, priv: priv, created: created}
	for _, n := range append(aliases, name) {
		f.keys[n] = k
	}
}

func (f *fakeKMS) set(fn func(*fakeKMS)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeKMS) Key(_ context.Context, name string) (*KMSKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keyErr != nil {
		return nil, f.keyErr
	}
	k := f.keys[name]
	if k == nil {
		return nil, fmt.Errorf("NotFoundException: key %s", name)
	}
	return &KMSKey{Name: k.name, Public: k.priv.Public(), Created: k.created}, nil
}

func (f *fakeKMS) Sign(_ context.Context, name string, _ crypto.PublicKey, digest []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signs++
	if f.signErr != nil {
		return nil, f.signErr
	}
	k := f.keys[name]
	if k == nil || k.name != name {
		return nil, fmt.Errorf("NotFoundException: Sign takes the canonical name, not %s", name)
	}
	priv := k.priv
	if f.tamper {
		priv = otherKey(priv)
	}
	return priv.Sign(rand.Reader, digest, crypto.SHA256)
}

func (f *fakeKMS) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signs
}

// otherKey is a fresh key of the type of priv.
func otherKey(priv crypto.Signer) crypto.Signer {
	if _, ok := priv.(*rsa.PrivateKey); ok {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		return k
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return k
}

const (
	kmsARN   = "arn:aws:kms:eu-west-1:111122223333:key/0e9f6c2a-test"
	kmsAlias = "alias/lajstrom-apt"
)

// kmsKeys are the private keys of the KMS tests, made once: RSA keys take a while.
var kmsKeys = sync.OnceValue(func() map[string]crypto.Signer {
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return map[string]crypto.Signer{"rsa2048": r, "p256": e}
})

// newKMSEnv is newEnv with a fake AWS KMS that holds priv as kmsARN, alias kmsAlias, made a day
// before the env's clock, and keys.kms set to kms ("" for none).
func newKMSEnv(t *testing.T, priv crypto.Signer, kms string) (*env, *fakeKMS) {
	t.Helper()
	f := newFakeKMS()
	e := newEnv(t, Options{KMS: map[string]KMS{config.KMSSchemeAWS: f}, KMSKey: kms, RetireGrace: 720 * time.Hour})
	f.add(kmsARN, priv, e.now.Add(-24*time.Hour), kmsAlias)
	return e, f
}

// TestKMSKey: under keys.kms a repo's first key is the KMS key, wrapped in an OpenPGP key whose
// creation time is the KMS key's and whose self-signature the KMS made; the row seals the
// canonical key URI, the alias resolved; InRelease and Release.gpg verify against key.asc, each
// signature one KMS request; a second repo on the same KMS key gets the same fingerprint.
func TestKMSKey(t *testing.T) {
	for name, algo := range map[string]packet.PublicKeyAlgorithm{"rsa2048": packet.PubKeyAlgoRSA, "p256": packet.PubKeyAlgoECDSA} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			e, f := newKMSEnv(t, kmsKeys()[name], "awskms:///"+kmsAlias)
			kr := keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))
			row := must2(e.st.GetKey(ctx, &e.repo.ID, KindGPG))
			checkKMSKey(t, e, kr, *row, algo)
			if f.calls() != 1 {
				t.Errorf("%d signatures for the key, want 1: its self-signature", f.calls())
			}

			in := must2(e.svc.ClearSign(ctx, e.repo, []byte(release)))
			if body, err := verifyClear(t, in, kr); err != nil || string(body)+"\n" != release {
				t.Fatalf("InRelease: %q, %v", body, err)
			}
			sig := must2(e.svc.DetachSign(ctx, e.repo, []byte(release)))
			if _, err := openpgp.CheckArmoredDetachedSignature(kr, strings.NewReader(release), bytes.NewReader(sig), nil); err != nil {
				t.Fatalf("Release.gpg: %v", err)
			}
			if f.calls() != 3 {
				t.Errorf("%d signatures in all, want 3", f.calls())
			}
			for _, doc := range [][]byte{in, sig} {
				if cur, err := e.svc.Current(ctx, e.repo, doc); err != nil || !cur {
					t.Errorf("Current = %v, %v", cur, err)
				}
			}

			other := index.Repo{Format: "rpm", Name: "el9", Registry: "local", Prefix: "rpm/el9", Kind: "hosted"}
			must(t, e.st.CreateRepo(ctx, &other))
			if k2 := must2(e.svc.KeyFor(ctx, other)); k2.Key.Fingerprint != row.Fingerprint {
				t.Errorf("rpm/el9 on the same KMS key: %s, want %s", k2.Key.Fingerprint, row.Fingerprint)
			}
		})
	}
}

// checkKMSKey checks the keyring kr of the env's repo, one key: the KMS key, of algo, created
// when the KMS made it, with the repo's user id; and its row, which seals the key URI of the
// key's ARN, the alias resolved.
func checkKMSKey(t *testing.T, e *env, kr openpgp.EntityList, row index.Key, algo packet.PublicKeyAlgorithm) {
	t.Helper()
	if len(kr) != 1 {
		t.Fatalf("key.asc holds %d keys", len(kr))
	}
	k := kr[0]
	if k.PrimaryKey.PubKeyAlgo != algo || !k.PrimaryKey.CreationTime.Equal(e.now.Add(-24*time.Hour).Truncate(time.Second)) {
		t.Errorf("key: algorithm %v created %v, want %v created a day before %v", k.PrimaryKey.PubKeyAlgo,
			k.PrimaryKey.CreationTime, algo, e.now)
	}
	if _, ok := k.Identities["Lajstrom apt/ubuntu"]; !ok || k.PrivateKey != nil {
		t.Errorf("identities %v, private key %v", k.Identities, k.PrivateKey)
	}
	if row.Fingerprint != fingerprint(k) {
		t.Errorf("row %s, key.asc %s", row.Fingerprint, fingerprint(k))
	}
	plain := must2(e.svc.master.open(row.Fingerprint, row.PrivateEnc))
	if string(plain) != "awskms:///"+kmsARN || bytes.Contains(row.PrivateEnc, []byte("awskms")) {
		t.Errorf("private_enc opens to %q", plain)
	}
}

// TestKMSRotate: a repo moves onto a KMS key, off it and back with RotateTo, each an ordinary
// rotation whose retired key signs beside the new one; back on the KMS key within the grace it
// has the fingerprint it had, once in key.asc; a rotation to the active key changes nothing.
func TestKMSRotate(t *testing.T) {
	ctx := t.Context()
	e, _ := newKMSEnv(t, kmsKeys()["p256"], "")
	local := must2(e.svc.KeyFor(ctx, e.repo)).Key.Fingerprint
	e.now = e.now.Add(time.Hour)
	onKMS := must2(e.svc.RotateTo(ctx, e.repo, "awskms:///"+kmsARN)).Key.Fingerprint
	if got := fingerprints(keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))); !equal(got, []string{onKMS, local}) {
		t.Fatalf("key.asc after the move onto the KMS = %v, want %v", got, []string{onKMS, local})
	}
	if got := issuers(t, must2(e.svc.ClearSign(ctx, e.repo, []byte(release)))); !equal(got, []string{onKMS, local}) {
		t.Errorf("InRelease signed by %v", got)
	}
	_, err := e.svc.RotateTo(ctx, e.repo, "awskms:///"+kmsAlias)
	if err == nil || !strings.Contains(err.Error(), "already "+onKMS) {
		t.Fatalf("rotation to the active key: %v", err)
	}
	e.now = e.now.Add(time.Hour)
	back := must2(e.svc.RotateTo(ctx, e.repo, Local)).Key.Fingerprint
	e.now = e.now.Add(time.Hour)
	again := must2(e.svc.RotateTo(ctx, e.repo, "awskms:///"+kmsAlias)).Key.Fingerprint
	if again != onKMS {
		t.Errorf("the KMS key again is %s, want %s", again, onKMS)
	}
	want := []string{onKMS, back, local}
	if got := fingerprints(keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))); !equal(got, want) {
		t.Errorf("key.asc = %v, want %v", got, want)
	}
	if got := issuers(t, must2(e.svc.DetachSign(ctx, e.repo, []byte(release)))); !equal(got, want) {
		t.Errorf("Release.gpg signed by %v, want %v", got, want)
	}
}

// TestKMSFails: a KMS that is unavailable makes ClearSign, DetachSign and a first key under
// keys.kms errors that the routes answer 503 with Retry-After: 5, and one that refuses errors
// they answer 500; a signature the key does not verify is an error for RSA, whose error
// go-crypto drops, and for ECDSA. Nothing is ever returned beside the error.
func TestKMSFails(t *testing.T) {
	unavail := fmt.Errorf("%w: dial tcp: connection refused", ErrKMSUnavailable)
	for _, tt := range []struct {
		name, key string
		set       func(*fakeKMS)
		status    int
	}{
		{"unavailable/rsa", "rsa2048", func(f *fakeKMS) { f.signErr = unavail }, http.StatusServiceUnavailable},
		{"unavailable/p256", "p256", func(f *fakeKMS) { f.signErr = unavail }, http.StatusServiceUnavailable},
		{"refused/rsa", "rsa2048", func(f *fakeKMS) { f.signErr = errors.New("DisabledException") }, http.StatusInternalServerError},
		{"refused/p256", "p256", func(f *fakeKMS) { f.signErr = errors.New("AccessDeniedException") }, http.StatusInternalServerError},
		{"tampered/rsa", "rsa2048", func(f *fakeKMS) { f.tamper = true }, http.StatusInternalServerError},
		{"tampered/p256", "p256", func(f *fakeKMS) { f.tamper = true }, http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			e, f := newKMSEnv(t, kmsKeys()[tt.key], "awskms:///"+kmsAlias)
			pub := must2(e.svc.PublicKeys(ctx, e.repo))
			f.set(tt.set)
			for name, sign := range map[string]func(context.Context, index.Repo, []byte) ([]byte, error){
				"ClearSign": e.svc.ClearSign, "DetachSign": e.svc.DetachSign,
			} {
				out, err := sign(ctx, e.repo, []byte(release))
				if err == nil || out != nil {
					t.Fatalf("%s = %q, %v; want an error and nothing", name, out, err)
				}
				checkStatus(t, err, tt.status)
			}
			if again, err := e.svc.PublicKeys(ctx, e.repo); err != nil || !bytes.Equal(again, pub) {
				t.Errorf("key.asc asks the KMS: %v", err)
			}
		})
	}

	t.Run("first key", func(t *testing.T) {
		ctx := t.Context()
		e, f := newKMSEnv(t, kmsKeys()["p256"], "awskms:///"+kmsAlias)
		f.set(func(f *fakeKMS) { f.keyErr = unavail })
		_, err := e.svc.PublicKeys(ctx, e.repo)
		checkStatus(t, err, http.StatusServiceUnavailable)
		if keys := must2(e.st.ListKeys(ctx, &e.repo.ID)); len(keys) != 0 {
			t.Errorf("%d keys stored", len(keys))
		}
		f.set(func(f *fakeKMS) { f.keyErr, f.signErr = nil, unavail }) // the self-signature
		_, err = e.svc.ClearSign(ctx, e.repo, []byte(release))
		checkStatus(t, err, http.StatusServiceUnavailable)
		if keys := must2(e.st.ListKeys(ctx, &e.repo.ID)); len(keys) != 0 {
			t.Errorf("%d keys stored", len(keys))
		}
	})
}

// checkStatus checks the status the routes answer err with, and the Retry-After of a 503.
func checkStatus(t *testing.T, err error, status int) {
	t.Helper()
	if got := format.StatusOf(err); got != status {
		t.Fatalf("%v: status %d, want %d", err, got, status)
	}
	h := http.Header{}
	format.SetRetryAfter(h, err)
	if want := map[bool]string{true: "5", false: ""}[status == http.StatusServiceUnavailable]; h.Get("Retry-After") != want {
		t.Errorf("Retry-After %q, want %q", h.Get("Retry-After"), want)
	}
}

// TestKMSRetiredRefused: a retired key whose KMS refuses is left out of the signatures, which
// the active key alone makes; a retired key whose KMS is unavailable fails the document.
func TestKMSRetiredRefused(t *testing.T) {
	ctx := t.Context()
	e, f := newKMSEnv(t, kmsKeys()["p256"], "awskms:///"+kmsAlias)
	onKMS := must2(e.svc.KeyFor(ctx, e.repo)).Key.Fingerprint
	e.now = e.now.Add(time.Hour)
	local := must2(e.svc.RotateTo(ctx, e.repo, Local)).Key.Fingerprint
	f.set(func(f *fakeKMS) { f.signErr = errors.New("DisabledException: the key is disabled") })
	for _, sign := range []func(context.Context, index.Repo, []byte) ([]byte, error){e.svc.ClearSign, e.svc.DetachSign} {
		if got := issuers(t, must2(sign(ctx, e.repo, []byte(release)))); !equal(got, []string{local}) {
			t.Errorf("signed by %v, want %v alone, not %s", got, local, onKMS)
		}
	}
	f.set(func(f *fakeKMS) { f.signErr = fmt.Errorf("%w: ThrottlingException", ErrKMSUnavailable) })
	_, err := e.svc.ClearSign(ctx, e.repo, []byte(release))
	checkStatus(t, err, http.StatusServiceUnavailable)
}

// TestKMSUnsupported: a KMS key that is not RSA of 2048 bits or more or ECDSA on P-256 is
// refused before anything is stored, and so is keys.kms without a KMS of its scheme.
func TestKMSUnsupported(t *testing.T) {
	ctx := t.Context()
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	must(t, err)
	rsa1024, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // a key too weak on purpose
	must(t, err)
	for name, priv := range map[string]crypto.Signer{"P-384": p384, "1024 bits": rsa1024} {
		t.Run(name, func(t *testing.T) {
			e, _ := newKMSEnv(t, priv, "")
			_, err := e.svc.RotateTo(ctx, e.repo, "awskms:///"+kmsARN)
			if !errors.Is(err, ErrKMSKeyUnsupported) || !strings.Contains(err.Error(), name) {
				t.Errorf("%v", err)
			}
			if keys := must2(e.st.ListKeys(ctx, &e.repo.ID)); len(keys) != 0 {
				t.Errorf("%d keys stored", len(keys))
			}
		})
	}
	st := newEnv(t, Options{}).st
	for uri, want := range map[string]string{
		"gcpkms://projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1": "no client for gcpkms",
		"awskms://kms.local/alias/x": "names an endpoint",
	} {
		_, err := New(Options{Index: st, Master: master(t, testMasterHex), KMSKey: uri,
			KMS: map[string]KMS{config.KMSSchemeAWS: newFakeKMS()}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("keys.kms %s: %v, want %q", uri, err, want)
		}
	}
}

// TestKMSRowSealed: the key URI of a KMS row opens only under its fingerprint: a row whose
// private_enc is another row's does not decrypt, so a database write cannot point a repo at
// another KMS key.
func TestKMSRowSealed(t *testing.T) {
	ctx := t.Context()
	e, _ := newKMSEnv(t, kmsKeys()["p256"], "awskms:///"+kmsAlias)
	row := must2(e.svc.KeyFor(ctx, e.repo)).Key
	if _, err := e.svc.decrypt(ctx, row); err != nil {
		t.Fatalf("the KMS row as stored: %v", err)
	}
	other, _, err := e.svc.newKey(ctx, e.repo, Local)
	must(t, err)
	moved := row
	moved.PrivateEnc = e.svc.master.seal(other.Fingerprint, []byte("awskms:///arn:aws:kms:eu-west-1:999999999999:key/theirs"))
	if _, err := e.svc.decrypt(ctx, moved); err == nil || !strings.Contains(err.Error(), "does not decrypt") {
		t.Errorf("a KMS row with another row's seal: %v", err)
	}
}
