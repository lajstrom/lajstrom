package keystest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/lajstrom/lajstrom/internal/keys"
)

// WrapKMS is a keys.MasterKMS in memory: symmetric keys of AWS KMS, AES-256-GCM under bytes of
// its own, each by its key ARN, the first also by an alias. A ciphertext names the key that made
// it, as AWS's does, and opens only with the additional data it was made with. Fail makes it
// fail, Disable makes a key one that does not wrap. It is safe for concurrent use.
type WrapKMS struct {
	// ARN is the first key's canonical name, Alias another name of it.
	ARN, Alias string

	mu       sync.Mutex
	keys     map[string]cipher.AEAD
	spec     map[string]string // a key's spec, usage and state as DescribeKey answers them
	fail     error
	decrypts int
}

// NewWrapKMS returns a WrapKMS with one key.
func NewWrapKMS() *WrapKMS {
	k := &WrapKMS{ARN: "arn:aws:kms:eu-west-1:111122223333:key/keystest-wrap", Alias: "alias/keystest-wrap",
		keys: map[string]cipher.AEAD{}, spec: map[string]string{}}
	k.AddKey(k.ARN)
	return k
}

// URI is the key URI of the first key by its ARN, as keys.masterKeyKMS names it.
func (k *WrapKMS) URI() string { return "awskms:///" + k.ARN }

// AddKey adds a symmetric key of the ARN arn.
func (k *WrapKMS) AddKey(arn string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	block, _ := aes.NewCipher(b)
	aead, _ := cipher.NewGCM(block)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[arn], k.spec[arn] = aead, "SYMMETRIC_DEFAULT ENCRYPT_DECRYPT Enabled"
}

// Disable makes the key of arn answer the spec, usage and state of desc ("RSA_2048
// ENCRYPT_DECRYPT Enabled", "SYMMETRIC_DEFAULT ENCRYPT_DECRYPT Disabled"); a state other than
// Enabled refuses Encrypt and Decrypt as AWS's DisabledException.
func (k *WrapKMS) Disable(arn, desc string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.spec[arn] = desc
}

// Fail makes every call answer err from now on; nil ends it. An err that wraps
// keys.ErrKMSUnavailable is a KMS that does not answer.
func (k *WrapKMS) Fail(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fail = err
}

// Decrypts is the number of DecryptSymmetric calls.
func (k *WrapKMS) Decrypts() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.decrypts
}

// arnOf returns the ARN of name, an ARN or the alias, under the lock.
func (k *WrapKMS) arnOf(name string) (string, error) {
	if name == k.Alias {
		name = k.ARN
	}
	switch {
	case k.fail != nil:
		return "", k.fail
	case k.keys[name] == nil:
		return "", fmt.Errorf("NotFoundException: %s", name)
	}
	return name, nil
}

// SymmetricKey describes the key of name, an ARN or the alias: its ARN, or
// keys.ErrKMSKeyCannotWrap for a key Disable changed.
func (k *WrapKMS) SymmetricKey(_ context.Context, name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	arn, err := k.arnOf(name)
	if err != nil {
		return "", err
	}
	if s := k.spec[arn]; s != "SYMMETRIC_DEFAULT ENCRYPT_DECRYPT Enabled" {
		return "", fmt.Errorf("%w: %s is %s", keys.ErrKMSKeyCannotWrap, arn, s)
	}
	return arn, nil
}

// usable returns the cipher of the key of the ARN name, or the refusal of a disabled one.
func (k *WrapKMS) usable(name string) (cipher.AEAD, error) {
	arn, err := k.arnOf(name)
	switch {
	case err != nil:
		return nil, err
	case arn != name:
		return nil, fmt.Errorf("InvalidKeyUsageException: %s is not a key ARN", name)
	case !strings.HasSuffix(k.spec[arn], " Enabled"):
		return nil, fmt.Errorf("DisabledException: %s", arn)
	}
	return k.keys[arn], nil
}

// Encrypt encrypts plain with the key of the ARN name under ad: the ARN, a 0 byte, the nonce and
// AES-256-GCM, ad as the additional data.
func (k *WrapKMS) Encrypt(_ context.Context, name string, plain []byte, ad keys.AdditionalData) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	aead, err := k.usable(name)
	if err != nil {
		return nil, err
	}
	out := append([]byte(name), 0)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return aead.Seal(append(out, nonce...), nonce, plain, []byte(ad.String())), nil
}

// DecryptSymmetric decrypts what Encrypt returned with the key of the ARN name and ad: a
// ciphertext of another key, of another ad or changed is AWS's InvalidCiphertextException.
func (k *WrapKMS) DecryptSymmetric(_ context.Context, name string, ciphertext []byte, ad keys.AdditionalData) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.decrypts++
	aead, err := k.usable(name)
	if err != nil {
		return nil, err
	}
	arn, rest, ok := bytes.Cut(ciphertext, []byte{0})
	if !ok || string(arn) != name || len(rest) < aead.NonceSize() {
		return nil, errors.New("InvalidCiphertextException: not a ciphertext of this key")
	}
	plain, err := aead.Open(nil, rest[:aead.NonceSize()], rest[aead.NonceSize():], []byte(ad.String()))
	if err != nil {
		return nil, errors.New("InvalidCiphertextException: the ciphertext does not open with this context")
	}
	return plain, nil
}

// AWSWrapKMS returns an AWS KMS of the JSON 1.1 API over HTTP in front of k, for a client that
// goes to it with AWS_ENDPOINT_URL_KMS: DescribeKey, Encrypt and Decrypt of symmetric keys, the
// encryption context of one pair. It does not check SigV4 beyond its presence. While k fails, it
// answers 503 ThrottlingException for an error that wraps keys.ErrKMSUnavailable, and a 400 of
// the error's type for another.
func AWSWrapKMS(k *WrapKMS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			KeyID             string `json:"KeyId"`
			Plaintext         []byte
			CiphertextBlob    []byte
			EncryptionContext map[string]string
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || json.NewDecoder(r.Body).Decode(&in) != nil ||
			len(in.EncryptionContext) > 1 {
			awsError(w, http.StatusBadRequest, "ValidationException")
			return
		}
		var ad keys.AdditionalData
		for key, v := range in.EncryptionContext {
			ad = keys.AdditionalData{Key: key, Value: v}
		}
		var out any
		var err error
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.DescribeKey":
			out, err = k.describe(in.KeyID)
		case "TrentService.Encrypt":
			var c []byte
			if c, err = k.Encrypt(r.Context(), in.KeyID, in.Plaintext, ad); err == nil {
				out = map[string]any{"KeyId": in.KeyID, "CiphertextBlob": c, "EncryptionAlgorithm": "SYMMETRIC_DEFAULT"}
			}
		case "TrentService.Decrypt":
			var p []byte
			if p, err = k.DecryptSymmetric(r.Context(), in.KeyID, in.CiphertextBlob, ad); err == nil {
				out = map[string]any{"KeyId": in.KeyID, "Plaintext": p, "EncryptionAlgorithm": "SYMMETRIC_DEFAULT"}
			}
		default:
			awsError(w, http.StatusBadRequest, "UnknownOperationException")
			return
		}
		if err != nil {
			wrapFailed(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// describe is the answer of DescribeKey of name.
func (k *WrapKMS) describe(name string) (any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	arn, err := k.arnOf(name)
	if err != nil {
		return nil, err
	}
	f := strings.Fields(k.spec[arn])
	return map[string]any{"KeyMetadata": map[string]any{"Arn": arn, "KeySpec": f[0], "KeyUsage": f[1], "KeyState": f[2]}}, nil
}

// wrapFailed answers err of a WrapKMS, by the type its message starts with.
func wrapFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, keys.ErrKMSUnavailable) {
		awsError(w, http.StatusServiceUnavailable, "ThrottlingException")
		return
	}
	typ, _, ok := strings.Cut(err.Error(), ":")
	if !ok || strings.ContainsAny(typ, " .") {
		typ = "AccessDeniedException"
	}
	awsError(w, http.StatusBadRequest, typ)
}
