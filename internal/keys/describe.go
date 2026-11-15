package keys

import (
	"bytes"
	"fmt"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Algorithm names the algorithm of an OpenPGP public key, the Public of a gpg row of keys, as
// the management API answers it (docs/api.md, Repos): ed25519 or rsa4096 for a key made here,
// rsa2048, rsa3072, rsa4096 or ecdsa-p256 for one a KMS holds (ADR 0020). Another key is named
// by its OpenPGP algorithm and size, and bytes that hold no public key are an error.
func Algorithm(public []byte) (string, error) {
	p, err := packet.NewReader(bytes.NewReader(public)).Next()
	if err != nil {
		return "", fmt.Errorf("keys: read the public key: %w", err)
	}
	pk, ok := p.(*packet.PublicKey)
	if !ok {
		return "", fmt.Errorf("keys: the first packet of the public key is a %T", p)
	}
	algo := pk.PubKeyAlgo
	if algo == packet.PubKeyAlgoRSA || algo == packet.PubKeyAlgoRSASignOnly {
		bits, err := pk.BitLength()
		if err != nil {
			return "", fmt.Errorf("keys: the size of RSA key %X: %w", pk.Fingerprint, err)
		}
		return fmt.Sprintf("rsa%d", bits), nil
	}
	if algo == packet.PubKeyAlgoEdDSA || algo == packet.PubKeyAlgoEd25519 {
		return "ed25519", nil
	}
	if curve, err := pk.Curve(); algo == packet.PubKeyAlgoECDSA && err == nil && curve == packet.CurveNistP256 {
		return "ecdsa-p256", nil
	}
	bits, _ := pk.BitLength()
	return fmt.Sprintf("openpgp-%d-%d", pk.PubKeyAlgo, bits), nil
}
