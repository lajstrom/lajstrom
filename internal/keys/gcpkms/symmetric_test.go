package gcpkms

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/keys"
)

const testKey = "projects/p/locations/europe-west1/keyRings/lajstrom/cryptoKeys/master"

// testAD is the additional data of the master key 0123456789abcdef (#1388).
var testAD = keys.AdditionalData{Key: "lajstrom", Value: "master key 0123456789abcdef"}

// symmetricKMS is Cloud KMS's REST API for one symmetric key, testKey, AES-256-GCM in memory: a
// ciphertext is the version that made it, a 0 byte, the nonce and the sealed bytes, the
// additional authenticated data as GCM's. It checks the CRC32C of every field it is sent.
type symmetricKMS struct {
	t *testing.T

	mu      sync.Mutex
	key     map[string]any // the CryptoKey of cryptoKeys.get, which a test may change
	aead    cipher.AEAD
	version string // the primary version, which encrypts
	fail    []int
	refuse  string
	badCRC  bool // answers a ciphertext or a plaintext with a CRC32C of other bytes
	aads    []string
}

func newSymmetricKMS(t *testing.T) (*symmetricKMS, *Client) {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	block, _ := aes.NewCipher(b)
	aead, _ := cipher.NewGCM(block)
	f := &symmetricKMS{t: t, aead: aead, version: testKey + "/cryptoKeyVersions/1",
		key: map[string]any{"name": testKey, "purpose": "ENCRYPT_DECRYPT",
			"primary": map[string]any{"name": testKey + "/cryptoKeyVersions/1", "state": "ENABLED", "algorithm": "GOOGLE_SYMMETRIC_ENCRYPTION"}}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New(Options{Endpoint: srv.URL, Log: slog.New(slog.DiscardHandler),
		Token: func(context.Context) (string, error) { return "token-1", nil }})
	c.backoff = time.Millisecond
	return f, c
}

// symmetricIn is the body of an encrypt or a decrypt.
type symmetricIn struct {
	Plaintext, Ciphertext, AdditionalAuthenticatedData                   []byte
	PlaintextCrc32c, CiphertextCrc32c, AdditionalAuthenticatedDataCrc32c string
}

func (f *symmetricKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer token-1" {
		apiError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "no token")
		return
	}
	if len(f.fail) > 0 {
		status := f.fail[0]
		f.fail = f.fail[1:]
		apiError(w, status, "UNAVAILABLE", "try again")
		return
	}
	if f.refuse != "" {
		apiError(w, http.StatusForbidden, "PERMISSION_DENIED", f.refuse)
		return
	}
	var in symmetricIn
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.AdditionalAuthenticatedDataCrc32c != crc(in.AdditionalAuthenticatedData) {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "additionalAuthenticatedDataCrc32c")
			return
		}
		f.aads = append(f.aads, string(in.AdditionalAuthenticatedData))
	}
	switch path := strings.TrimPrefix(r.URL.Path, "/v1/"); {
	case r.Method == http.MethodGet && path == testKey:
		_ = json.NewEncoder(w).Encode(f.key)
	case r.Method == http.MethodPost && path == testKey+":encrypt" && in.PlaintextCrc32c == crc(in.Plaintext):
		f.encrypt(w, in)
	case r.Method == http.MethodPost && path == testKey+":decrypt" && in.CiphertextCrc32c == crc(in.Ciphertext):
		f.decrypt(w, in)
	default:
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "no "+r.Method+" "+path+" with these CRC32C")
	}
}

// sum is the CRC32C of b, or of other bytes while badCRC is set.
func (f *symmetricKMS) sum(b []byte) string {
	if f.badCRC {
		return crc(append(b, 0))
	}
	return crc(b)
}

// encrypt answers encrypt: the version, a 0 byte, the nonce and the sealed bytes.
func (f *symmetricKMS) encrypt(w http.ResponseWriter, in symmetricIn) {
	nonce := make([]byte, f.aead.NonceSize())
	_, _ = rand.Read(nonce)
	c := f.aead.Seal(append(append([]byte(f.version), 0), nonce...), nonce, in.Plaintext, in.AdditionalAuthenticatedData)
	_ = json.NewEncoder(w).Encode(map[string]any{"name": f.version, "ciphertext": c, "ciphertextCrc32c": f.sum(c),
		"verifiedPlaintextCrc32c": true, "verifiedAdditionalAuthenticatedDataCrc32c": true, "protectionLevel": "HSM"})
}

// decrypt answers decrypt: the plaintext of what encrypt answered, with the same data.
func (f *symmetricKMS) decrypt(w http.ResponseWriter, in symmetricIn) {
	_, rest, _ := bytes.Cut(in.Ciphertext, []byte{0})
	if len(rest) < f.aead.NonceSize() {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Decryption failed: the ciphertext is invalid.")
		return
	}
	p, err := f.aead.Open(nil, rest[:f.aead.NonceSize()], rest[f.aead.NonceSize():], in.AdditionalAuthenticatedData)
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Decryption failed: verify that 'name' refers to the correct CryptoKey.")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"plaintext": p, "plaintextCrc32c": f.sum(p), "usedPrimary": true,
		"protectionLevel": "HSM"})
}

// TestSymmetricKey: cryptoKeys.get of a key without a version answers its name, and a key whose
// purpose or primary version cannot wrap is ErrKMSKeyCannotWrap, naming them.
func TestSymmetricKey(t *testing.T) {
	f, c := newSymmetricKMS(t)
	if name, err := c.SymmetricKey(t.Context(), testKey); err != nil || name != testKey {
		t.Fatalf("SymmetricKey = %q, %v; want the key's name", name, err)
	}
	for _, tt := range []struct {
		key  map[string]any
		want string
	}{
		{map[string]any{"purpose": "ASYMMETRIC_DECRYPT"}, "purpose ASYMMETRIC_DECRYPT, not ENCRYPT_DECRYPT"},
		{map[string]any{"purpose": "ENCRYPT_DECRYPT"}, "has no primary version"},
		{map[string]any{"purpose": "ENCRYPT_DECRYPT", "primary": map[string]any{"state": "ENABLED", "algorithm": "AES_256_GCM"}},
			"of AES_256_GCM, not GOOGLE_SYMMETRIC_ENCRYPTION"},
		{map[string]any{"purpose": "ENCRYPT_DECRYPT", "primary": map[string]any{"state": "DISABLED",
			"algorithm": "GOOGLE_SYMMETRIC_ENCRYPTION"}}, "is DISABLED, not ENABLED"},
	} {
		f.mu.Lock()
		f.key = tt.key
		f.mu.Unlock()
		if _, err := c.SymmetricKey(t.Context(), testKey); !errors.Is(err, keys.ErrKMSKeyCannotWrap) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("SymmetricKey of %v: %v, want ErrKMSKeyCannotWrap naming %q", tt.key, err, tt.want)
		}
	}
}

// wrapOnce wraps 32 bytes under testAD with c, and returns them and the ciphertext.
func wrapOnce(t *testing.T, c *Client) (plain, wrapped []byte) {
	t.Helper()
	plain = bytes.Repeat([]byte{0x5a}, 32)
	wrapped, err := c.Encrypt(t.Context(), testKey, plain, testAD)
	if err != nil {
		t.Fatal(err)
	}
	return plain, wrapped
}

// noSecret fails the test when err holds one of the byte strings of secrets, as they are or in
// base64.
func noSecret(t *testing.T, what string, err error, secrets ...[]byte) {
	t.Helper()
	for _, s := range secrets {
		if err != nil && (strings.Contains(err.Error(), string(s)) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(s))) {
			t.Errorf("%s: the error %q holds the key or the ciphertext", what, err)
		}
	}
}

// TestEncryptDecryptSymmetric: what encrypt wraps under additional data, decrypt unwraps under
// it, the data "lajstrom master key <id>" with its CRC32C, and under other data the KMS refuses
// it; no error holds the key or the ciphertext.
func TestEncryptDecryptSymmetric(t *testing.T) {
	f, c := newSymmetricKMS(t)
	ctx := t.Context()
	plain, wrapped := wrapOnce(t, c)
	if got, err := c.DecryptSymmetric(ctx, testKey, wrapped, testAD); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("DecryptSymmetric = %x, %v; want the plaintext", got, err)
	}
	for _, aad := range f.aads {
		if aad != "lajstrom master key 0123456789abcdef" {
			t.Errorf("the additional data %q, want lajstrom master key 0123456789abcdef", aad)
		}
	}
	_, err := c.DecryptSymmetric(ctx, testKey, wrapped, keys.AdditionalData{Key: "lajstrom", Value: "master key fedcba9876543210"})
	if err == nil || errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "Decryption failed") {
		t.Errorf("DecryptSymmetric under other data: %v, want the KMS's refusal", err)
	}
	noSecret(t, "other data", err, plain, wrapped)
}

// TestSymmetricUnavailable: a CRC32C that does not match is unavailable, and no plaintext; a KMS
// that does not answer is asked again, then unavailable; a refusal is not unavailable; no error
// holds the key or the ciphertext.
func TestSymmetricUnavailable(t *testing.T) {
	f, c := newSymmetricKMS(t)
	ctx := t.Context()
	plain, wrapped := wrapOnce(t, c)
	f.mu.Lock()
	f.badCRC = true
	f.mu.Unlock()
	if _, err := c.Encrypt(ctx, testKey, plain, testAD); !errors.Is(err, keys.ErrKMSUnavailable) {
		t.Errorf("Encrypt answered with a CRC32C of other bytes: %v, want ErrKMSUnavailable", err)
	}
	p, err := c.DecryptSymmetric(ctx, testKey, wrapped, testAD)
	if p != nil || !errors.Is(err, keys.ErrKMSUnavailable) {
		t.Errorf("DecryptSymmetric answered with a CRC32C of other bytes: %x, %v; want no plaintext, ErrKMSUnavailable", p, err)
	}
	noSecret(t, "a CRC32C of other bytes", err, plain, wrapped)

	f.mu.Lock()
	f.badCRC, f.fail = false, []int{503, 503}
	f.mu.Unlock()
	if got, err := c.DecryptSymmetric(ctx, testKey, wrapped, testAD); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("DecryptSymmetric after two unavailable answers: %v, want it asked again", err)
	}
	f.mu.Lock()
	f.fail = []int{503, 503, 503}
	f.mu.Unlock()
	if _, err := c.DecryptSymmetric(ctx, testKey, wrapped, testAD); !errors.Is(err, keys.ErrKMSUnavailable) {
		t.Errorf("DecryptSymmetric of a KMS that does not answer: %v, want ErrKMSUnavailable", err)
	}
	f.mu.Lock()
	f.refuse = "Permission 'cloudkms.cryptoKeyVersions.useToDecrypt' denied"
	f.mu.Unlock()
	for _, err := range []error{second(c.Encrypt(ctx, testKey, plain, testAD)), second(c.DecryptSymmetric(ctx, testKey, wrapped, testAD))} {
		if err == nil || errors.Is(err, keys.ErrKMSUnavailable) || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
			t.Errorf("a refusal: %v, want PERMISSION_DENIED, not unavailable", err)
		}
		noSecret(t, "a refusal", err, plain, wrapped)
	}
}

// second returns the error of a call of two results.
func second[T any](_ T, err error) error { return err }
