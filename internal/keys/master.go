package keys

import (
	"context"
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
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
// names (SealWith) and with no other; it opens with any key held, that one first. A data key of
// keys.masterKeyKMS joins the held keys once the KMS unwrapped it (Unwrap, #1388): until then it
// is pending, and stays so in a process whose KMS does not answer.
type Master struct {
	keys    *atomic.Pointer[heldKeys]  // never nil; pointers, so that a copy of a Master shares them
	sealing *atomic.Pointer[sealState] // never nil
	mu      *sync.Mutex                // serializes the unwraps that add a key
}

// heldKeys are the keys a Master holds, by id, and the ids of the data keys it is configured
// with and has not unwrapped yet, by id. Never changed once stored: an unwrap stores another.
type heldKeys struct {
	held    []*masterKey
	pending []string
}

// sealedMin is the length of the shortest sealed value: a nonce and the tag of AES-256-GCM.
const sealedMin = 12 + 16

// ErrOtherDataKey means the KMS unwrapped, for a data key of keys.masterKeyKMS, bytes that are
// not that key: another id, or not 32 bytes. They are never held (#1388).
var ErrOtherDataKey = errors.New("keys: the KMS answered another key than the one named")

// MasterAD is the additional data of every wrap and unwrap of the data key id (#1388): on AWS the
// encryption context {"lajstrom": "master key <id>"}, on GCP "lajstrom master key <id>".
func MasterAD(id string) AdditionalData {
	return AdditionalData{Key: "lajstrom", Value: "master key " + id}
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
	m.SealWith(m.state().held[0].id)
	return m, nil
}

// HoldMasters returns the Master of the keys of srcs, none of which seals until the index names
// one (SealWith): a seal fails with ErrNoSealingKey until then. A text that is not a key is an
// error that names its source and the format, never the text; a key in two sources is held once,
// under the first; no source is an error wrapping ErrNoMasterKey, and more than MaxMasterKeys keys
// an error that names how many.
func HoldMasters(srcs []MasterSource) (*Master, error) { return HoldMasterKeys(srcs, nil) }

// HoldMasterKeys is HoldMasters with the ids of the data keys of keys.masterKeyKMS, pending until
// Unwrap holds them (#1388): they count among the keys, against MaxMasterKeys and against none.
func HoldMasterKeys(srcs []MasterSource, dataKeys []string) (*Master, error) {
	m := &Master{keys: &atomic.Pointer[heldKeys]{}, sealing: &atomic.Pointer[sealState]{}, mu: &sync.Mutex{}}
	m.sealing.Store(&sealState{})
	var held []*masterKey
	holds := func(id string) bool { return slices.ContainsFunc(held, func(k *masterKey) bool { return k.id == id }) }
	for _, src := range srcs {
		b, err := parseMaster(src.Text)
		if err != nil {
			if src.Source == "" {
				return nil, err
			}
			return nil, fmt.Errorf("%s: %w", src.Source, err)
		}
		id := masterID(b)
		if !holds(id) {
			held = append(held, newMasterKey(id, src.Source, b))
		}
		clear(b)
	}
	var pending []string
	for _, id := range dataKeys {
		if !holds(id) && !slices.Contains(pending, id) {
			pending = append(pending, id)
		}
	}
	switch n := len(held) + len(pending); {
	case n == 0:
		return nil, fmt.Errorf("%w: no source holds a key", ErrNoMasterKey)
	case n > MaxMasterKeys:
		return nil, fmt.Errorf("the sources of the master keys hold %d keys, more than %d", n, MaxMasterKeys)
	}
	slices.SortFunc(held, func(a, b *masterKey) int { return strings.Compare(a.id, b.id) })
	slices.Sort(pending)
	m.keys.Store(&heldKeys{held: held, pending: pending})
	return m, nil
}

// state returns the keys m holds now, and those pending.
func (m *Master) state() *heldKeys { return m.keys.Load() }

// Pending returns the ids of the data keys of keys.masterKeyKMS that m has not unwrapped yet, by
// id (#1388).
func (m *Master) Pending() []string { return slices.Clone(m.state().pending) }

// IsPending reports whether id is a data key that m is configured with and has not unwrapped.
func (m *Master) IsPending(id string) bool { return slices.Contains(m.state().pending, id) }

// Unwrap has kms unwrap the data key of row, wrapped by the KMS key key, and holds it (#1388):
// the KMS's answer, 32 bytes whose id is row.KeyID, joins the held keys under the source key, the
// KMS key's URI, and leaves the pending ones; it does not seal until SealWith names it. Another
// answer wraps ErrOtherDataKey and is never held; a KMS that refused or did not answer is its
// error, which wraps ErrKMSUnavailable when asking again may succeed. A row of another KMS key is
// an error, and nothing is asked. A key m holds is not asked for again. The bytes are cleared
// before it returns, and no error holds them or the ciphertext.
func (m *Master) Unwrap(ctx context.Context, kms MasterKMS, key config.KMSKey, row index.WrappedMasterKey) error {
	if m.Holds(row.KeyID) {
		return nil
	}
	if row.KMS != key.String() {
		return fmt.Errorf("keys: master key %s is wrapped by %s, not %s", row.KeyID, row.KMS, key)
	}
	plain, err := kms.DecryptSymmetric(ctx, key.Name, row.Wrapped, MasterAD(row.KeyID))
	if err != nil {
		return fmt.Errorf("keys: master key %s: the KMS key %s: %w", row.KeyID, key, err)
	}
	defer clear(plain)
	if len(plain) != 32 || masterID(plain) != row.KeyID {
		return fmt.Errorf("%w: the KMS key %s unwrapped, for master key %s, what is not that key; it is not held", ErrOtherDataKey,
			key, row.KeyID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state()
	if slices.ContainsFunc(st.held, func(k *masterKey) bool { return k.id == row.KeyID }) {
		return nil
	}
	held := append(slices.Clone(st.held), newMasterKey(row.KeyID, key.String(), plain))
	slices.SortFunc(held, func(a, b *masterKey) int { return strings.Compare(a.id, b.id) })
	pending := slices.DeleteFunc(slices.Clone(st.pending), func(id string) bool { return id == row.KeyID })
	m.keys.Store(&heldKeys{held: held, pending: pending})
	return nil
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
	for _, k := range m.state().held {
		if k.id == id {
			return k
		}
	}
	return nil
}

// Held returns the ids of the keys m holds, each with its source, by id.
func (m *Master) Held() []HeldKey {
	held := m.state().held
	out := make([]HeldKey, 0, len(held))
	for _, k := range held {
		out = append(out, HeldKey{ID: k.id, Source: k.source})
	}
	return out
}

// IDs returns the ids of the keys m holds, by id.
func (m *Master) IDs() []string {
	held := m.state().held
	out := make([]string, 0, len(held))
	for _, k := range held {
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
		return nil, &SealingError{ID: s.id, Pending: s.id != "" && m.IsPending(s.id)}
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
	// Pending is true when ID is a data key of keys.masterKeyKMS that the KMS has not unwrapped
	// for this process yet (#1388).
	Pending bool
}

func (e *SealingError) Error() string {
	switch {
	case e.ID == "":
		return "keys: no master key seals yet: the index named none to this process"
	case e.Pending:
		return "keys: the index seals with master key " + e.ID + ", which this process has not unwrapped yet"
	}
	return "keys: the index seals with master key " + e.ID + ", which this process does not hold"
}

// Unwrap returns ErrNoSealingKey and a *format.UnavailableError of SealRetryAfter.
func (e *SealingError) Unwrap() []error {
	return []error{ErrNoSealingKey, &format.UnavailableError{RetryAfter: SealRetryAfter}}
}

// order returns the keys of st in the order an open tries them: the sealing key first, then the
// others by id.
func (m *Master) order(st *heldKeys) []*masterKey {
	s := m.sealing.Load()
	if s.key == nil || len(st.held) == 1 {
		return st.held
	}
	out := make([]*masterKey, 0, len(st.held))
	out = append(out, s.key)
	for _, k := range st.held {
		if k != s.key {
			out = append(out, k)
		}
	}
	return out
}

// pendingErr is err, of a value that no key of st opens, as the KMS's unavailability while st has
// data keys it has not unwrapped yet, one of which may be the value's: the routes answer 503 with
// Retry-After (#1388). err as it is otherwise.
func pendingErr(st *heldKeys, err error) error {
	if len(st.pending) == 0 {
		return err
	}
	return unavailable(fmt.Errorf("%w; master keys %s are not unwrapped yet: %w", err, strings.Join(st.pending, ", "),
		ErrKMSUnavailable))
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
	st := m.state()
	plain, _, err := m.openKey(st, aad, sealed)
	if err != nil {
		return nil, pendingErr(st, fmt.Errorf("keys: the secret of %s does not decrypt with the master key: another master key, "+
			"or the row was changed", aad))
	}
	return plain, nil
}

// OpenKey decrypts what Seal or seal returned for aad, as Open, and returns the id of the key
// that opened it (#1288: the rewrap counts the values under each key).
func (m *Master) OpenKey(aad string, sealed []byte) (plain []byte, id string, err error) {
	st := m.state()
	plain, id, err = m.openKey(st, aad, sealed)
	return plain, id, pendingErr(st, err)
}

// openKey is OpenKey with the keys of st.
func (m *Master) openKey(st *heldKeys, aad string, sealed []byte) (plain []byte, id string, err error) {
	for _, k := range m.order(st) {
		if plain, err = openWith(k.aead, sealed, []byte(aad)); err == nil {
			return plain, k.id, nil
		}
	}
	if len(sealed) < sealedMin {
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
	if len(sealed) < sealedMin {
		return nil, fmt.Errorf("keys: the private key of %s is %d bytes, too short to be encrypted", fingerprint, len(sealed))
	}
	st := m.state()
	plain, _, err := m.openKey(st, fingerprint, sealed)
	if err != nil {
		return nil, pendingErr(st, fmt.Errorf("keys: the private key of %s does not decrypt with the master key: another master "+
			"key, or the row was changed", fingerprint))
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
	if len(sealed) < sealedMin {
		return nil, fmt.Errorf("keys: the snapshot is %d bytes, too short to be sealed", len(sealed))
	}
	st := m.state()
	for _, k := range m.order(st) {
		if plain, err := openWith(k.snapshot, sealed, ad); err == nil {
			return plain, nil
		}
	}
	return nil, pendingErr(st, errors.New("keys: the snapshot does not open with the master key: another master key, or its "+
		"bytes were changed"))
}
