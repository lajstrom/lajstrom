package blobcrypt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/go-jose/go-jose/v4"
)

// ErrNoRecipient is the error of Unwrap when the wrapping has no recipient for the key: the
// blob was wrapped for another key of the repo, or before it had one.
var ErrNoRecipient = errors.New("blobcrypt: the options are not wrapped for this key")

// minRSABits is the smallest RSA key a blob is wrapped for.
const minRSABits = 2048

// Recipient is a public key that the private options of a blob are wrapped for: the repo's key,
// RSA, or a key of policy.encryption.recipients, RSA or EC on P-256, P-384, P-521. KID is its
// fingerprint, the kid of its JWE recipient.
type Recipient struct {
	Key crypto.PublicKey
	KID string
}

// NewRecipient returns the recipient of key, its kid its fingerprint. A key of another type, an
// RSA key under 2048 bits or an EC key of another curve is refused.
func NewRecipient(key crypto.PublicKey) (Recipient, error) {
	switch k := key.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < minRSABits {
			return Recipient{}, fmt.Errorf("blobcrypt: an RSA key of %d bits; at least %d", k.N.BitLen(), minRSABits)
		}
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
		default:
			return Recipient{}, errors.New("blobcrypt: an EC key that is not on P-256, P-384 or P-521")
		}
	default:
		return Recipient{}, fmt.Errorf("blobcrypt: a key of type %T; RSA or EC", key)
	}
	fp, err := Fingerprint(key)
	if err != nil {
		return Recipient{}, err
	}
	return Recipient{Key: key, KID: fp}, nil
}

// ParseRecipient reads a recipient of policy.encryption.recipients: one PEM block "PUBLIC KEY",
// a SubjectPublicKeyInfo, of a key NewRecipient takes.
func ParseRecipient(text string) (Recipient, error) {
	block, rest := pem.Decode([]byte(strings.TrimSpace(text)))
	switch {
	case block == nil || block.Type != "PUBLIC KEY":
		return Recipient{}, errors.New("blobcrypt: not a PEM block PUBLIC KEY")
	case len(strings.TrimSpace(string(rest))) > 0:
		return Recipient{}, errors.New("blobcrypt: more than one PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return Recipient{}, fmt.Errorf("blobcrypt: the public key: %w", err)
	}
	return NewRecipient(key)
}

// Fingerprint returns the fingerprint of a public key: the uppercase hex sha256 of its DER
// SubjectPublicKeyInfo.
func Fingerprint(key crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", fmt.Errorf("blobcrypt: the public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

// Wrap returns the value of AnnotationJWE for p: the standard base64 of a JWE in JSON
// serialization, content encrypted with A256GCM, with one recipient per key of recipients in
// their order, RSA-OAEP-256 for an RSA key and ECDH-ES+A256KW for an EC key, each with its kid.
func Wrap(p Private, recipients []Recipient) (string, error) {
	if err := p.Check(); err != nil {
		return "", err
	}
	if len(recipients) == 0 {
		return "", errors.New("blobcrypt: no recipient to wrap for")
	}
	rcpts := make([]jose.Recipient, len(recipients))
	for i, r := range recipients {
		alg := jose.RSA_OAEP_256
		if _, ok := r.Key.(*ecdsa.PublicKey); ok {
			alg = jose.ECDH_ES_A256KW
		}
		rcpts[i] = jose.Recipient{Algorithm: alg, Key: r.Key, KeyID: r.KID}
	}
	enc, err := jose.NewMultiEncrypter(jose.A256GCM, rcpts, nil)
	if err != nil {
		return "", fmt.Errorf("blobcrypt: wrap: %w", err)
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("blobcrypt: wrap: %w", err)
	}
	defer clear(plain)
	obj, err := enc.Encrypt(plain)
	if err != nil {
		return "", fmt.Errorf("blobcrypt: wrap: %w", err)
	}
	return base64.StdEncoding.EncodeToString([]byte(obj.FullSerialize())), nil
}

// KeyDecrypter decrypts the encrypted_key of a JWE recipient, RSAES-OAEP with SHA-256 and MGF1
// with SHA-256, without a label: a private key here (RSAKey), or a key in a KMS.
type KeyDecrypter interface {
	DecryptOAEP(encryptedKey []byte) ([]byte, error)
}

// RSAKey is a KeyDecrypter over an RSA private key held in memory.
type RSAKey struct{ Key *rsa.PrivateKey }

// DecryptOAEP decrypts with the private key.
func (k RSAKey) DecryptOAEP(encryptedKey []byte) ([]byte, error) {
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, k.Key, encryptedKey, nil)
}

// keyAlgorithms are the recipients' algorithms a wrapping may have: what Wrap writes, and
// ocicrypt's RSA-OAEP, which Unwrap does not open but must parse beside a recipient it opens.
var keyAlgorithms = []jose.KeyAlgorithm{jose.RSA_OAEP_256, jose.RSA_OAEP, jose.ECDH_ES_A128KW, jose.ECDH_ES_A192KW, jose.ECDH_ES_A256KW}

// Unwrap opens value, the value of AnnotationJWE (ocicrypt joins several wrappings with commas),
// for the recipient of the kid kid, with d: only that recipient's encrypted_key is sent to d, and
// only when it is RSA-OAEP-256. A wrapping without that recipient is ErrNoRecipient; one that
// cannot be read, or whose plaintext is not private options of the right sizes, ErrInvalid; an
// error of d is returned as it is, wrapped.
func Unwrap(value, kid string, d KeyDecrypter) (Private, error) {
	found := false
	for _, part := range strings.Split(strings.TrimSpace(value), ",") {
		raw, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			return Private{}, fmt.Errorf("%w: the wrapping is not base64", ErrInvalid)
		}
		obj, err := jose.ParseEncryptedJSON(string(raw), keyAlgorithms, []jose.ContentEncryption{jose.A256GCM, jose.A192GCM, jose.A128GCM})
		if err != nil {
			return Private{}, fmt.Errorf("%w: the wrapping: %w", ErrInvalid, err)
		}
		od := &opaque{kid: kid, d: d}
		_, _, plain, err := obj.DecryptMulti(od)
		if od.err != nil {
			return Private{}, od.err
		}
		found = found || od.found
		if err != nil {
			continue
		}
		defer clear(plain)
		var p Private
		if err := json.Unmarshal(plain, &p); err != nil {
			return Private{}, fmt.Errorf("%w: the private options are not JSON: %w", ErrInvalid, err)
		}
		if err := p.Check(); err != nil {
			return Private{}, err
		}
		return p, nil
	}
	if found {
		return Private{}, fmt.Errorf("%w: the recipient of %s does not open", ErrInvalid, kid)
	}
	return Private{}, fmt.Errorf("%w: no recipient %s", ErrNoRecipient, kid)
}

// opaque hands go-jose the recipient of kid alone, and keeps the error of the KeyDecrypter, which
// go-jose would replace with its own.
type opaque struct {
	kid   string
	d     KeyDecrypter
	found bool
	err   error
}

func (o *opaque) DecryptKey(encryptedKey []byte, h jose.Header) ([]byte, error) {
	if h.KeyID != o.kid || o.err != nil {
		return nil, ErrNoRecipient
	}
	o.found = true
	if h.Algorithm != string(jose.RSA_OAEP_256) {
		return nil, fmt.Errorf("%w: the recipient of %s is %s, not %s", ErrInvalid, o.kid, h.Algorithm, jose.RSA_OAEP_256)
	}
	cek, err := o.d.DecryptOAEP(encryptedKey)
	if err != nil {
		o.err = fmt.Errorf("blobcrypt: unwrap for %s: %w", o.kid, err)
	}
	return cek, err
}
