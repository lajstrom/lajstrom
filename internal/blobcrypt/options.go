package blobcrypt

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opencontainers/go-digest"
)

const (
	// Cipher is the only blob cipher of ocicrypt, the cipher of the public options.
	Cipher = "AES_256_CTR_HMAC_SHA256"
	// Suffix ends the media type of an encrypted layer: the role's media type, then Suffix.
	Suffix = "+encrypted"
	// AnnotationJWE is the layer annotation of the private options, wrapped.
	AnnotationJWE = "org.opencontainers.image.enc.keys.jwe"
	// AnnotationPubOpts is the layer annotation of the public options.
	AnnotationPubOpts = "org.opencontainers.image.enc.pubopts"
	// AnnotationPrefix starts every annotation of ocicrypt; a client may not send one.
	AnnotationPrefix = "org.opencontainers.image.enc."
	// KeySize is the size of a data key: AES-256, and the key of the HMAC.
	KeySize = 32
	// NonceSize is the size of a nonce: the AES block, the initial counter block.
	NonceSize = 16
	// nonceOption is the name of the nonce among the cipher options.
	nonceOption = "nonce"
)

// ErrInvalid is the error of options or a wrapping that cannot be read: not base64, not the JSON
// of ocicrypt, another cipher, a key or a nonce of the wrong size.
var ErrInvalid = errors.New("blobcrypt: invalid options")

// Private are the private options of a blob: its data key, its plaintext digest and its nonce,
// as ocicrypt names them in JSON. They are never in the clear in the registry: Wrap wraps them.
// String and the logs show the digest only.
type Private struct {
	Key           []byte            `json:"symkey"`
	Digest        digest.Digest     `json:"digest"`
	CipherOptions map[string][]byte `json:"cipheroptions"`
}

// NewPrivate returns the options of a new blob: a random data key and nonce, no digest yet.
func NewPrivate() Private {
	key := make([]byte, KeySize)
	nonce := make([]byte, NonceSize)
	_, _ = rand.Read(key) // crypto/rand.Read never returns an error; it crashes the program instead
	_, _ = rand.Read(nonce)
	return Private{Key: key, CipherOptions: map[string][]byte{nonceOption: nonce}}
}

// Nonce returns the nonce of the options.
func (p Private) Nonce() []byte { return p.CipherOptions[nonceOption] }

// Check returns ErrInvalid unless the data key and the nonce have their sizes.
func (p Private) Check() error {
	if len(p.Key) != KeySize || len(p.Nonce()) != NonceSize {
		return fmt.Errorf("%w: a data key of %d bytes and a nonce of %d, not %d and %d",
			ErrInvalid, len(p.Key), len(p.Nonce()), KeySize, NonceSize)
	}
	return nil
}

// String shows the digest only: the key is never printed.
func (p Private) String() string { return "blobcrypt.Private(" + p.Digest.String() + ")" }

// GoString is String, for %#v.
func (p Private) GoString() string { return p.String() }

// Public are the public options of a blob: the cipher and the HMAC of its ciphertext.
type Public struct {
	Cipher        string            `json:"cipher"`
	HMAC          []byte            `json:"hmac"`
	CipherOptions map[string][]byte `json:"cipheroptions"`
}

// EncodePublic returns the value of AnnotationPubOpts for a blob whose ciphertext has the HMAC
// mac: the standard base64 of {"cipher","hmac","cipheroptions"}.
func EncodePublic(mac []byte) string {
	b, _ := json.Marshal(Public{Cipher: Cipher, HMAC: mac, CipherOptions: map[string][]byte{}}) // never an error
	return base64.StdEncoding.EncodeToString(b)
}

// DecodePublic reads the value of AnnotationPubOpts and returns the HMAC. Another cipher, or an
// HMAC that is not 32 bytes, is ErrInvalid.
func DecodePublic(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("%w: the public options are not base64", ErrInvalid)
	}
	var p Public
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%w: the public options are not JSON: %w", ErrInvalid, err)
	}
	if p.Cipher != Cipher || len(p.HMAC) != 32 {
		return nil, fmt.Errorf("%w: the public options name the cipher %q and an HMAC of %d bytes", ErrInvalid, p.Cipher, len(p.HMAC))
	}
	return p.HMAC, nil
}

// Encrypted reports whether mediaType is that of an encrypted layer.
func Encrypted(mediaType string) bool { return strings.HasSuffix(mediaType, Suffix) }
