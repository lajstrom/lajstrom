package keys

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// KMS is a key management service that holds signing keys and signs digests with them, so that
// the private key never leaves it (ADR 0020): internal/keys/awskms and internal/keys/gcpkms.
// The Service builds an OpenPGP key around the KMS's public key and sends it the digest of each
// signature. Errors wrap ErrKMSUnavailable when asking again later may succeed, and
// ErrKMSKeyUnsupported for a key that cannot sign as the Service needs.
type KMS interface {
	// Key describes the key of name, a config.KMSKey's Name: its canonical name, which Sign
	// gets from then on (an AWS alias resolved to the key's ARN, so that moving the alias does
	// not change what signs), its public key, its creation time, and whether it signs PKCS #1
	// v1.5 or ECDSA with SHA-256; an error wrapping ErrKMSKeyUnsupported names the spec of a
	// key that does not.
	Key(ctx context.Context, name string) (*KMSKey, error)
	// Sign returns the signature of digest, a SHA-256 hash, by the key of the canonical name,
	// whose public key pub (what Key described) says how it signs: PKCS #1 v1.5 for an RSA key,
	// ECDSA in ASN.1 DER for an ECDSA key.
	Sign(ctx context.Context, name string, pub crypto.PublicKey, digest []byte) ([]byte, error)
}

// KMSKey describes a key of a KMS.
type KMSKey struct {
	// Name is the canonical name of the key in the KMS: an AWS key ARN, a GCP key version.
	Name string
	// Public is an *rsa.PublicKey of 2048 bits or more, or an *ecdsa.PublicKey on P-256.
	Public crypto.PublicKey
	// Created is when the KMS made the key: the creation time of the OpenPGP key, so that its
	// fingerprint depends on the KMS key alone.
	Created time.Time
}

var (
	// ErrKMSUnavailable means the KMS did not answer, timed out, failed or asked to wait: the
	// signed routes answer 503 with Retry-After (format.ErrUnavailable).
	ErrKMSUnavailable = errors.New("keys: the KMS is unavailable")
	// ErrKMSKeyUnsupported means the KMS key cannot be a repo's signing key: another usage, an
	// algorithm OpenPGP clients do not verify or this package does not sign with, a key that is
	// not enabled.
	ErrKMSKeyUnsupported = errors.New("keys: the KMS key cannot sign apt and rpm indexes")
)

// KMSRetryAfter is the Retry-After of a signed route while the KMS is unavailable.
const KMSRetryAfter = 5 * time.Second

// Local is the source of RotateTo for a key that Lajstrom makes, as without keys.kms.
const Local = "local"

// unavailable returns err, wrapped in a *format.UnavailableError of KMSRetryAfter when it
// wraps ErrKMSUnavailable, so that the routes answer 503 with Retry-After.
func unavailable(err error) error {
	if err == nil || !errors.Is(err, ErrKMSUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", err, &format.UnavailableError{RetryAfter: KMSRetryAfter})
}

// kmsSigner is the crypto.Signer of a key in a KMS for the signatures of one call: go-crypto's
// packet.Signature.Sign calls it for RSA and ECDSA keys. Every signature the KMS answers is
// checked with the public key of the row before it is used. go-crypto drops the error of the
// signer in its RSA branch, so the first error is kept here as well, and the caller checks it
// (failed) after signing: a failure never becomes an empty signature packet.
type kmsSigner struct {
	ctx  context.Context // crypto.Signer takes no context: the signer lives for one call
	kms  KMS
	key  config.KMSKey // the canonical key URI, for the errors and the logs
	pub  crypto.PublicKey
	err  error
	used int // signatures asked for
}

// Public returns the public key.
func (k *kmsSigner) Public() crypto.PublicKey { return k.pub }

// Sign asks the KMS for the signature of digest, a SHA-256 hash, and checks it.
func (k *kmsSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	k.used++
	var sig []byte
	var err error
	if opts.HashFunc() != crypto.SHA256 {
		err = fmt.Errorf("the signature's hash is %v, not SHA-256", opts.HashFunc())
	} else if sig, err = k.kms.Sign(k.ctx, k.key.Name, k.pub, digest); err == nil {
		err = verifyDigest(k.pub, digest, sig)
	}
	if err != nil {
		err = fmt.Errorf("keys: sign with %s: %w", k.key, err)
		if k.err == nil {
			k.err = err
		}
		return nil, err
	}
	return sig, nil
}

// failed returns the first error of the signer since it was made: nil when every signature it
// was asked for came back and verified.
func (k *kmsSigner) failed() error { return k.err }

// verifyDigest checks sig, the KMS's signature of digest, with pub.
func verifyDigest(pub crypto.PublicKey, digest, sig []byte) error {
	switch pub := pub.(type) {
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest, sig) == nil {
			return nil
		}
	case *ecdsa.PublicKey:
		if ecdsa.VerifyASN1(pub, digest, sig) {
			return nil
		}
	default:
		return fmt.Errorf("a public key of type %T", pub)
	}
	return errors.New("the KMS answered a signature that the key's public key does not verify")
}

// checkKMSPublic checks that pub can be the primary key of a repo's OpenPGP key: RSA of 2048
// bits or more, which apt's gpgv takes, or ECDSA on P-256. go-crypto signs neither EdDSA nor
// with another hash than the repo's other keys through a crypto.Signer.
func checkKMSPublic(pub crypto.PublicKey) error {
	switch pub := pub.(type) {
	case *rsa.PublicKey:
		if pub.N.BitLen() < 2048 {
			return fmt.Errorf("%w: an RSA key of %d bits, not 2048 or more", ErrKMSKeyUnsupported, pub.N.BitLen())
		}
		return nil
	case *ecdsa.PublicKey:
		if pub.Curve != elliptic.P256() {
			return fmt.Errorf("%w: an ECDSA key on %s, not P-256", ErrKMSKeyUnsupported, pub.Curve.Params().Name)
		}
		return nil
	}
	return fmt.Errorf("%w: a public key of type %T, not RSA or ECDSA", ErrKMSKeyUnsupported, pub)
}

// oidP256 is the OID of NIST P-256 in an OpenPGP key packet, without its tag and length
// (RFC 9580 §9.2).
var oidP256 = [8]byte{0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}

const (
	// p256Point is the length of an uncompressed point of P-256: 0x04, X, Y.
	p256Point = 65
	// p256KeyBody is the length of the body of a v4 public key packet of ECDSA on P-256: the
	// version, the creation time, the algorithm, the OID with its length, the point as an MPI.
	p256KeyBody = 1 + 4 + 1 + 1 + len(oidP256) + 2 + p256Point
)

// openPGPPublic returns the v4 OpenPGP public key of pub, created at created. go-crypto takes an
// ECDSA key only in a type of its own whose curve comes from an internal package, so the packet
// of a P-256 key is written here and read back.
func openPGPPublic(pub crypto.PublicKey, created time.Time) (*packet.PublicKey, error) {
	switch pub := pub.(type) {
	case *rsa.PublicKey:
		return packet.NewRSAPublicKey(created, pub), nil
	case *ecdsa.PublicKey:
		point, err := pub.Bytes()
		if err != nil {
			return nil, fmt.Errorf("keys: the KMS's public key: %w", err)
		}
		if len(point) != p256Point {
			return nil, fmt.Errorf("%w: a point of %d bytes, not of P-256", ErrKMSKeyUnsupported, len(point))
		}
		pkt := []byte{0xc0 | 6, byte(p256KeyBody), 4, 0, 0, 0, 0, byte(packet.PubKeyAlgoECDSA), byte(len(oidP256))}
		binary.BigEndian.PutUint32(pkt[3:7], uint32(created.Unix())) //nolint:gosec // a creation time after 1970 and before 2106
		pkt = append(pkt, oidP256[:]...)
		// The point is an MPI: its length in bits, then its bytes; it starts with 0x04, three bits.
		pkt = binary.BigEndian.AppendUint16(pkt, (p256Point-1)*8+3)
		pkt = append(pkt, point...)
		p, err := packet.Read(bytes.NewReader(pkt))
		if err != nil {
			return nil, fmt.Errorf("keys: the OpenPGP key of the KMS's public key: %w", err)
		}
		pk, ok := p.(*packet.PublicKey)
		if !ok {
			return nil, fmt.Errorf("keys: the OpenPGP key of the KMS's public key read back as %T", p)
		}
		return pk, nil
	}
	return nil, fmt.Errorf("%w: a public key of type %T", ErrKMSKeyUnsupported, pub)
}

// stdPublic returns the public key of an OpenPGP primary key that a KMS holds, in the types of
// the standard library, which verifyDigest takes.
func stdPublic(pk *packet.PublicKey) (crypto.PublicKey, error) {
	if pub, ok := pk.PublicKey.(*rsa.PublicKey); ok && pk.PubKeyAlgo == packet.PubKeyAlgoRSA {
		return pub, nil
	}
	type point interface{ MarshalPoint() []byte }
	if p, ok := pk.PublicKey.(point); ok && pk.PubKeyAlgo == packet.PubKeyAlgoECDSA {
		if curve, err := pk.Curve(); err == nil && curve == packet.CurveNistP256 {
			return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), p.MarshalPoint())
		}
	}
	return nil, fmt.Errorf("keys: the key %X is not an RSA or P-256 key that a KMS holds", pk.Fingerprint)
}

// kmsOf returns the KMS of the scheme of key, an error when the server has none.
func (s *Service) kmsOf(key config.KMSKey) (KMS, error) {
	if k := s.kms[key.Scheme]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("keys: %s: this server has no client for %s", key, key.Scheme)
}

// generateKMS makes a key for repo held in the KMS of uri, as KeyFor describes it for a key
// made here, and returns its row, not yet stored, with the canonical key URI sealed by the
// master key in PrivateEnc, and the key itself. The OpenPGP key's creation time is the KMS
// key's, to the second, or now when the KMS's clock is ahead; its self-signature is the KMS's,
// made now.
func (s *Service) generateKMS(ctx context.Context, repo index.Repo, uri string) (*index.Key, *Entity, error) {
	key, err := config.ParseKMSKey(uri)
	if err != nil {
		return nil, nil, fmt.Errorf("keys: %w", err)
	}
	kms, err := s.kmsOf(key)
	if err != nil {
		return nil, nil, err
	}
	desc, err := kms.Key(ctx, key.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("keys: the KMS key %s: %w", key, err)
	}
	if err := checkKMSPublic(desc.Public); err != nil {
		return nil, nil, fmt.Errorf("keys: the KMS key %s: %w", key, err)
	}
	canonical := config.KMSKey{Scheme: key.Scheme, Name: desc.Name}
	now := s.now().UTC().Truncate(time.Second)
	created := desc.Created.UTC().Truncate(time.Second)
	if created.After(now) {
		created = now
	}
	pub, err := openPGPPublic(desc.Public, created)
	if err != nil {
		return nil, nil, err
	}
	signer := &kmsSigner{ctx: ctx, kms: kms, key: canonical, pub: desc.Public}
	e := &openpgp.Entity{PrimaryKey: pub, PrivateKey: &packet.PrivateKey{PublicKey: *pub, PrivateKey: signer},
		Identities: map[string]*openpgp.Identity{}}
	err = e.AddUserId("Lajstrom "+repoName(repo), "", "", &packet.Config{DefaultHash: crypto.SHA256, Time: func() time.Time { return now }})
	if err = errors.Join(err, signer.failed()); err != nil {
		return nil, nil, fmt.Errorf("keys: the self-signature of the key of %s in %s: %w", repoName(repo), canonical, err)
	}
	var public bytes.Buffer
	if err := e.Serialize(&public); err != nil {
		return nil, nil, fmt.Errorf("keys: serialize the key of %s in %s: %w", repoName(repo), canonical, err)
	}
	fp := fingerprint(e)
	id := repo.ID
	row := &index.Key{RepoID: &id, Kind: KindGPG, Public: public.Bytes(), PrivateEnc: s.master.seal(fp, []byte(canonical.String())),
		Fingerprint: fp, CreatedAt: created}
	return row, &Entity{Key: *row, OpenPGP: e, kms: signer}, nil
}

// kmsKeyOf returns the key URI that plain, the opened PrivateEnc of a row, holds, and false
// when plain is a private key: an OpenPGP packet starts with a byte whose high bit is set,
// never with the letter of a scheme.
func kmsKeyOf(plain []byte) (config.KMSKey, bool, error) {
	if len(plain) == 0 || plain[0]&0x80 != 0 {
		return config.KMSKey{}, false, nil
	}
	key, err := config.ParseKMSKey(string(plain))
	return key, true, err
}

// openKMS returns the key of row k, whose sealed PrivateEnc held the key URI key: the OpenPGP
// key of its public packets, checked to be the key of k's fingerprint, signing through the KMS
// with ctx.
func (s *Service) openKMS(ctx context.Context, k index.Key, key config.KMSKey) (*Entity, error) {
	kms, err := s.kmsOf(key)
	if err != nil {
		return nil, err
	}
	e, err := openpgp.ReadEntity(packet.NewReader(bytes.NewReader(k.Public)))
	switch {
	case err != nil:
		return nil, fmt.Errorf("keys: the public key of %s: %w", k.Fingerprint, err)
	case fingerprint(e) != k.Fingerprint:
		return nil, fmt.Errorf("keys: the row of %s holds the key %s", k.Fingerprint, fingerprint(e))
	}
	pub, err := stdPublic(e.PrimaryKey)
	if err != nil {
		return nil, err
	}
	signer := &kmsSigner{ctx: ctx, kms: kms, key: key, pub: pub}
	e.PrivateKey = &packet.PrivateKey{PublicKey: *e.PrimaryKey, PrivateKey: signer}
	return &Entity{Key: k, OpenPGP: e, kms: signer}, nil
}
