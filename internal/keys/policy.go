package keys

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// KindPolicy is the kind of the server's policy key (docs/spec/policy.md §5): the ECDSA P-256 key
// that signs the verdicts the server makes and the deny list.
const KindPolicy = "policy"

// PolicyKey is the server's policy key. It prints as its fingerprint, never with the key
// material.
type PolicyKey struct {
	// Key is its row in keys.
	Key index.Key
	// Public is its public key, an *ecdsa.PublicKey on P-256.
	Public crypto.PublicKey

	private *ecdsa.PrivateKey // a key made here
	kms     KMS               // or the KMS that holds it, under kmsName
	kmsName string
}

// String is the key's fingerprint, never its material.
func (k *PolicyKey) String() string { return "policy key " + k.Key.Fingerprint }

// GoString is String.
func (k *PolicyKey) GoString() string { return k.String() }

// LogValue is the fingerprint.
func (k *PolicyKey) LogValue() slog.Value { return slog.StringValue(k.Key.Fingerprint) }

// Fingerprint is the hex sha256 of the public key's PKIX DER.
func (k *PolicyKey) Fingerprint() string { return k.Key.Fingerprint }

// PublicDER is the public key's PKIX DER.
func (k *PolicyKey) PublicDER() []byte { return k.Key.Public }

func (k *PolicyKey) hashOf(msg []byte) []byte {
	s := sha256.Sum256(msg)
	return s[:]
}

// PEM is the public key as a PEM PUBLIC KEY block (PKIX): what cosign verify-attestation --key
// and cosign verify-blob --key take.
func (k *PolicyKey) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.Key.Public})
}

// Sign returns the ECDSA signature, ASN.1 DER, of the SHA-256 of msg: by the key itself, or by the
// KMS that holds it, with ctx.
func (k *PolicyKey) Sign(ctx context.Context, msg []byte) ([]byte, error) {
	digest := k.hashOf(msg)
	if k.private != nil {
		return ecdsa.SignASN1(rand.Reader, k.private, digest)
	}
	sig, err := k.kms.Sign(ctx, k.kmsName, k.Public, digest)
	if err != nil {
		return nil, unavailable(fmt.Errorf("keys: the policy key %s in the KMS: %w", k.Key.Fingerprint, err))
	}
	return sig, nil
}

// PolicyKey returns the server's policy key, made now when the index has none (one replica's
// insert wins, keys_active): an ECDSA P-256 key made here and sealed with the master key, or,
// when uri is a KMS key URI of ADR 0020, that key, whose canonical URI the row seals instead.
// uri "" or Local makes the key here. A row that does not open is an error naming its
// fingerprint.
func (s *Service) PolicyKey(ctx context.Context, uri string) (*PolicyKey, error) {
	row, err := s.st.GetKey(ctx, nil, KindPolicy)
	if errors.Is(err, index.ErrNotFound) {
		row, err = s.newPolicyKey(ctx, uri)
	}
	if err != nil {
		return nil, err
	}
	return s.openPolicyKey(*row)
}

// newPolicyKey makes and stores the policy key of uri, or reads the one another replica stored
// first.
func (s *Service) newPolicyKey(ctx context.Context, uri string) (*index.Key, error) {
	var row *index.Key
	var err error
	if uri == "" || uri == Local {
		row, err = s.localPolicyKey()
	} else {
		row, err = s.kmsPolicyKey(ctx, uri)
	}
	if err != nil {
		return nil, err
	}
	err = s.st.CreateKey(ctx, row)
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "policy key made", "fingerprint", row.Fingerprint, "source", sourceName(uri))
		return row, nil
	case !errors.Is(err, index.ErrConflict):
		return nil, fmt.Errorf("keys: store the policy key: %w", err)
	}
	return s.st.GetKey(ctx, nil, KindPolicy)
}

// sourceName is the source of a key as a log line names it.
func sourceName(uri string) string {
	if uri == "" {
		return Local
	}
	return uri
}

// localPolicyKey makes an ECDSA P-256 key, its row with the PKCS #8 DER sealed.
func (s *Service) localPolicyKey() (*index.Key, error) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader) // P-256 and crypto/rand: never an error
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("keys: serialize the policy key: %w", err)
	}
	defer clear(der)
	row, err := policyRow(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	row.PrivateEnc = s.master.seal(row.Fingerprint, der)
	row.CreatedAt = s.now().UTC()
	return row, nil
}

// kmsPolicyKey describes the KMS key of uri, which must be ECDSA on P-256, its row with the
// canonical key URI sealed.
func (s *Service) kmsPolicyKey(ctx context.Context, uri string) (*index.Key, error) {
	key, err := config.ParseKMSKey(uri)
	if err != nil {
		return nil, fmt.Errorf("keys: policy.signingKey: %w", err)
	}
	kms, err := s.kmsOf(key)
	if err != nil {
		return nil, err
	}
	desc, err := kms.Key(ctx, key.Name)
	if err != nil {
		return nil, fmt.Errorf("keys: the KMS key %s: %w", key, err)
	}
	pub, ok := desc.Public.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("keys: the KMS key %s: %w: the policy key is ECDSA on P-256", key, ErrKMSKeyUnsupported)
	}
	row, err := policyRow(pub)
	if err != nil {
		return nil, err
	}
	canonical := config.KMSKey{Scheme: key.Scheme, Name: desc.Name}
	row.PrivateEnc = s.master.seal(row.Fingerprint, []byte(canonical.String()))
	row.CreatedAt = s.now().UTC()
	return row, nil
}

// policyRow is the row of the policy key pub, without its private part.
func policyRow(pub *ecdsa.PublicKey) (*index.Key, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("keys: serialize the public policy key: %w", err)
	}
	sum := sha256.Sum256(der)
	return &index.Key{Kind: KindPolicy, Public: der, Fingerprint: hex.EncodeToString(sum[:])}, nil
}

// openPolicyKey opens the row k: a PKCS #8 key checked to be the key of its public key, or a KMS
// key URI.
func (s *Service) openPolicyKey(k index.Key) (*PolicyKey, error) {
	pubAny, err := x509.ParsePKIXPublicKey(k.Public)
	if err != nil {
		return nil, fmt.Errorf("keys: the public key of the policy key %s: %w", k.Fingerprint, err)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("keys: the policy key %s is a %T, not ECDSA", k.Fingerprint, pubAny)
	}
	plain, err := s.master.open(k.Fingerprint, k.PrivateEnc)
	if err != nil {
		return nil, fmt.Errorf("keys: the policy key %s: %w", k.Fingerprint, err)
	}
	defer clear(plain)
	if parsed, err := x509.ParsePKCS8PrivateKey(plain); err == nil {
		priv, ok := parsed.(*ecdsa.PrivateKey)
		if !ok || !priv.PublicKey.Equal(pub) {
			return nil, fmt.Errorf("keys: the row of the policy key %s holds another key", k.Fingerprint)
		}
		return &PolicyKey{Key: k, Public: pub, private: priv}, nil
	}
	key, err := config.ParseKMSKey(string(plain))
	if err != nil {
		return nil, fmt.Errorf("keys: the row of the policy key %s holds neither a key nor a key URI", k.Fingerprint)
	}
	kms, err := s.kmsOf(key)
	if err != nil {
		return nil, err
	}
	return &PolicyKey{Key: k, Public: pub, kms: kms, kmsName: key.Name}, nil
}
