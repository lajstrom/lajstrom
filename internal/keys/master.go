package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
)

// masterFormat says what a master key is, for the errors about one. It never shows a value.
const masterFormat = "the master key is 64 hexadecimal characters (32 bytes), e.g. the output of openssl rand -hex 32"

// ErrNoMasterKey is the error of LoadMaster when no master key is configured: keys.masterKeyEnv
// is empty, or the variable it names is not set. serve then runs with signing off, unless the
// index holds signing keys that only the master key decrypts; admin key fails.
var ErrNoMasterKey = errors.New("keys: no master key")

// ErrNoSealingKey is the error of a seal while this process does not hold the master key that
// the index names to seal, or before the index named one to it (docs/spec/index-schema.md, The
// master key and its replacement, #1288): it fails closed, and seals with no other key. A
// *SealingError wraps it.
var ErrNoSealingKey = errors.New("keys: this process does not hold the master key that seals")

// SealRetryAfter is the Retry-After of a request that would seal while this process does not
// hold the master key that seals: a beat of serve reads the sealing key again (#1288).
const SealRetryAfter = 30 * time.Second

// MaxMasterKeys is how many master keys a process holds at most, in all its sources (#1288).
const MaxMasterKeys = 8

// snapshotInfo is the HKDF info of the snapshot key (docs/spec/manifest-profile.md §9, Sealed):
// a snapshot of the control plane is sealed with a key derived from the master key, so that the
// cipher of the private keys is used for nothing else.
const snapshotInfo = "lajstrom snapshot v1"

// idInfo is the HKDF info of a master key's id (docs/spec/index-schema.md, The master key and its
// replacement): 8 bytes of HKDF-SHA256 of the key, no salt, in hex.
const idInfo = "lajstrom master key id v1"

// Master is the set of master keys a process holds, and the one of them that seals: AES-256-GCM
// over the private keys at rest and the other secrets of the index, and over the snapshots of the
// control plane with a key derived from each (#1288). The keys are loaded once at start and never
// printed: String, GoString and LogValue show no key material, and the bytes themselves are not
// kept, only the ciphers made from them and the ids. A value is sealed with the key the index
// names (SealWith) and with no other; it opens with any key held, that one first.
type Master struct {
	held    []*masterKey               // by id
	sealing *atomic.Pointer[sealState] // never nil; a pointer, so that a copy of a Master shares it
}

// masterKey is one master key held: its id, its source (the variable's name or the file's path),
// and its ciphers.
type masterKey struct {
	id, source string
	aead       cipher.AEAD
	snapshot   cipher.AEAD // AES-256-GCM under HKDF-SHA256(master key, snapshotInfo)
}

// sealState is the key that seals: id the one the index names ("" before it named one), key the
// held key of that id, nil when this process does not hold it.
type sealState struct {
	id  string
	key *masterKey
}

// HeldKey names a master key a process holds: its id and its source, never the key.
type HeldKey struct {
	ID     string
	Source string
}

// MasterSource is a master key as a source of the configuration holds it: Source names the source
// (the variable's name or the file's path) for the log and the errors, Text is its content.
type MasterSource struct {
	Source string
	Text   string
}

// MasterID returns the id of the master key whose 64 hexadecimal characters are s, or an error
// that never shows s: the hex of the first 8 bytes of HKDF-SHA256 of the key's 32 bytes, no salt,
// the info "lajstrom master key id v1".
func MasterID(s string) (string, error) {
	b, err := parseMaster(s)
	if err != nil {
		return "", err
	}
	defer clear(b)
	return masterID(b), nil
}

// masterID is the id of the key b.
func masterID(b []byte) string {
	id, _ := hkdf.Key(sha256.New, b, nil, idInfo, 8) // 8 bytes of SHA-256: never an error
	return hex.EncodeToString(id)
}

// LoadMaster reads the master key from the environment variable name, as keys.masterKeyEnv
// names it; lookupEnv is os.LookupEnv outside tests. No name, or a variable that is not set,
// is an error wrapping ErrNoMasterKey. A variable that is set but empty, or is not 64
// hexadecimal characters, is an error that names the variable and the format, never the value.
// Spaces and line breaks around the value are ignored, as a file mounted from a secret ends
// with one. The key seals: a Master of one key, as ParseMaster's.
func LoadMaster(name string, lookupEnv func(string) (string, bool)) (*Master, error) {
	src, err := MasterFromEnv(name, lookupEnv)
	if err != nil {
		return nil, err
	}
	return ParseMaster(src.Text)
}

// MasterFromEnv reads the master key of the environment variable name, as LoadMaster does, and
// returns it as a source, named by the variable.
func MasterFromEnv(name string, lookupEnv func(string) (string, bool)) (MasterSource, error) {
	if name == "" {
		return MasterSource{}, fmt.Errorf("%w: keys.masterKeyEnv is not set", ErrNoMasterKey)
	}
	v, ok := lookupEnv(name)
	if !ok {
		return MasterSource{}, fmt.Errorf("%w: keys.masterKeyEnv names %s, which is not set", ErrNoMasterKey, name)
	}
	if strings.TrimSpace(v) == "" {
		return MasterSource{}, fmt.Errorf("keys.masterKeyEnv: the environment variable %s is empty; %s", name, masterFormat)
	}
	if _, err := MasterID(v); err != nil {
		return MasterSource{}, fmt.Errorf("keys.masterKeyEnv: the environment variable %s: %w", name, err)
	}
	return MasterSource{Source: name, Text: v}, nil
}

// ParseMaster returns the Master of the one key s, 64 hexadecimal characters in either case,
// spaces and line breaks around them ignored, which seals: what a process of one key holds once
// the index names it, for the tests and the tools that seal without an index. Its error never
// shows s.
func ParseMaster(s string) (*Master, error) {
	m, err := HoldMasters([]MasterSource{{Text: s}})
	if err != nil {
		return nil, err
	}
	m.SealWith(m.held[0].id)
	return m, nil
}

// HoldMasters returns the Master of the keys of srcs, none of which seals until the index names
// one (SealWith): a seal fails with ErrNoSealingKey until then. A text that is not a key is an
// error that names its source and the format, never the text; a key in two sources is held once,
// under the first; no source is an error wrapping ErrNoMasterKey, and more than MaxMasterKeys keys
// an error that names how many.
func HoldMasters(srcs []MasterSource) (*Master, error) {
	m := &Master{sealing: &atomic.Pointer[sealState]{}}
	m.sealing.Store(&sealState{})
	for _, src := range srcs {
		b, err := parseMaster(src.Text)
		if err != nil {
			if src.Source == "" {
				return nil, err
			}
			return nil, fmt.Errorf("%s: %w", src.Source, err)
		}
		id := masterID(b)
		if m.holds(id) == nil {
			m.held = append(m.held, newMasterKey(id, src.Source, b))
		}
		clear(b)
	}
	switch n := len(m.held); {
	case n == 0:
		return nil, fmt.Errorf("%w: no source holds a key", ErrNoMasterKey)
	case n > MaxMasterKeys:
		return nil, fmt.Errorf("the sources of the master keys hold %d keys, more than %d", n, MaxMasterKeys)
	}
	slices.SortFunc(m.held, func(a, b *masterKey) int { return strings.Compare(a.id, b.id) })
	return m, nil
}

// parseMaster returns the 32 bytes of s, 64 hexadecimal characters, spaces and line breaks
// around them ignored; the caller clears them. Its error never shows s.
func parseMaster(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	b, err := hex.DecodeString(s)
	if len(s) != 64 || err != nil { // the error of hex names the byte: it is not passed on
		clear(b)
		return nil, fmt.Errorf("not a master key: %s", masterFormat)
	}
	return b, nil
}

// newMasterKey returns the key b of id and source, its ciphers made; b is not kept.
func newMasterKey(id, source string, b []byte) *masterKey {
	sk, _ := hkdf.Key(sha256.New, b, nil, snapshotInfo, 32) // 32 bytes of SHA-256: never an error
	defer clear(sk)
	return &masterKey{id: id, source: source, aead: gcm(b), snapshot: gcm(sk)}
}

// gcm returns AES-256-GCM under the 32 bytes of key.
func gcm(key []byte) cipher.AEAD {
	block, _ := aes.NewCipher(key)  // 32 bytes: AES-256, never an error
	aead, _ := cipher.NewGCM(block) // AES has the block size of GCM: never an error
	return aead
}

// String returns a placeholder: a master key is never printed.
func (Master) String() string { return "keys.Master(redacted)" }

// GoString returns a placeholder for %#v.
func (Master) GoString() string { return "keys.Master(redacted)" }

// LogValue returns a placeholder for log/slog.
func (Master) LogValue() slog.Value { return slog.StringValue("redacted") }

// holds returns the held key of id, nil when there is none.
func (m *Master) holds(id string) *masterKey {
	for _, k := range m.held {
		if k.id == id {
			return k
		}
	}
	return nil
}

// Held returns the ids of the keys m holds, each with its source, by id.
func (m *Master) Held() []HeldKey {
	out := make([]HeldKey, 0, len(m.held))
	for _, k := range m.held {
		out = append(out, HeldKey{ID: k.id, Source: k.source})
	}
	return out
}

// IDs returns the ids of the keys m holds, by id.
func (m *Master) IDs() []string {
	out := make([]string, 0, len(m.held))
	for _, k := range m.held {
		out = append(out, k.id)
	}
	return out
}

// Holds reports whether m holds the key of id.
func (m *Master) Holds(id string) bool { return m.holds(id) != nil }

// SealWith makes the key of id the one m seals with, as the index names it, and reports whether
// m holds it. When it does not, m seals with no key from then on, the one it sealed with before
// included, until a SealWith of a key it holds: it fails closed.
func (m *Master) SealWith(id string) bool {
	k := m.holds(id)
	m.sealing.Store(&sealState{id: id, key: k})
	return k != nil
}

// Sealing returns the id of the key m seals with, as the index named it ("" before it named
// one), and whether m holds it.
func (m *Master) Sealing() (id string, held bool) {
	s := m.sealing.Load()
	return s.id, s.key != nil
}

// sealer returns the key m seals with, or a *SealingError.
func (m *Master) sealer() (*masterKey, error) {
	s := m.sealing.Load()
	if s.key == nil {
		return nil, &SealingError{ID: s.id}
	}
	return s.key, nil
}

// SealingError is the error of a seal while m does not hold the key the index names, or before
// the index named one: errors.Is matches ErrNoSealingKey, and errors.As a
// *format.UnavailableError of SealRetryAfter, so that a request that would seal is answered 503
// with Retry-After (#1288).
type SealingError struct {
	// ID is the id of the key the index names, "" when it named none to this process.
	ID string
}

func (e *SealingError) Error() string {
	if e.ID == "" {
		return "keys: no master key seals yet: the index named none to this process"
	}
	return "keys: the index seals with master key " + e.ID + ", which this process does not hold"
}

// Unwrap returns ErrNoSealingKey and a *format.UnavailableError of SealRetryAfter.
func (e *SealingError) Unwrap() []error {
	return []error{ErrNoSealingKey, &format.UnavailableError{RetryAfter: SealRetryAfter}}
}

// order returns the keys of m in the order an open tries them: the sealing key first, then the
// others by id.
func (m *Master) order() []*masterKey {
	s := m.sealing.Load()
	if s.key == nil || len(m.held) == 1 {
		return m.held
	}
	out := make([]*masterKey, 0, len(m.held))
	out = append(out, s.key)
	for _, k := range m.held {
		if k != s.key {
			out = append(out, k)
		}
	}
	return out
}

// seal encrypts plain, a private key, as nonce || AES-256-GCM(plain) under the key that seals,
// with the fingerprint of the key as the additional data, so that a ciphertext moved to the row
// of another key does not decrypt. The nonce is random: 96 bits, and a repo has a handful of
// keys. Without the key that seals it is a *SealingError.
func (m *Master) seal(fingerprint string, plain []byte) ([]byte, error) {
	k, err := m.sealer()
	if err != nil {
		return nil, err
	}
	return sealWith(k.aead, plain, []byte(fingerprint)), nil
}

// sealWith returns nonce || aead.Seal(plain, ad) with a random nonce.
func sealWith(aead cipher.AEAD, plain, ad []byte) []byte {
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	_, _ = rand.Read(nonce) // crypto/rand.Read never returns an error; it crashes the program instead
	return aead.Seal(nonce, nonce, plain, ad)
}

// Seal encrypts plain, a secret of the index that is not a private key (a webhook's,
// docs/spec/webhooks.md §5), as nonce || AES-256-GCM(plain) under the key that seals, with aad as
// the additional data, so that a ciphertext moved to another row does not decrypt. Without the
// key that seals it is a *SealingError.
func (m *Master) Seal(aad string, plain []byte) ([]byte, error) { return m.seal(aad, plain) }

// Open decrypts what Seal returned for aad, with any key m holds. A master key m does not hold,
// a ciphertext of another row and one that was changed fail alike, and the error never shows a
// byte of either.
func (m *Master) Open(aad string, sealed []byte) ([]byte, error) {
	plain, _, err := m.OpenKey(aad, sealed)
	if err != nil {
		return nil, fmt.Errorf("keys: the secret of %s does not decrypt with the master key: another master key, "+
			"or the row was changed", aad)
	}
	return plain, nil
}

// OpenKey decrypts what Seal or seal returned for aad, as Open, and returns the id of the key
// that opened it (#1288: the rewrap counts the values under each key).
func (m *Master) OpenKey(aad string, sealed []byte) (plain []byte, id string, err error) {
	for _, k := range m.order() {
		if plain, err = openWith(k.aead, sealed, []byte(aad)); err == nil {
			return plain, k.id, nil
		}
	}
	if len(sealed) < m.held[0].aead.NonceSize()+m.held[0].aead.Overhead() {
		return nil, "", fmt.Errorf("keys: the value of %s is %d bytes, too short to be sealed", aad, len(sealed))
	}
	return nil, "", fmt.Errorf("keys: %s does not decrypt with a master key this process holds", aad)
}

// openWith opens nonce || ciphertext with aead and ad; one too short to be sealed fails.
func openWith(aead cipher.AEAD, sealed, ad []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(sealed) < n+aead.Overhead() {
		return nil, errors.New("too short")
	}
	return aead.Open(nil, sealed[:n], sealed[n:], ad)
}

// open decrypts what seal returned for the key of fingerprint, with any key m holds. A master
// key m does not hold, a ciphertext of another key and one that was changed all fail alike.
func (m *Master) open(fingerprint string, sealed []byte) ([]byte, error) {
	if n := m.held[0].aead.NonceSize() + m.held[0].aead.Overhead(); len(sealed) < n {
		return nil, fmt.Errorf("keys: the private key of %s is %d bytes, too short to be encrypted", fingerprint, len(sealed))
	}
	plain, _, err := m.OpenKey(fingerprint, sealed)
	if err != nil {
		return nil, fmt.Errorf("keys: the private key of %s does not decrypt with the master key: another master key, "+
			"or the row was changed", fingerprint)
	}
	return plain, nil
}

// SealSnapshot encrypts plain, the gzip'd document of a snapshot of the control plane
// (docs/spec/manifest-profile.md §9), as nonce || AES-256-GCM(plain) under the snapshot key of
// the key that seals, with ad, the layer's media type, as the additional data. The nonce is
// random: 96 bits, and an installation writes a snapshot a day. Without the key that seals it is
// a *SealingError.
func (m *Master) SealSnapshot(plain, ad []byte) ([]byte, error) {
	k, err := m.sealer()
	if err != nil {
		return nil, err
	}
	return sealWith(k.snapshot, plain, ad), nil
}

// OpenSnapshot decrypts what SealSnapshot returned with the same ad, with the snapshot key of
// any key m holds, the key that seals first. A master key m does not hold, other additional data
// and changed bytes fail alike.
func (m *Master) OpenSnapshot(sealed, ad []byte) ([]byte, error) {
	if n := m.held[0].snapshot.NonceSize() + m.held[0].snapshot.Overhead(); len(sealed) < n {
		return nil, fmt.Errorf("keys: the snapshot is %d bytes, too short to be sealed", len(sealed))
	}
	for _, k := range m.order() {
		if plain, err := openWith(k.snapshot, sealed, ad); err == nil {
			return plain, nil
		}
	}
	return nil, errors.New("keys: the snapshot does not open with the master key: another master key, or its bytes were changed")
}
