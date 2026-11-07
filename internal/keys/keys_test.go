package keys

import (
	"bytes"
	"context"
	"crypto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
)

// release is a document to sign, shaped like an apt Release: it ends with a line break, and a
// line starts with a space.
const release = "Origin: Lajstrom\nLabel: ubuntu\nSuite: jammy\nSHA256:\n 0123 12 main/binary-amd64/Packages\n"

// env is a Service over a fresh SQLite database in memory with one repo, apt/ubuntu, and a
// clock the test moves.
type env struct {
	t    *testing.T
	st   index.Store
	repo index.Repo
	now  time.Time
	svc  *Service
}

// newEnv returns an env whose Service has o over the database, the test master key and the
// env's clock, which starts 60 days ago so that keys and signatures are never in the future of
// a verifier.
func newEnv(t *testing.T, o Options) *env {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:keys-"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &env{t: t, st: st, now: time.Now().Add(-60 * 24 * time.Hour)}
	e.repo = index.Repo{Format: "apt", Name: "ubuntu", Registry: "local", Prefix: "apt/ubuntu", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &e.repo))
	if o.Index == nil {
		o.Index = st
	}
	if o.Master == nil {
		o.Master = master(t, testMasterHex)
	}
	o.Now = func() time.Time { return e.now }
	o.Log = slog.New(slog.DiscardHandler)
	e.svc, err = New(o)
	must(t, err)
	return e
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// keyring parses an armored keyring.
func keyring(t testing.TB, armored []byte) openpgp.EntityList {
	t.Helper()
	el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armored))
	must(t, err)
	return el
}

// fingerprints returns the fingerprints of the keys of el, in order.
func fingerprints(el openpgp.EntityList) []string {
	var fps []string
	for _, e := range el {
		fps = append(fps, fingerprint(e))
	}
	return fps
}

// verifyClear checks the clearsigned document signed against kr and returns its cleartext.
func verifyClear(t testing.TB, signed []byte, kr openpgp.KeyRing) ([]byte, error) {
	t.Helper()
	b, rest := clearsign.Decode(signed)
	if b == nil || len(rest) != 0 {
		t.Fatalf("not one clearsigned block:\n%s", signed)
	}
	_, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(b.Bytes), b.ArmoredSignature.Body, nil)
	return b.Plaintext, err
}

// issuers returns the fingerprints of the issuers of the signatures in an armored block, or in
// the signature block of a clearsigned document.
func issuers(t testing.TB, signed []byte) []string {
	t.Helper()
	var body io.Reader
	if b, _ := clearsign.Decode(signed); b != nil {
		body = b.ArmoredSignature.Body
	} else {
		block, err := armor.Decode(bytes.NewReader(signed))
		must(t, err)
		body = block.Body
	}
	var fps []string
	r := packet.NewReader(body)
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return fps
		}
		must(t, err)
		sig, ok := p.(*packet.Signature)
		if !ok {
			t.Fatalf("a %T in a signature block", p)
		}
		if sig.Hash != crypto.SHA256 {
			t.Errorf("a signature with the hash %v, want SHA-256", sig.Hash)
		}
		fps = append(fps, strings.ToUpper(hex.EncodeToString(sig.IssuerFingerprint)))
	}
}

func TestNew(t *testing.T) {
	st, m := &struct{ index.Store }{}, master(t, testMasterHex)
	for _, tt := range []struct {
		name string
		o    Options
		want string
	}{
		{"no index", Options{Master: m}, "no index"},
		{"no master key", Options{Index: st}, "no master key"},
		{"an unknown algorithm", Options{Index: st, Master: m, Algorithm: "dsa"}, `algorithm "dsa" is not ed25519 or rsa4096`},
		{"a negative grace", Options{Index: st, Master: m, RetireGrace: -time.Hour}, "negative"},
	} {
		if _, err := New(tt.o); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("New with %s: %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
	s, err := New(Options{Index: st, Master: m})
	must(t, err)
	if s.algorithm != AlgorithmEd25519 || s.grace != 0 || s.now == nil || s.log == nil {
		t.Errorf("New with defaults = %+v", s)
	}
}

// TestKeyFor checks the key a repo gets on first use, for both algorithms: one row with the
// public key packets, the private key encrypted (no private packet in the clear), the
// fingerprint, the user id, the algorithm, no expiry, SHA-256; the next call returns the same
// key, decrypted from the row.
func TestKeyFor(t *testing.T) {
	for _, tt := range []struct {
		algorithm string
		pubAlgo   packet.PublicKeyAlgorithm
		bits      int // RSA only
	}{
		{AlgorithmEd25519, packet.PubKeyAlgoEdDSA, 0},
		{AlgorithmRSA4096, packet.PubKeyAlgoRSA, 4096},
	} {
		t.Run(tt.algorithm, func(t *testing.T) {
			e := newEnv(t, Options{Algorithm: tt.algorithm})
			ctx := t.Context()
			first, err := e.svc.KeyFor(ctx, e.repo)
			must(t, err)
			rows, err := e.st.ListKeys(ctx, &e.repo.ID)
			must(t, err)
			if len(rows) != 1 {
				t.Fatalf("%d keys after the first KeyFor, want 1", len(rows))
			}
			row := rows[0]
			if row.Kind != KindGPG || row.RetiredAt != nil || len(row.Fingerprint) != 40 ||
				row.Fingerprint != strings.ToUpper(row.Fingerprint) || row.Fingerprint != first.Key.Fingerprint {
				t.Errorf("row %+v, key %s", row, first)
			}
			checkKey(t, first.OpenPGP, row, tt.pubAlgo, tt.bits)
			checkStored(t, e.svc.master, row)
			again, err := e.svc.KeyFor(ctx, e.repo)
			must(t, err)
			if again.Key.ID != row.ID || again.OpenPGP.PrivateKey == nil || again.OpenPGP.PrivateKey.Encrypted {
				t.Errorf("the second KeyFor = %s (id %d), want the row %d decrypted", again, again.Key.ID, row.ID)
			}
		})
	}
}

// checkKey checks a key KeyFor made for the row: its algorithm (and the RSA bits or the
// curve), the user id, a self-signature with SHA-256, signing and no expiry, the creation time.
func checkKey(t *testing.T, k *openpgp.Entity, row index.Key, algo packet.PublicKeyAlgorithm, bits int) {
	t.Helper()
	if k.PrimaryKey.PubKeyAlgo != algo {
		t.Errorf("algorithm %v, want %v", k.PrimaryKey.PubKeyAlgo, algo)
	}
	if algo == packet.PubKeyAlgoRSA {
		if n, err := k.PrimaryKey.BitLength(); err != nil || int(n) != bits {
			t.Errorf("bit length %d, %v; want %d", n, err, bits)
		}
	} else if curve, err := k.PrimaryKey.Curve(); err != nil || curve != packet.Curve25519 {
		t.Errorf("curve %v, %v; want Curve25519", curve, err)
	}
	id := k.PrimaryIdentity()
	if id == nil || id.Name != "Lajstrom apt/ubuntu" {
		t.Fatalf("user id %+v, want Lajstrom apt/ubuntu", id)
	}
	sig := id.SelfSignature
	if sig.Hash != crypto.SHA256 || (sig.KeyLifetimeSecs != nil && *sig.KeyLifetimeSecs != 0) || !sig.FlagSign {
		t.Errorf("self-signature: hash %v, key lifetime %v, sign %t", sig.Hash, sig.KeyLifetimeSecs, sig.FlagSign)
	}
	if !k.PrimaryKey.CreationTime.Equal(row.CreatedAt) {
		t.Errorf("created %v, row %v", k.PrimaryKey.CreationTime, row.CreatedAt)
	}
}

// checkStored checks what the row stores: the public key packets alone in keys.public, and in
// keys.private_enc the private key packets encrypted with m, none of their bytes in the clear.
func checkStored(t *testing.T, m *Master, row index.Key) {
	t.Helper()
	pub, err := openpgp.ReadKeyRing(bytes.NewReader(row.Public))
	must(t, err)
	if len(pub) != 1 || pub[0].PrivateKey != nil || fingerprint(pub[0]) != row.Fingerprint {
		t.Errorf("keys.public holds %d keys, private %v", len(pub), pub[0].PrivateKey != nil)
	}
	plain, err := m.open(row.Fingerprint, row.PrivateEnc)
	must(t, err)
	priv, err := openpgp.ReadKeyRing(bytes.NewReader(plain))
	if err != nil || len(priv) != 1 || priv[0].PrivateKey == nil || fingerprint(priv[0]) != row.Fingerprint {
		t.Errorf("keys.private_enc does not decrypt to the private key: %v", err)
	}
	if bytes.Contains(row.PrivateEnc, plain[:32]) || bytes.Contains(row.PrivateEnc, plain[len(plain)-32:]) {
		t.Error("keys.private_enc contains the private key packets in the clear")
	}
}

// TestSign signs with a repo's key: ClearSign is an InRelease as gpg writes it, whose
// cleartext is the document and whose signature verifies against the key and against the
// repo's keyring; DetachSign is an armored binary signature of the exact bytes.
func TestSign(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	key, err := e.svc.KeyFor(ctx, e.repo)
	must(t, err)
	kr := keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))
	if got := fingerprints(kr); len(got) != 1 || got[0] != key.Key.Fingerprint {
		t.Fatalf("keyring %v, want the key %s", got, key)
	}

	in, err := e.svc.ClearSign(ctx, e.repo, []byte(release))
	must(t, err)
	s := string(in)
	if !strings.HasPrefix(s, "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n"+release+"-----BEGIN PGP SIGNATURE-----\n") ||
		!strings.HasSuffix(s, "-----END PGP SIGNATURE-----\n") {
		t.Errorf("InRelease is not shaped as gpg --clearsign writes it:\n%s", in)
	}
	for name, kr := range map[string]openpgp.KeyRing{"the key": openpgp.EntityList{key.OpenPGP}, "key.asc": kr} {
		plain, err := verifyClear(t, in, kr)
		if err != nil || string(plain)+"\n" != release {
			t.Errorf("InRelease against %s: %v, cleartext %q", name, err, plain)
		}
	}
	if got := issuers(t, in); len(got) != 1 || got[0] != key.Key.Fingerprint {
		t.Errorf("InRelease signed by %v", got)
	}

	sig, err := e.svc.DetachSign(ctx, e.repo, []byte(release))
	must(t, err)
	if !strings.HasPrefix(string(sig), "-----BEGIN PGP SIGNATURE-----\n") || !strings.HasSuffix(string(sig), "-----END PGP SIGNATURE-----\n") {
		t.Errorf("Release.gpg:\n%s", sig)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(kr, strings.NewReader(release), bytes.NewReader(sig), nil); err != nil {
		t.Errorf("Release.gpg against key.asc: %v", err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(kr, strings.NewReader(release+" "), bytes.NewReader(sig), nil); err == nil {
		t.Error("Release.gpg verifies other bytes")
	}
}

// must2 returns v and stops a test on err; for one-line calls in tests.
func must2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestRotate rotates a repo's key twice and checks what clients see: the old InRelease
// verifies against the new keyring; for the grace the new InRelease and Release.gpg carry a
// signature of the new key and one of the retired key, so they verify against the keyring of
// before the rotation and against the new one; after the grace the retired key neither signs
// nor is in key.asc. A rotation drops the repo's index_cache and writes the audit row.
func TestRotate(t *testing.T) {
	e := newEnv(t, Options{RetireGrace: 30 * 24 * time.Hour})
	ctx := auth.WithPrincipal(t.Context(), auth.Principal{Kind: auth.KindUser, Name: "admin"})
	old, err := e.svc.KeyFor(ctx, e.repo)
	must(t, err)
	oldRing := keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))
	oldIn := must2(e.svc.ClearSign(ctx, e.repo, []byte(release)))
	must(t, e.st.PutIndexCache(ctx, &index.Cache{RepoID: e.repo.ID, Scope: "jammy", Kind: "InRelease", Content: oldIn, SHA256: "x"}))

	e.now = e.now.Add(time.Hour)
	rotated, err := e.svc.Rotate(ctx, e.repo)
	must(t, err)
	if rotated.Key.Fingerprint == old.Key.Fingerprint || rotated.OpenPGP.PrivateKey == nil {
		t.Fatalf("Rotate returned %s, the old key is %s", rotated, old)
	}
	newRing := keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))
	if got, want := fingerprints(newRing), []string{rotated.Key.Fingerprint, old.Key.Fingerprint}; !equal(got, want) {
		t.Errorf("keyring after the rotation %v, want %v (the new key first)", got, want)
	}
	if _, err := e.st.GetIndexCache(ctx, e.repo.ID, "jammy", "InRelease"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("index_cache after the rotation: %v, want it dropped", err)
	}
	evs, err := e.st.ListAudit(ctx, index.AuditFilter{Action: ActionRotate})
	must(t, err)
	if len(evs) != 1 || evs[0].Repo != "apt/ubuntu" || evs[0].Subject != rotated.Key.Fingerprint ||
		evs[0].Principal != "user:admin" || !jsonEqual(evs[0].Details, `{"retired": "`+old.Key.Fingerprint+`"}`) {
		t.Errorf("audit rows %+v", evs)
	}
	if _, err := verifyClear(t, oldIn, newRing); err != nil {
		t.Errorf("the InRelease of before the rotation against the new keyring: %v", err)
	}

	checkGrace(t, e, []string{rotated.Key.Fingerprint, old.Key.Fingerprint},
		map[string]openpgp.EntityList{"the keyring of before": oldRing, "the new keyring": newRing, "the new key alone": {rotated.OpenPGP}})

	e.now = e.now.Add(31 * 24 * time.Hour)
	if got := fingerprints(keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))); !equal(got, []string{rotated.Key.Fingerprint}) {
		t.Errorf("keyring after the grace %v, want the new key alone", got)
	}
	in := must2(e.svc.ClearSign(ctx, e.repo, []byte(release)))
	if got := issuers(t, in); !equal(got, []string{rotated.Key.Fingerprint}) {
		t.Errorf("InRelease after the grace signed by %v", got)
	}
	if _, err := verifyClear(t, in, oldRing); err == nil {
		t.Error("an InRelease of after the grace verifies against the keyring of before the rotation")
	}
}

// checkGrace checks InRelease and Release.gpg signed in the grace after a rotation: signed by
// the keys of want, in that order, and verifying against each keyring of rings.
func checkGrace(t *testing.T, e *env, want []string, rings map[string]openpgp.EntityList) {
	t.Helper()
	ctx := t.Context()
	in := must2(e.svc.ClearSign(ctx, e.repo, []byte(release)))
	sig := must2(e.svc.DetachSign(ctx, e.repo, []byte(release)))
	if got := issuers(t, in); !equal(got, want) {
		t.Errorf("InRelease in the grace signed by %v, want %v", got, want)
	}
	if got := issuers(t, sig); !equal(got, want) {
		t.Errorf("Release.gpg in the grace signed by %v, want %v", got, want)
	}
	for name, kr := range rings {
		if _, err := verifyClear(t, in, kr); err != nil {
			t.Errorf("InRelease in the grace against %s: %v", name, err)
		}
		if _, err := openpgp.CheckArmoredDetachedSignature(kr, strings.NewReader(release), bytes.NewReader(sig), nil); err != nil {
			t.Errorf("Release.gpg in the grace against %s: %v", name, err)
		}
	}
}

// TestPublicKeysGrace checks which retired keys key.asc holds, with the clock moved: the active
// key first, then those retired within the grace, the most recently retired first; one
// retired longer ago is left out.
func TestPublicKeysGrace(t *testing.T) {
	e := newEnv(t, Options{RetireGrace: 720 * time.Hour}) // the default of keys.retireGrace
	ctx := t.Context()
	a := must2(e.svc.KeyFor(ctx, e.repo)).Key.Fingerprint
	b := must2(e.svc.Rotate(ctx, e.repo)).Key.Fingerprint // a retired at day 0
	e.now = e.now.Add(10 * 24 * time.Hour)
	c := must2(e.svc.Rotate(ctx, e.repo)).Key.Fingerprint // b retired at day 10
	start := e.now.Add(-10 * 24 * time.Hour)
	for _, tt := range []struct {
		day  int
		want []string
	}{
		{10, []string{c, b, a}},
		{30, []string{c, b, a}}, // a retired 30 days ago: the grace still holds
		{31, []string{c, b}},
		{40, []string{c, b}},
		{41, []string{c}},
	} {
		e.now = start.Add(time.Duration(tt.day) * 24 * time.Hour)
		if got := fingerprints(keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))); !equal(got, tt.want) {
			t.Errorf("day %d: keyring %v, want %v", tt.day, got, tt.want)
		}
		signing, err := e.svc.Signing(ctx, e.repo)
		must(t, err)
		var got []string
		for _, s := range signing {
			got = append(got, s.Key.Fingerprint)
		}
		if !equal(got, tt.want) {
			t.Errorf("day %d: signing keys %v, want %v", tt.day, got, tt.want)
		}
	}
}

// TestRotateNoGrace rotates with a grace of zero, for a key that must stop being trusted at
// once: the retired key neither signs nor stays in key.asc.
func TestRotateNoGrace(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	must2(e.svc.KeyFor(ctx, e.repo))
	rotated := must2(e.svc.Rotate(ctx, e.repo)).Key.Fingerprint
	e.now = e.now.Add(time.Second)
	if got := fingerprints(keyring(t, must2(e.svc.PublicKeys(ctx, e.repo)))); !equal(got, []string{rotated}) {
		t.Errorf("keyring %v, want the new key alone", got)
	}
	if got := issuers(t, must2(e.svc.DetachSign(ctx, e.repo, []byte(release)))); !equal(got, []string{rotated}) {
		t.Errorf("Release.gpg signed by %v, want the new key alone", got)
	}
}

// TestNewMasterKey replaces a master key that is lost: the keys made under the old one do not
// decrypt, a rotation under the new one works, and the repo then signs with the new key alone;
// the retired key, which does not decrypt, is left out of the signatures and stays in key.asc
// for the grace.
func TestNewMasterKey(t *testing.T) {
	e := newEnv(t, Options{RetireGrace: time.Hour})
	ctx := t.Context()
	lost := must2(e.svc.KeyFor(ctx, e.repo)).Key.Fingerprint
	svc, err := New(Options{Index: e.st, Master: master(t, otherMasterHex), RetireGrace: time.Hour, Now: e.svc.now, Log: e.svc.log})
	must(t, err)
	if _, err := svc.ClearSign(ctx, e.repo, []byte(release)); err == nil {
		t.Fatal("ClearSign with a key of another master key succeeded")
	}
	rotated := must2(svc.Rotate(ctx, e.repo)).Key.Fingerprint
	in := must2(svc.ClearSign(ctx, e.repo, []byte(release)))
	if got := issuers(t, in); !equal(got, []string{rotated}) {
		t.Errorf("InRelease signed by %v, want the new key alone", got)
	}
	kr := keyring(t, must2(svc.PublicKeys(ctx, e.repo)))
	if got := fingerprints(kr); !equal(got, []string{rotated, lost}) {
		t.Errorf("keyring %v, want the new key and the retired one", got)
	}
	if _, err := verifyClear(t, in, kr); err != nil {
		t.Errorf("InRelease against key.asc: %v", err)
	}
}

// TestRotateFirst rotates a repo that has no key: it gets its first, and the audit row names
// no retired key.
func TestRotateFirst(t *testing.T) {
	e := newEnv(t, Options{})
	k, err := e.svc.Rotate(t.Context(), e.repo)
	must(t, err)
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: ActionRotate})
	must(t, err)
	if len(evs) != 1 || evs[0].Subject != k.Key.Fingerprint || evs[0].Details != nil || evs[0].Principal != "anonymous" {
		t.Errorf("audit rows %+v", evs)
	}
}

// raceStore makes the first two GetKey calls wait for each other, so that two KeyFor on a new
// repo both find no key and both make one.
type raceStore struct {
	index.Store
	calls *atomic.Int32
	both  chan struct{}
}

func (s raceStore) GetKey(ctx context.Context, repoID *int64, kind string) (*index.Key, error) {
	k, err := s.Store.GetKey(ctx, repoID, kind)
	if n := s.calls.Add(1); n <= 2 {
		if n == 2 {
			close(s.both)
		}
		<-s.both
	}
	return k, err
}

// TestKeyForRace runs two KeyFor at once on a repo without a key: both make a key, the unique
// index keeps one, and both return it; the repo has one key.
func TestKeyForRace(t *testing.T) {
	e := newEnv(t, Options{})
	race := raceStore{Store: e.st, calls: &atomic.Int32{}, both: make(chan struct{})}
	svc, err := New(Options{Index: race, Master: e.svc.master, Now: e.svc.now, Log: e.svc.log})
	must(t, err)
	var wg sync.WaitGroup
	got := make([]*Entity, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { got[i], errs[i] = svc.KeyFor(t.Context(), e.repo) })
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	if got[0].Key.Fingerprint != got[1].Key.Fingerprint {
		t.Errorf("the two KeyFor returned %s and %s", got[0], got[1])
	}
	rows, err := e.st.ListKeys(t.Context(), &e.repo.ID)
	must(t, err)
	if len(rows) != 1 || race.calls.Load() != 3 {
		t.Errorf("%d keys and %d GetKey calls, want 1 key and 3 calls (the loser reads the winner's key)", len(rows), race.calls.Load())
	}
}

// TestDecryptFails checks the keys that do not decrypt: another master key, a row whose
// fingerprint was changed, and a row whose ciphertext is of another key or of no key at all.
func TestDecryptFails(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	key, err := e.svc.KeyFor(ctx, e.repo)
	must(t, err)
	other, err := New(Options{Index: e.st, Master: master(t, otherMasterHex), Log: e.svc.log})
	must(t, err)
	for name, call := range map[string]func() error{
		"KeyFor":     func() error { _, err := other.KeyFor(ctx, e.repo); return err },
		"ClearSign":  func() error { _, err := other.ClearSign(ctx, e.repo, []byte(release)); return err },
		"DetachSign": func() error { _, err := other.DetachSign(ctx, e.repo, []byte(release)); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "does not decrypt with the master key") {
			t.Errorf("%s with another master key: %v", name, err)
		}
	}

	row := key.Key
	changed := row
	changed.Fingerprint = strings.Repeat("A", 40)
	if _, err := e.svc.decrypt(changed); err == nil || !strings.Contains(err.Error(), "does not decrypt") {
		t.Errorf("decrypt of a row with another fingerprint: %v", err)
	}
	b, _, err := e.svc.generate(e.repo)
	must(t, err)
	plain, err := e.svc.master.open(b.Fingerprint, b.PrivateEnc)
	must(t, err)
	swapped := row
	swapped.PrivateEnc = e.svc.master.seal(row.Fingerprint, plain)
	if _, err := e.svc.decrypt(swapped); err == nil || !strings.Contains(err.Error(), "holds the key "+b.Fingerprint) {
		t.Errorf("decrypt of a row with the private key of another: %v", err)
	}
	garbage := row
	garbage.PrivateEnc = e.svc.master.seal(row.Fingerprint, []byte("not a key"))
	if _, err := e.svc.decrypt(garbage); err == nil {
		t.Error("decrypt of a row whose private key is no OpenPGP packet succeeded")
	}
	public := row
	public.PrivateEnc = e.svc.master.seal(row.Fingerprint, row.Public)
	if _, err := e.svc.decrypt(public); err == nil || !strings.Contains(err.Error(), "no usable private key") {
		t.Errorf("decrypt of a row whose private key is the public key: %v", err)
	}
}

// failStore fails the calls it names with errBoom.
type failStore struct {
	index.Store
	fail map[string]bool
}

var errBoom = errors.New("boom")

func (s failStore) GetKey(ctx context.Context, repoID *int64, kind string) (*index.Key, error) {
	if s.fail["GetKey"] {
		return nil, errBoom
	}
	return s.Store.GetKey(ctx, repoID, kind)
}

func (s failStore) CreateKey(ctx context.Context, k *index.Key) error {
	if s.fail["CreateKey"] {
		return errBoom
	}
	return s.Store.CreateKey(ctx, k)
}

func (s failStore) ListKeys(ctx context.Context, repoID *int64) ([]index.Key, error) {
	if s.fail["ListKeys"] {
		return nil, errBoom
	}
	return s.Store.ListKeys(ctx, repoID)
}

func (s failStore) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(failWriter{Writer: w, fail: s.fail}) })
}

// failWriter is a transaction of failStore.
type failWriter struct {
	index.Writer
	fail map[string]bool
}

func (w failWriter) GetKey(ctx context.Context, repoID *int64, kind string) (*index.Key, error) {
	if w.fail["tx.GetKey"] {
		return nil, errBoom
	}
	return w.Writer.GetKey(ctx, repoID, kind)
}

func (w failWriter) InvalidateIndexCache(ctx context.Context, repoID int64, scope string) error {
	if w.fail["tx.InvalidateIndexCache"] {
		return errBoom
	}
	return w.Writer.InvalidateIndexCache(ctx, repoID, scope)
}

func (w failWriter) AppendAudit(ctx context.Context, ev *index.AuditEvent) error {
	if w.fail["tx.AppendAudit"] {
		return errBoom
	}
	return w.Writer.AppendAudit(ctx, ev)
}

// TestStoreFails checks that an error of the index reaches the caller, wrapped, from every
// call, and that a rotation that fails changes nothing.
func TestStoreFails(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	before := must2(e.svc.KeyFor(ctx, e.repo))
	fresh := index.Repo{Format: "apt", Name: "fresh", Registry: "local", Kind: "hosted"}
	must(t, e.st.CreateRepo(ctx, &fresh))
	for _, tt := range []struct {
		fail string
		repo index.Repo
		call func(*Service, index.Repo) error
	}{
		{"GetKey", e.repo, func(s *Service, r index.Repo) error { _, err := s.KeyFor(ctx, r); return err }},
		{"CreateKey", fresh, func(s *Service, r index.Repo) error { _, err := s.KeyFor(ctx, r); return err }},
		{"ListKeys", e.repo, func(s *Service, r index.Repo) error { _, err := s.PublicKeys(ctx, r); return err }},
		{"ListKeys", e.repo, func(s *Service, r index.Repo) error { _, err := s.ClearSign(ctx, r, nil); return err }},
		{"ListKeys", e.repo, func(s *Service, r index.Repo) error { _, err := s.DetachSign(ctx, r, nil); return err }},
		{"tx.GetKey", e.repo, func(s *Service, r index.Repo) error { _, err := s.Rotate(ctx, r); return err }},
		{"tx.InvalidateIndexCache", e.repo, func(s *Service, r index.Repo) error { _, err := s.Rotate(ctx, r); return err }},
		{"tx.AppendAudit", e.repo, func(s *Service, r index.Repo) error { _, err := s.Rotate(ctx, r); return err }},
	} {
		svc, err := New(Options{Index: failStore{Store: e.st, fail: map[string]bool{tt.fail: true}}, Master: e.svc.master, Log: e.svc.log})
		must(t, err)
		if err := tt.call(svc, tt.repo); !errors.Is(err, errBoom) {
			t.Errorf("with %s failing: %v, want it wrapped", tt.fail, err)
		}
	}
	after := must2(e.svc.KeyFor(ctx, e.repo))
	keys := must2(e.st.ListKeys(ctx, &e.repo.ID))
	if after.Key.ID != before.Key.ID || len(keys) != 1 {
		t.Errorf("after failed rotations: the key %s, %d keys; want %s alone", after, len(keys), before)
	}
	if _, err := e.svc.KeyFor(ctx, index.Repo{ID: fresh.ID + 100, Format: "apt", Name: "gone"}); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("KeyFor of a repo that does not exist: %v, want index.ErrNotFound", err)
	}
}

// conflictWriter makes CreateKey in a transaction lose to a concurrent rotation.
type conflictStore struct{ index.Store }

func (s conflictStore) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(conflictWriter{w}) })
}

type conflictWriter struct{ index.Writer }

func (conflictWriter) CreateKey(context.Context, *index.Key) error {
	return fmt.Errorf("%w: keys gpg exists", index.ErrConflict)
}

// TestRotateConflict checks a rotation that loses to another: index.ErrConflict, and the key
// it would have retired is still the active one.
func TestRotateConflict(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := t.Context()
	before := must2(e.svc.KeyFor(ctx, e.repo))
	svc, err := New(Options{Index: conflictStore{e.st}, Master: e.svc.master, Log: e.svc.log})
	must(t, err)
	if _, err := svc.Rotate(ctx, e.repo); !errors.Is(err, index.ErrConflict) {
		t.Errorf("Rotate that loses: %v, want index.ErrConflict", err)
	}
	if after := must2(e.svc.KeyFor(ctx, e.repo)); after.Key.ID != before.Key.ID {
		t.Errorf("the active key is %s after a rotation that failed, want %s", after, before)
	}
}

// rotatedStore lists the keys as a rotation by another replica between GetKey and ListKeys
// leaves them for this one's read: the key GetKey found is retired in ListKeys.
type rotatedStore struct{ index.Store }

func (s rotatedStore) ListKeys(ctx context.Context, repoID *int64) ([]index.Key, error) {
	keys, err := s.Store.ListKeys(ctx, repoID)
	for i := range keys {
		at := time.Now()
		keys[i].RetiredAt = &at
	}
	return keys, err
}

// TestKeyringRotatedMeanwhile checks the read of a keyring that a rotation interleaved with:
// an error that says so, rather than a keyring without an active key.
func TestKeyringRotatedMeanwhile(t *testing.T) {
	e := newEnv(t, Options{})
	svc, err := New(Options{Index: rotatedStore{e.st}, Master: e.svc.master, Log: e.svc.log})
	must(t, err)
	if _, err := svc.PublicKeys(t.Context(), e.repo); err == nil || !strings.Contains(err.Error(), "rotated meanwhile") {
		t.Errorf("PublicKeys: %v, want the rotation named", err)
	}
}

// TestEntityRedacted checks that a key prints as its fingerprint, in every verb and in a log
// line.
func TestEntityRedacted(t *testing.T) {
	e := newEnv(t, Options{})
	k := must2(e.svc.KeyFor(t.Context(), e.repo))
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("key", "key", k)
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k), logged.String()} {
		if !strings.Contains(s, k.Key.Fingerprint) || strings.Contains(s, "PrivateKey") {
			t.Errorf("a key prints as %q", s)
		}
	}
}

func equal(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// jsonEqual reports whether raw is the JSON value of want.
func jsonEqual(raw json.RawMessage, want string) bool {
	var a, b any
	return json.Unmarshal(raw, &a) == nil && json.Unmarshal([]byte(want), &b) == nil && fmt.Sprint(a) == fmt.Sprint(b)
}
