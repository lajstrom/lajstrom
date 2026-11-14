// Package attesttest makes signed attachments in memory for the tests of ingest, reindex and the
// API (#89): a key, in-toto statements, DSSE envelopes and sigstore bundles as cosign writes
// them. Non-test code does not import it; it is a normal package so that the tests of other
// packages can.
package attesttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/attest"
)

// The media types cosign writes: v3's bundle, and the layer of a legacy .att.
const (
	BundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	DSSEMediaType   = "application/vnd.dsse.envelope.v1+json"
)

// Key is an ECDSA P-256 key, the kind cosign generate-key-pair makes.
type Key struct{ priv *ecdsa.PrivateKey }

// NewKey makes a key.
func NewKey(t testing.TB) *Key {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{priv}
}

// PublicPEM is the key's public half as cosign.pub has it.
func (k *Key) PublicPEM(t testing.TB) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// Entry is a trust entry of the key.
func (k *Key) Entry(t testing.TB, name string, predicateTypes ...string) attest.Entry {
	t.Helper()
	return attest.Entry{Name: name, PublicKey: k.PublicPEM(t), PredicateTypes: predicateTypes}
}

// sign signs msg as cosign does: ECDSA over its SHA-256, ASN.1.
func (k *Key) sign(t testing.TB, msg []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// Statement is an in-toto statement v1 of predicateType about subject, with the predicate JSON
// predicate.
func Statement(subject digest.Digest, predicateType, predicate string) []byte {
	return fmt.Appendf(nil, `{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"artifact","digest":{%q:%q}}],"predicateType":%q,"predicate":%s}`,
		subject.Algorithm().String(), subject.Encoded(), predicateType, predicate)
}

// Envelope is the JSON of a DSSE envelope of the in-toto payload, signed by each of keys.
func Envelope(t testing.TB, payload []byte, keys ...*Key) []byte {
	t.Helper()
	b, err := json.Marshal(envelope(t, payload, keys...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func envelope(t testing.TB, payload []byte, keys ...*Key) map[string]any {
	t.Helper()
	const payloadType = "application/vnd.in-toto+json"
	pae := fmt.Appendf(nil, "DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload)
	sigs := []map[string]string{}
	for _, k := range keys {
		sigs = append(sigs, map[string]string{"keyid": "", "sig": base64.StdEncoding.EncodeToString(k.sign(t, pae))})
	}
	return map[string]any{"payloadType": payloadType, "payload": base64.StdEncoding.EncodeToString(payload), "signatures": sigs}
}

// Bundle is a sigstore bundle v0.3 around the DSSE envelope of payload, signed by each of keys,
// as cosign v3 attest --key writes it.
func Bundle(t testing.TB, payload []byte, keys ...*Key) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"mediaType": BundleMediaType,
		"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": ""}},
		"dsseEnvelope":         envelope(t, payload, keys...)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
