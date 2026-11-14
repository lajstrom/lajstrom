package webhook

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// secretPrefix starts the text of a secret, as Standard Webhooks writes one: its verifiers take
// the base64 after it as the key.
const secretPrefix = "whsec_"

// secretBytes is the length of a key: 256 bits.
const secretBytes = 32

// ErrNoMasterKey is the error of a webhook secret without a master key to seal or open it with.
var ErrNoMasterKey = errors.New("webhook: webhooks need the master key (keys.masterKeyEnv or keys.masterKeyFile)")

// NewSecret returns a new secret: its text, whsec_ and the base64 of the key, shown once; and the
// key, which signs.
func NewSecret() (text string, key []byte) {
	key = make([]byte, secretBytes)
	_, _ = rand.Read(key) // crypto/rand.Read never returns an error; it crashes the program instead
	return secretPrefix + base64.StdEncoding.EncodeToString(key), key
}

// ParseSecret returns the key of the text of a secret.
func ParseSecret(text string) ([]byte, error) {
	b64, ok := strings.CutPrefix(text, secretPrefix)
	key, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(key) == 0 {
		return nil, errors.New("webhook: not a secret: want whsec_ and base64")
	}
	return key, nil
}

// secretAAD is the additional data of the sealed secret of the webhook uid: a ciphertext moved to
// another webhook's row does not open.
func secretAAD(uid string) string { return "webhook:" + uid }

// Seal returns the key sealed with the master key for the webhook uid (index.Webhook.SecretEnc).
func Seal(m *keys.Master, uid string, key []byte) ([]byte, error) {
	if m == nil {
		return nil, ErrNoMasterKey
	}
	return m.Seal(secretAAD(uid), key), nil
}

// Open returns the key of the webhook h, sealed with the master key.
func Open(m *keys.Master, h index.Webhook) ([]byte, error) {
	if m == nil {
		return nil, ErrNoMasterKey
	}
	key, err := m.Open(secretAAD(h.UID), h.SecretEnc)
	if err != nil {
		return nil, fmt.Errorf("webhook %d: the secret does not open with this master key: %w", h.ID, err)
	}
	return key, nil
}
