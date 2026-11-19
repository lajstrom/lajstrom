package keys_test

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/keystest"
)

// The data keys of keys.masterKeyKMS (#1388; docs/spec/index-schema.md, A master key in a KMS).

// fileKey is a master key of a file of the tests.
var fileKey = strings.Repeat("0123456789abcdef", 4)

// dataKey is a data key of the tests: its 32 bytes in hex, its id, its row under kms.
type dataKey struct {
	hex, id string
	row     index.WrappedMasterKey
}

// newDataKey makes a data key and its row, wrapped by the first key of kms under its own id.
func newDataKey(t *testing.T, kms *keystest.WrapKMS) dataKey {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	d := dataKey{hex: hex.EncodeToString(b)}
	d.id = mustID(t, d.hex)
	wrapped, err := kms.Encrypt(t.Context(), kms.ARN, b, keys.MasterAD(d.id))
	if err != nil {
		t.Fatal(err)
	}
	d.row = index.WrappedMasterKey{KeyID: d.id, KMS: kms.URI(), Wrapped: wrapped}
	return d
}

func mustID(t *testing.T, hex string) string {
	t.Helper()
	id, err := keys.MasterID(hex)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// kmsKey is the key URI of kms's first key, taken apart.
func kmsKey(t *testing.T, kms *keystest.WrapKMS) config.KMSKey {
	t.Helper()
	k, err := config.ParseMasterKMSKey(kms.URI())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// noKeyIn fails the test when err shows one of secrets, as it is or in base64.
func noKeyIn(t *testing.T, err error, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if err != nil && (strings.Contains(err.Error(), s) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(s)))) {
			t.Errorf("the error %q shows a key or a ciphertext", err)
		}
	}
}

// pendingMaster returns a Master of the file key and the data key d, pending, which the index
// names to seal, and a value sealed under d.
func pendingMaster(t *testing.T, d dataKey) (*keys.Master, []byte) {
	t.Helper()
	m, err := keys.HoldMasterKeys([]keys.MasterSource{{Source: "file", Text: fileKey}}, []string{d.id})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Pending(); len(got) != 1 || got[0] != d.id || m.Holds(d.id) || !m.IsPending(d.id) {
		t.Fatalf("Pending = %v, Holds %t: want the data key pending, not held", got, m.Holds(d.id))
	}
	under, err := keys.ParseMaster(d.hex)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := under.Seal("webhook:1", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	m.SealWith(d.id)
	return m, sealed
}

// TestUnwrapPending: a data key configured is pending, not held: while the index names it to
// seal, the Master seals nothing, with a SealingError that says it is not unwrapped yet (503,
// Retry-After 30), and a value under it fails as an unavailable KMS (503, Retry-After 5).
func TestUnwrapPending(t *testing.T) {
	d := newDataKey(t, keystest.NewWrapKMS())
	m, sealed := pendingMaster(t, d)
	if m.SealWith(d.id) {
		t.Fatal("SealWith of a pending key: held")
	}
	var se *keys.SealingError
	var busy *format.UnavailableError
	_, err := m.Seal("webhook:2", []byte("x"))
	if !errors.As(err, &se) || !se.Pending || !strings.Contains(err.Error(), "has not unwrapped yet") ||
		!errors.As(err, &busy) || busy.RetryAfter != keys.SealRetryAfter {
		t.Errorf("Seal while the sealing key is pending: %v, want a SealingError of a pending key, Retry-After 30s", err)
	}
	_, err = m.Open("webhook:1", sealed)
	if !errors.Is(err, keys.ErrKMSUnavailable) || !errors.As(err, &busy) || busy.RetryAfter != 5*time.Second ||
		!strings.Contains(err.Error(), d.id) {
		t.Errorf("Open of a value under the pending key: %v, want the KMS unavailable, Retry-After 5s, naming the id", err)
	}
}

// TestUnwrap: Unwrap holds a data key, under its KMS key, and it opens and seals; a second Unwrap
// asks the KMS nothing.
func TestUnwrap(t *testing.T) {
	kms := keystest.NewWrapKMS()
	d := newDataKey(t, kms)
	m, sealed := pendingMaster(t, d)
	if err := m.Unwrap(t.Context(), kms, kmsKey(t, kms), d.row); err != nil {
		t.Fatal(err)
	}
	if !m.Holds(d.id) || len(m.Pending()) != 0 || fmt.Sprint(m.Held()) != fmt.Sprint(heldOf(t, d.id, kms.URI())) {
		t.Errorf("after the unwrap: Held %v, Pending %v; want the data key held under %s", m.Held(), m.Pending(), kms.URI())
	}
	if got, err := m.Open("webhook:1", sealed); err != nil || string(got) != "secret" {
		t.Errorf("Open after the unwrap = %q, %v", got, err)
	}
	if !m.SealWith(d.id) {
		t.Error("SealWith of the unwrapped key: not held")
	}
	if _, err := m.Seal("webhook:2", []byte("x")); err != nil {
		t.Errorf("Seal with the unwrapped key: %v", err)
	}
	n := kms.Decrypts()
	if err := m.Unwrap(t.Context(), kms, kmsKey(t, kms), d.row); err != nil || kms.Decrypts() != n {
		t.Errorf("a second Unwrap: %v, %d unwraps asked; want none", err, kms.Decrypts()-n)
	}
}

// heldOf is what Held answers of the file key with the key id from source.
func heldOf(t *testing.T, id, source string) []keys.HeldKey {
	t.Helper()
	out := []keys.HeldKey{{ID: mustID(t, fileKey), Source: "file"}, {ID: id, Source: source}}
	if out[1].ID < out[0].ID {
		out[0], out[1] = out[1], out[0]
	}
	return out
}

// TestUnwrapOtherID is K-5 of the threat model (#1388): a row changed by someone who could write
// the database and may encrypt with the KMS key, to another key wrapped under the entry's id, is
// never held: the KMS's answer has another id, and Unwrap is ErrOtherDataKey; an answer of
// another length too. A ciphertext of another id, moved to the row, does not open (the additional
// data binds the id), and a row of another KMS key asks nothing. The key stays pending, and no
// error shows a key or a ciphertext.
func TestUnwrapOtherID(t *testing.T) {
	kms := keystest.NewWrapKMS()
	d, planted := newDataKey(t, kms), newDataKey(t, kms)
	m, err := keys.HoldMasterKeys(nil, []string{d.id})
	if err != nil {
		t.Fatal(err)
	}
	other, err := hex.DecodeString(planted.hex)
	if err != nil {
		t.Fatal(err)
	}
	asD, err := kms.Encrypt(t.Context(), kms.ARN, other, keys.MasterAD(d.id))
	if err != nil {
		t.Fatal(err)
	}
	short, err := kms.Encrypt(t.Context(), kms.ARN, other[:16], keys.MasterAD(d.id))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		what    string
		row     index.WrappedMasterKey
		other   bool // ErrOtherDataKey
		decrypt bool // the KMS is asked
	}{
		{"another key wrapped under the entry's id", index.WrappedMasterKey{KeyID: d.id, KMS: kms.URI(), Wrapped: asD}, true, true},
		{"16 bytes wrapped under the entry's id", index.WrappedMasterKey{KeyID: d.id, KMS: kms.URI(), Wrapped: short}, true, true},
		{"the ciphertext of another id", index.WrappedMasterKey{KeyID: d.id, KMS: kms.URI(), Wrapped: planted.row.Wrapped}, false, true},
		{"a row of another KMS key", index.WrappedMasterKey{KeyID: d.id, KMS: "awskms:///arn:aws:kms:eu-west-1:111122223333:key/x",
			Wrapped: d.row.Wrapped}, false, false},
	} {
		n := kms.Decrypts()
		err := m.Unwrap(t.Context(), kms, kmsKey(t, kms), tt.row)
		switch {
		case err == nil:
			t.Errorf("%s: held", tt.what)
		case errors.Is(err, keys.ErrOtherDataKey) != tt.other:
			t.Errorf("%s: %v; ErrOtherDataKey %t, want %t", tt.what, err, !tt.other, tt.other)
		case (kms.Decrypts() > n) != tt.decrypt:
			t.Errorf("%s: the KMS asked %t, want %t", tt.what, kms.Decrypts() > n, tt.decrypt)
		}
		if m.Holds(d.id) || m.Holds(planted.id) || !m.IsPending(d.id) || len(m.IDs()) != 0 {
			t.Errorf("%s: Held %v, Pending %v; want nothing held, the entry pending", tt.what, m.Held(), m.Pending())
		}
		noKeyIn(t, err, d.hex, planted.hex, string(other), string(asD), string(d.row.Wrapped))
	}
}

// TestHoldMasterKeysPending: a data key not yet unwrapped counts among the keys, against
// MaxMasterKeys and against none; one whose id a file holds is not pending.
func TestHoldMasterKeysPending(t *testing.T) {
	if m, err := keys.HoldMasterKeys(nil, []string{"0123456789abcdef"}); err != nil || len(m.IDs()) != 0 || len(m.Pending()) != 1 {
		t.Errorf("a data key alone: %v, %v", m, err)
	}
	if _, err := keys.HoldMasterKeys(nil, nil); !errors.Is(err, keys.ErrNoMasterKey) {
		t.Errorf("no key: %v, want ErrNoMasterKey", err)
	}
	var srcs []keys.MasterSource
	for i := range keys.MaxMasterKeys {
		srcs = append(srcs, keys.MasterSource{Source: fmt.Sprint(i), Text: strings.Repeat(fmt.Sprintf("%02x", i), 32)})
	}
	if _, err := keys.HoldMasterKeys(srcs, []string{"0123456789abcdef"}); err == nil || !strings.Contains(err.Error(), "9 keys, more than 8") {
		t.Errorf("8 keys of files and a data key: %v, want an error of 9 keys", err)
	}
	m, err := keys.HoldMasterKeys(srcs[:1], []string{mustID(t, srcs[0].Text), "0123456789abcdef"})
	if err != nil || len(m.Pending()) != 1 || m.Pending()[0] != "0123456789abcdef" {
		t.Errorf("a data key whose id a file holds: %v, %v; want it held, the other pending", m.Pending(), err)
	}
}

// TestPolicyKeyPending: the policy key of a row under a data key not yet unwrapped is had all the
// same, its public key known, and signs nothing, as an unavailable KMS, until the unwrap; then it
// signs, and its signature verifies.
func TestPolicyKeyPending(t *testing.T) {
	ctx := t.Context()
	kms := keystest.NewWrapKMS()
	d := newDataKey(t, kms)
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	under, err := keys.ParseMaster(d.hex)
	if err != nil {
		t.Fatal(err)
	}
	made, err := mustService(t, st, under).PolicyKey(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := keys.HoldMasterKeys(nil, []string{d.id})
	if err != nil {
		t.Fatal(err)
	}
	m.SealWith(d.id)
	k, err := mustService(t, st, m).PolicyKey(ctx, "")
	if err != nil || k.Fingerprint() != made.Fingerprint() {
		t.Fatalf("the policy key while its master key is pending: %v, %v; want the row's", k, err)
	}
	if _, err := k.Sign(ctx, []byte("verdict")); !errors.Is(err, keys.ErrKMSUnavailable) {
		t.Errorf("a signature before the unwrap: %v, want the KMS unavailable", err)
	}
	if err := m.Unwrap(ctx, kms, kmsKey(t, kms), d.row); err != nil {
		t.Fatal(err)
	}
	sig, err := k.Sign(ctx, []byte("verdict"))
	digest := sha256.Sum256([]byte("verdict"))
	if pub, ok := k.Public.(*ecdsa.PublicKey); err != nil || !ok || !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Errorf("a signature after the unwrap: %v, or it does not verify", err)
	}
}

// mustService returns the keys service of st under m.
func mustService(t *testing.T, st index.Store, m *keys.Master) *keys.Service {
	t.Helper()
	svc, err := keys.New(keys.Options{Index: st, Master: m})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}
