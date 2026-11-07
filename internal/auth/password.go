package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// ErrInvalid means a user name or a password breaks its rule. The message of an error that
// wraps it starts with the field it is about.
var ErrInvalid = errors.New("auth: invalid")

// ErrMalformedHash means a stored password hash is not an argon2id PHC string this package
// can verify. It is a fault of the stored data, never a wrong password.
var ErrMalformedHash = errors.New("auth: malformed password hash")

// Bounds of passwords and user names.
const (
	// MinPasswordLen and MaxPasswordLen bound a password, in bytes.
	MinPasswordLen = 8
	MaxPasswordLen = 1024
	// MaxNameLen bounds a user name, in bytes.
	MaxNameLen = 64
)

// argonParams are the argon2id parameters of a hash.
type argonParams struct {
	memory  uint32 // KiB
	time    uint32 // passes
	threads uint8
	saltLen int
	keyLen  int
}

// hashParams are the parameters of every new hash: the first recommendation of the OWASP
// Password Storage Cheat Sheet for argon2id (19 MiB, 2 passes, 1 thread), a 16-byte salt and a
// 32-byte key. They are the one place to change: VerifyPassword reads the parameters of each
// hash from the hash, so the hashes made before a change still verify.
var hashParams = argonParams{memory: 19456, time: 2, threads: 1, saltLen: 16, keyLen: 32}

// Bounds of the parameters VerifyPassword accepts in a hash: below them a hash is not one this
// package made, and argon2 panics on zero passes or threads.
const (
	minSaltLen = 8
	minKeyLen  = 16
)

// phcVersion is the argon2 version of the PHC string, 19 (0x13).
var phcVersion = "v=" + strconv.Itoa(argon2.Version)

// b64 is the base64 of PHC strings: the standard alphabet without padding.
var b64 = base64.RawStdEncoding

// HashPassword checks password (CheckPassword) and returns its argon2id hash as a PHC string,
// $argon2id$v=19$m=<KiB>,t=<passes>,p=<threads>$<salt>$<key>, with a random salt and the
// parameters of hashParams.
func HashPassword(password string) (string, error) {
	if err := CheckPassword(password); err != nil {
		return "", err
	}
	return hashWith(hashParams, password), nil
}

// hashWith returns the PHC string of password under p, with a random salt.
func hashWith(p argonParams, password string) string {
	salt := make([]byte, p.saltLen)
	_, _ = rand.Read(salt)                                                                     // crypto/rand.Read does not return errors; it crashes the program instead
	key := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(p.keyLen)) //nolint:gosec // keyLen is a small constant or a parsed length
	return fmt.Sprintf("$argon2id$%s$m=%d,t=%d,p=%d$%s$%s",
		phcVersion, p.memory, p.time, p.threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// VerifyPassword reports whether password is the one hash was made from. It computes argon2id
// with the parameters, the salt and the key length of hash, and compares in constant time. A
// hash it cannot parse is an error wrapping ErrMalformedHash, not false; the error never
// contains the hash.
func VerifyPassword(hash, password string) (bool, error) {
	p, salt, want, err := parseHash(hash)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want))) //nolint:gosec // len(want) is a decoded key length
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// parseHash splits a PHC string into its parameters, salt and key.
func parseHash(hash string) (argonParams, []byte, []byte, error) {
	fields := strings.Split(hash, "$")
	if len(fields) != 6 || fields[0] != "" {
		return argonParams{}, nil, nil, fmt.Errorf("%w: want $argon2id$v=19$m=…,t=…,p=…$<salt>$<key>", ErrMalformedHash)
	}
	if fields[1] != "argon2id" {
		return argonParams{}, nil, nil, fmt.Errorf("%w: the algorithm is not argon2id", ErrMalformedHash)
	}
	if fields[2] != phcVersion {
		return argonParams{}, nil, nil, fmt.Errorf("%w: the version is not %s", ErrMalformedHash, phcVersion)
	}
	p, err := parseParams(fields[3])
	if err != nil {
		return argonParams{}, nil, nil, err
	}
	salt, err := b64.DecodeString(fields[4])
	if err != nil || len(salt) < minSaltLen {
		return argonParams{}, nil, nil, fmt.Errorf("%w: the salt is not %d or more bytes of unpadded base64", ErrMalformedHash, minSaltLen)
	}
	key, err := b64.DecodeString(fields[5])
	if err != nil || len(key) < minKeyLen {
		return argonParams{}, nil, nil, fmt.Errorf("%w: the key is not %d or more bytes of unpadded base64", ErrMalformedHash, minKeyLen)
	}
	return p, salt, key, nil
}

// parseParams parses m=<KiB>,t=<passes>,p=<threads>, in this order: memory and passes at
// least 1 and at most 2^32-1, threads 1 to 255. The bounds are checked here, next to the
// conversions, where the linters see them.
func parseParams(s string) (argonParams, error) {
	var p argonParams
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return p, fmt.Errorf("%w: the parameters are not m=…,t=…,p=…", ErrMalformedHash)
	}
	memory, err := parseParam(parts[0], "m")
	if err != nil {
		return p, err
	}
	passes, err := parseParam(parts[1], "t")
	if err != nil {
		return p, err
	}
	threads, err := parseParam(parts[2], "p")
	if err != nil {
		return p, err
	}
	if memory > math.MaxUint32 {
		return p, fmt.Errorf("%w: parameter m is more than %d", ErrMalformedHash, uint64(math.MaxUint32))
	}
	if passes > math.MaxUint32 {
		return p, fmt.Errorf("%w: parameter t is more than %d", ErrMalformedHash, uint64(math.MaxUint32))
	}
	if threads > math.MaxUint8 {
		return p, fmt.Errorf("%w: parameter p is more than %d", ErrMalformedHash, math.MaxUint8)
	}
	return argonParams{memory: uint32(memory), time: uint32(passes), threads: uint8(threads)}, nil
}

// parseParam parses part as <name>=<n>, n a positive integer.
func parseParam(part, name string) (uint64, error) {
	v, ok := strings.CutPrefix(part, name+"=")
	n, err := strconv.ParseUint(v, 10, 64)
	if !ok || err != nil || n == 0 {
		return 0, fmt.Errorf("%w: parameter %s is not a positive integer", ErrMalformedHash, name)
	}
	return n, nil
}

// CheckPassword checks that password is UTF-8 of MinPasswordLen to MaxPasswordLen bytes. Any
// character is allowed. Errors wrap ErrInvalid and name the bound; they never contain the
// password.
func CheckPassword(password string) error {
	switch {
	case len(password) < MinPasswordLen:
		return invalidf("password: must be at least %d bytes", MinPasswordLen)
	case len(password) > MaxPasswordLen:
		return invalidf("password: must be at most %d bytes", MaxPasswordLen)
	case !utf8.ValidString(password):
		return invalidf("password: must be UTF-8")
	}
	return nil
}

// reservedNames are user names that HTTP Basic gives another meaning: deploy:<token> is how a
// deploy token authenticates (docs/spec/rbac.md §1, #44).
var reservedNames = map[string]bool{"deploy": true}

// CheckName checks a user name: 1 to MaxNameLen bytes of lowercase letters, digits, '.', '_'
// and '-', the first a letter or a digit, and not a reserved name. A name is what HTTP Basic
// carries before the colon and what user:<name> binds, so it has no colon, no space and no
// upper case. Errors wrap ErrInvalid.
func CheckName(name string) error {
	if name == "" {
		return invalidf("name: must not be empty")
	}
	if reservedNames[name] {
		return invalidf("name: %q is reserved: Basic credentials %s:<token> are a deploy token", name, name)
	}
	ok := len(name) <= MaxNameLen && isLowerOrDigit(name[0])
	for i := 1; ok && i < len(name); i++ {
		c := name[i]
		ok = isLowerOrDigit(c) || c == '.' || c == '_' || c == '-'
	}
	if !ok {
		return invalidf("name: %q must be lowercase letters, digits, '.', '_' and '-', starting with a letter or a digit, at most %d bytes",
			name, MaxNameLen)
	}
	return nil
}

func isLowerOrDigit(c byte) bool { return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' }

// authError is an error whose message is written for the person who caused it, a CLI user or
// an API client. It wraps the sentinel that tells what went wrong: ErrInvalid,
// index.ErrNotFound or index.ErrConflict.
type authError struct {
	msg string
	err error
}

func (e *authError) Error() string { return e.msg }
func (e *authError) Unwrap() error { return e.err }

// invalidf returns an *authError wrapping ErrInvalid with the message fmt.Sprintf(format, args...).
func invalidf(format string, args ...any) error {
	return &authError{msg: fmt.Sprintf(format, args...), err: ErrInvalid}
}
