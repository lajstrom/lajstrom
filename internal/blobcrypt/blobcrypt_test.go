package blobcrypt

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/opencontainers/go-digest"
)

// fixed returns options of a fixed key and a nonce whose low half overflows within 7 blocks.
func fixed(t *testing.T) Private {
	t.Helper()
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	nonce, _ := hex.DecodeString("0102030405060708fffffffffffffffe")
	return Private{Key: key, CipherOptions: map[string][]byte{nonceOption: nonce}}
}

func plainOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// TestOcicryptVector checks the cipher against a blob that containers/ocicrypt v1.3.2 encrypted
// (blockcipher.AESCTRLayerBlockCipher) with the key, nonce and plaintext of fixed and plainOf(100).
func TestOcicryptVector(t *testing.T) {
	const (
		wantCT  = "4859f09168445629c7e2d36c6b41a98597a8fd7052b4c4ba071eb8d7d30d2ff15eaea91fbd9ac04bd77ba4db4c0d8f3f311901ee875949b7ec100d8387230ac2e1af05c9a49a016282aea671f3e4bb3a98e39ee9563b8b5dcc7e9f826e18c608afa38cac"
		wantMAC = "11bbaf875795422c75e8e85d2655052fcc082d4491550ac16a2d802038127b56"
	)
	ct, mac, err := Seal(fixed(t), plainOf(100))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ct); got != wantCT {
		t.Errorf("ciphertext\n got %s\nwant %s", got, wantCT)
	}
	if got := hex.EncodeToString(mac); got != wantMAC {
		t.Errorf("hmac %s, want %s", got, wantMAC)
	}
}

// TestAt: the keystream from any offset is the rest of the keystream from 0, across the carry
// of the counter's low half.
func TestAt(t *testing.T) {
	p := fixed(t)
	full, _, _ := Seal(p, make([]byte, 200))
	for _, off := range []int64{0, 1, 15, 16, 17, 31, 32, 95, 96, 97, 199} {
		s, err := At(p, off)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 200-off)
		s.XORKeyStream(got, got)
		if !bytes.Equal(got, full[off:]) {
			t.Errorf("At(%d) is not the keystream from %d", off, off)
		}
	}
	if _, err := At(p, -1); err == nil {
		t.Error("At(-1): no error")
	}
	if _, err := At(Private{Key: []byte("short")}, 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("At with a short key: %v, want ErrInvalid", err)
	}
}

// TestStreams: EncryptReader and DecryptReader in reads of any size are Seal, and feed the
// HMAC with the ciphertext.
func TestStreams(t *testing.T) {
	p := NewPrivate()
	plain := plainOf(100_000)
	ct, mac, _ := Seal(p, plain)
	s, _ := At(p, 0)
	m := NewMAC(p)
	h := sha256.New()
	got, err := io.ReadAll(iotest.OneByteReader(EncryptReader(bytes.NewReader(plain), s, m, h)))
	if err != nil || !bytes.Equal(got, ct) || !bytes.Equal(m.Sum(nil), mac) {
		t.Fatalf("EncryptReader: %v, same ciphertext %v, same hmac %v", err, bytes.Equal(got, ct), bytes.Equal(m.Sum(nil), mac))
	}
	if digest.NewDigest(digest.SHA256, h) != digest.FromBytes(ct) {
		t.Error("the hash of EncryptReader is not the ciphertext's")
	}
	s, _ = At(p, 0)
	m = NewMAC(p)
	got, err = io.ReadAll(iotest.HalfReader(DecryptReader(bytes.NewReader(ct), s, m)))
	if err != nil || !bytes.Equal(got, plain) || !bytes.Equal(m.Sum(nil), mac) {
		t.Fatalf("DecryptReader: %v, same plaintext %v, same hmac %v", err, bytes.Equal(got, plain), bytes.Equal(m.Sum(nil), mac))
	}
}

// TestOpen: a whole blob comes back when it matches, and never whole when its ciphertext or
// its digest does not.
func TestOpen(t *testing.T) {
	for _, n := range []int{0, 1, 2, 32 << 10, 32<<10 + 1, 100_000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			p := NewPrivate()
			plain := plainOf(n)
			p.Digest = digest.FromBytes(plain)
			ct, mac, _ := Seal(p, plain)
			r, err := Open(bytes.NewReader(ct), p, mac)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("Open: %v, same plaintext %v", err, bytes.Equal(got, plain))
			}
			if n == 0 {
				return
			}
			changed := bytes.Clone(ct)
			changed[n-1] ^= 1
			wrongDigest := p
			wrongDigest.Digest = digest.FromString("other")
			for name, c := range map[string]struct {
				ct []byte
				p  Private
			}{"ciphertext": {changed, p}, "digest": {ct, wrongDigest}} {
				r, _ := Open(iotest.OneByteReader(bytes.NewReader(c.ct)), c.p, mac)
				got, err := io.ReadAll(r)
				if !errors.Is(err, ErrMismatch) {
					t.Errorf("%s changed: %v, want ErrMismatch", name, err)
				}
				if len(got) >= n {
					t.Errorf("%s changed: %d bytes of %d came out", name, len(got), n)
				}
			}
		})
	}
}

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var (
	keyOnce sync.Once
	key2048 *rsa.PrivateKey
)

func testKey(t *testing.T) *rsa.PrivateKey {
	keyOnce.Do(func() { key2048 = rsaKey(t, 2048) })
	return key2048
}

func TestWrapUnwrap(t *testing.T) {
	repoKey := testKey(t)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	repo, err := NewRecipient(&repoKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewRecipient(&ec.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPrivate()
	p.Digest = digest.FromString("file")
	value, err := Wrap(p, []Recipient{other, repo})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unwrap(value, repo.KID, RSAKey{Key: repoKey})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Key, p.Key) || !bytes.Equal(got.Nonce(), p.Nonce()) || got.Digest != p.Digest {
		t.Errorf("Unwrap = %v, want %v", got, p)
	}
	if _, err := Unwrap(value, "NOT-A-KID", RSAKey{Key: repoKey}); !errors.Is(err, ErrNoRecipient) {
		t.Errorf("another kid: %v, want ErrNoRecipient", err)
	}
	kmsDown := errors.New("kms unavailable")
	if _, err := Unwrap(value, repo.KID, failing{kmsDown}); !errors.Is(err, kmsDown) {
		t.Errorf("a failing decrypter: %v, want its error", err)
	}
	if _, err := Unwrap(value, repo.KID, RSAKey{Key: rsaKey(t, 2048)}); err == nil {
		t.Error("another private key: no error")
	}
	if _, err := Unwrap("not base64!", repo.KID, RSAKey{Key: repoKey}); !errors.Is(err, ErrInvalid) {
		t.Errorf("garbage: %v, want ErrInvalid", err)
	}
	if _, err := Wrap(Private{}, []Recipient{repo}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Wrap of empty options: %v, want ErrInvalid", err)
	}
	if _, err := Wrap(p, nil); err == nil {
		t.Error("Wrap without recipients: no error")
	}
	// ocicrypt joins wrappings with commas; the recipient may be in the second.
	other2, _ := Wrap(p, []Recipient{other})
	if _, err := Unwrap(other2+","+value, repo.KID, RSAKey{Key: repoKey}); err != nil {
		t.Errorf("two wrappings: %v", err)
	}
}

type failing struct{ err error }

func (f failing) DecryptOAEP([]byte) ([]byte, error) { return nil, f.err }

func pemOf(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestParseRecipient(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	ed, _, _ := ed25519.GenerateKey(rand.Reader)
	good := pemOf(t, &testKey(t).PublicKey)
	for name, c := range map[string]struct {
		text string
		ok   bool
	}{
		"rsa 2048":   {good, true},
		"ec p-384":   {pemOf(t, &ec.PublicKey), true},
		"rsa 1024":   {pemOf(t, &rsaKey(t, 1024).PublicKey), false},
		"ed25519":    {pemOf(t, ed), false},
		"two blocks": {good + good, false},
		"not pem":    {"hello", false},
		"private":    {strings.Replace(good, "PUBLIC KEY", "PRIVATE KEY", 2), false},
	} {
		r, err := ParseRecipient(c.text)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok %v", name, err, c.ok)
		}
		if c.ok && len(r.KID) != 64 {
			t.Errorf("%s: kid %q", name, r.KID)
		}
	}
}

func TestPublic(t *testing.T) {
	mac := bytes.Repeat([]byte{7}, 32)
	got, err := DecodePublic(EncodePublic(mac))
	if err != nil || !bytes.Equal(got, mac) {
		t.Fatalf("DecodePublic(EncodePublic) = %x, %v", got, err)
	}
	for _, bad := range []string{"%%", "bm90IGpzb24=", "eyJjaXBoZXIiOiJBRVNfMjU2X0dDTSIsImhtYWMiOiIifQ=="} {
		if _, err := DecodePublic(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("DecodePublic(%q): %v, want ErrInvalid", bad, err)
		}
	}
}

func TestPrivateNeverPrinted(t *testing.T) {
	p := NewPrivate()
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%v %+v %#v", p, p, p)} {
		if strings.Contains(s, fmt.Sprintf("%v", p.Key)) || strings.Contains(s, hex.EncodeToString(p.Key)) {
			t.Errorf("the key is printed: %s", s)
		}
	}
	if !Encrypted("application/vnd.lajstrom.raw.file.v1+encrypted") || Encrypted("application/vnd.lajstrom.raw.file.v1") {
		t.Error("Encrypted")
	}
}
