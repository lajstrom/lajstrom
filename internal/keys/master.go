package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// masterFormat says what a master key is, for the errors about one. It never shows a value.
const masterFormat = "the master key is 64 hexadecimal characters (32 bytes), e.g. the output of openssl rand -hex 32"

// ErrNoMasterKey is the error of LoadMaster when no master key is configured: keys.masterKeyEnv
// is empty, or the variable it names is not set. serve then runs with signing off, unless the
// index holds signing keys that only the master key decrypts; admin key fails.
var ErrNoMasterKey = errors.New("keys: no master key")

// Master is the master key: AES-256-GCM over the private keys at rest. It is loaded once at
// start and never printed: String, GoString and LogValue show no key material, and the bytes
// themselves are not kept, only the cipher made from them.
type Master struct {
	aead cipher.AEAD
}

// LoadMaster reads the master key from the environment variable name, as keys.masterKeyEnv
// names it; lookupEnv is os.LookupEnv outside tests. No name, or a variable that is not set,
// is an error wrapping ErrNoMasterKey. A variable that is set but empty, or is not 64
// hexadecimal characters, is an error that names the variable and the format, never the value.
// Spaces and line breaks around the value are ignored, as a file mounted from a secret ends
// with one.
func LoadMaster(name string, lookupEnv func(string) (string, bool)) (*Master, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: keys.masterKeyEnv is not set", ErrNoMasterKey)
	}
	v, ok := lookupEnv(name)
	if !ok {
		return nil, fmt.Errorf("%w: keys.masterKeyEnv names %s, which is not set", ErrNoMasterKey, name)
	}
	if strings.TrimSpace(v) == "" {
		return nil, fmt.Errorf("keys.masterKeyEnv: the environment variable %s is empty; %s", name, masterFormat)
	}
	m, err := ParseMaster(v)
	if err != nil {
		return nil, fmt.Errorf("keys.masterKeyEnv: the environment variable %s: %w", name, err)
	}
	return m, nil
}

// ParseMaster returns the master key of s, 64 hexadecimal characters in either case, spaces
// and line breaks around them ignored. Its error never shows s.
func ParseMaster(s string) (*Master, error) {
	s = strings.TrimSpace(s)
	b, err := hex.DecodeString(s)
	if len(s) != 64 || err != nil { // the error of hex names the byte: it is not passed on
		return nil, fmt.Errorf("not a master key: %s", masterFormat)
	}
	defer clear(b)
	block, _ := aes.NewCipher(b)    // 32 bytes: AES-256, never an error
	aead, _ := cipher.NewGCM(block) // AES has the block size of GCM: never an error
	return &Master{aead: aead}, nil
}

// String returns a placeholder: a master key is never printed.
func (Master) String() string { return "keys.Master(redacted)" }

// GoString returns a placeholder for %#v.
func (Master) GoString() string { return "keys.Master(redacted)" }

// LogValue returns a placeholder for log/slog.
func (Master) LogValue() slog.Value { return slog.StringValue("redacted") }

// seal encrypts plain, a private key, as nonce || AES-256-GCM(plain) with the fingerprint of
// the key as the additional data, so that a ciphertext moved to the row of another key does
// not decrypt. The nonce is random: 96 bits, and a repo has a handful of keys.
func (m *Master) seal(fingerprint string, plain []byte) []byte {
	nonce := make([]byte, m.aead.NonceSize(), m.aead.NonceSize()+len(plain)+m.aead.Overhead())
	_, _ = rand.Read(nonce) // crypto/rand.Read never returns an error; it crashes the program instead
	return m.aead.Seal(nonce, nonce, plain, []byte(fingerprint))
}

// Seal encrypts plain, a secret of the index that is not a private key (a webhook's,
// docs/spec/webhooks.md §5), as nonce || AES-256-GCM(plain) with aad as the additional data, so
// that a ciphertext moved to another row does not decrypt.
func (m *Master) Seal(aad string, plain []byte) []byte { return m.seal(aad, plain) }

// Open decrypts what Seal returned for aad. A wrong master key, a ciphertext of another row
// and one that was changed fail alike, and the error never shows a byte of either.
func (m *Master) Open(aad string, sealed []byte) ([]byte, error) {
	plain, err := m.open(aad, sealed)
	if err != nil {
		return nil, fmt.Errorf("keys: the secret of %s does not decrypt with the master key: another master key, "+
			"or the row was changed", aad)
	}
	return plain, nil
}

// open decrypts what seal returned for the key of fingerprint. A wrong master key, a
// ciphertext of another key and one that was changed all fail alike.
func (m *Master) open(fingerprint string, sealed []byte) ([]byte, error) {
	n := m.aead.NonceSize()
	if len(sealed) < n+m.aead.Overhead() {
		return nil, fmt.Errorf("keys: the private key of %s is %d bytes, too short to be encrypted", fingerprint, len(sealed))
	}
	plain, err := m.aead.Open(nil, sealed[:n], sealed[n:], []byte(fingerprint))
	if err != nil {
		return nil, fmt.Errorf("keys: the private key of %s does not decrypt with the master key: another master key, "+
			"or the row was changed", fingerprint)
	}
	return plain, nil
}
