// Package keystest has test doubles of internal/keys: a KMS in memory. Imported only by tests.
package keystest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/keys"
)

// KMS is a keys.KMS in memory that holds one ECDSA P-256 key, as AWS KMS holds one of
// ECC_NIST_P256, under a key ARN and an alias. Fail makes it fail. It is safe for concurrent use.
type KMS struct {
	// ARN is the key's canonical name, Alias another name of it.
	ARN, Alias string

	mu      sync.Mutex
	priv    *ecdsa.PrivateKey
	created time.Time
	fail    error
	signs   int
}

// NewKMS returns a KMS with a new key made at created.
func NewKMS(created time.Time) *KMS {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err) // crypto/rand does not fail
	}
	return &KMS{ARN: "arn:aws:kms:eu-west-1:111122223333:key/keystest", Alias: "alias/keystest", priv: priv, created: created}
}

// URI is the key URI of the key by its alias, as keys.kms names it.
func (k *KMS) URI() string { return "awskms:///" + k.Alias }

// Fail makes Key and Sign answer err from now on; nil ends it. An err that wraps
// keys.ErrKMSUnavailable is a KMS that does not answer.
func (k *KMS) Fail(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fail = err
}

// Signs is the number of signatures asked for.
func (k *KMS) Signs() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.signs
}

// Key describes the key by its ARN or its alias.
func (k *KMS) Key(_ context.Context, name string) (*keys.KMSKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case k.fail != nil:
		return nil, k.fail
	case name != k.ARN && name != k.Alias:
		return nil, fmt.Errorf("NotFoundException: %s", name)
	}
	return &keys.KMSKey{Name: k.ARN, Public: k.priv.Public(), Created: k.created}, nil
}

// Sign signs digest with the key, by its ARN.
func (k *KMS) Sign(_ context.Context, name string, _ crypto.PublicKey, digest []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.signs++
	switch {
	case k.fail != nil:
		return nil, k.fail
	case name != k.ARN:
		return nil, fmt.Errorf("NotFoundException: %s", name)
	}
	return k.priv.Sign(rand.Reader, digest, crypto.SHA256)
}
