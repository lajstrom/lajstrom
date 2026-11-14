package attesttest

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

// Sigstore is a Sigstore of the test, in memory: a Fulcio, a Rekor, a timestamp authority and a
// CT log of its own (sigstore-go's VirtualSigstore). It signs keyless bundles as cosign v3's
// attest without --key writes them, and has the trusted_root.json that verifies them.
type Sigstore struct{ vs *ca.VirtualSigstore }

// NewSigstore makes one.
func NewSigstore(t testing.TB) *Sigstore {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	return &Sigstore{vs}
}

// TrustedRoot is the trusted_root.json of the Sigstore.
func (s *Sigstore) TrustedRoot(t testing.TB) []byte {
	t.Helper()
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01, s.vs.FulcioCertificateAuthorities(), rawLogIDs(t, s.vs.CTLogs()),
		s.vs.TimestampingAuthorities(), rawLogIDs(t, s.vs.RekorLogs()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := tr.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawLogIDs returns copies of the logs with the IDs a trusted_root.json carries: the sha256 of
// the key, which VirtualSigstore keeps as its hex text (the key of its map), so that the file
// reads back with the IDs of the log entries.
func rawLogIDs(t testing.TB, logs map[string]*root.TransparencyLog) map[string]*root.TransparencyLog {
	t.Helper()
	out := map[string]*root.TransparencyLog{}
	for id, l := range logs {
		raw, err := hex.DecodeString(id)
		if err != nil {
			t.Fatal(err)
		}
		c := *l
		c.ID = raw
		out[id] = &c
	}
	return out
}

// Bundle is a sigstore bundle v0.3 of the in-toto payload, signed keyless by subject (the
// certificate's subject alternative name, an email address) as the OIDC issuer issuer names it:
// a Fulcio certificate, a DSSE envelope, a Rekor entry with its inclusion proof and checkpoint,
// and an RFC 3161 timestamp. Its certificate carries no SCT.
func (s *Sigstore) Bundle(t testing.TB, issuer, subject string, payload []byte) []byte {
	t.Helper()
	e, err := s.vs.AttestAtTime(subject, issuer, payload, time.Now().Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := e.VerificationContent()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := e.SignatureContent()
	if err != nil {
		t.Fatal(err)
	}
	env := sc.(*sgbundle.Envelope).Envelope
	body, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	pe := &protodsse.Envelope{Payload: body, PayloadType: env.PayloadType}
	for _, sig := range env.Signatures {
		raw, err := base64.StdEncoding.DecodeString(sig.Sig)
		if err != nil {
			t.Fatal(err)
		}
		pe.Signatures = append(pe.Signatures, &protodsse.Signature{Sig: raw, Keyid: sig.KeyID})
	}
	entries, err := e.TlogEntries()
	if err != nil {
		t.Fatal(err)
	}
	material := &protobundle.VerificationMaterial{
		Content:                   &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: vc.Certificate().Raw}},
		TimestampVerificationData: &protobundle.TimestampVerificationData{},
	}
	for _, entry := range entries {
		tle := entry.TransparencyLogEntry()
		var kind struct{ Kind, APIVersion string }
		if err := json.Unmarshal(tle.GetCanonicalizedBody(), &kind); err != nil {
			t.Fatal(err)
		}
		tle.KindVersion = &protorekor.KindVersion{Kind: kind.Kind, Version: kind.APIVersion}
		material.TlogEntries = append(material.TlogEntries, tle)
	}
	stamps, err := e.Timestamps()
	if err != nil {
		t.Fatal(err)
	}
	for _, ts := range stamps {
		material.TimestampVerificationData.Rfc3161Timestamps = append(material.TimestampVerificationData.Rfc3161Timestamps,
			&protocommon.RFC3161SignedTimestamp{SignedTimestamp: ts})
	}
	b, err := sgbundle.NewBundle(&protobundle.Bundle{MediaType: BundleMediaType, VerificationMaterial: material,
		Content: &protobundle.Bundle_DsseEnvelope{DsseEnvelope: pe}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
