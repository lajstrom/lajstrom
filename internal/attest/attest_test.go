package attest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	cyclonedx = "https://cyclonedx.org/bom"
	vuln      = "https://cosign.sigstore.dev/attestation/vuln/v1"
	bundleV3  = "application/vnd.dev.sigstore.bundle.v0.3+json"
)

// signer is a key made for the test: it signs as cosign does, and its public half is PEM.
type signer struct {
	priv crypto.Signer
}

func newSigner(t *testing.T, alg string) signer {
	t.Helper()
	var priv crypto.Signer
	var err error
	switch alg {
	case "p256":
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "p384":
		priv, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "p521":
		priv, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	case "ed25519":
		_, priv, err = ed25519.GenerateKey(rand.Reader)
	case "rsa":
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
	default:
		t.Fatalf("no algorithm %s", alg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return signer{priv}
}

func (s signer) pem(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(s.priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func (s signer) sign(t *testing.T, msg []byte) []byte {
	t.Helper()
	var sig []byte
	var err error
	switch k := s.priv.(type) {
	case *ecdsa.PrivateKey:
		var sum []byte
		switch k.Curve {
		case elliptic.P256():
			h := sha256.Sum256(msg)
			sum = h[:]
		case elliptic.P384():
			h := sha512.Sum384(msg)
			sum = h[:]
		default:
			h := sha512.Sum512(msg)
			sum = h[:]
		}
		sig, err = ecdsa.SignASN1(rand.Reader, k, sum)
	case ed25519.PrivateKey:
		sig = ed25519.Sign(k, msg)
	case *rsa.PrivateKey:
		h := sha256.Sum256(msg)
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, h[:])
	}
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// statementOf is an in-toto statement of predicateType about subject.
func statementOf(subject digest.Digest, predicateType string, predicate string) []byte {
	return []byte(`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"x","digest":{"` +
		subject.Algorithm().String() + `":"` + subject.Encoded() + `"}}],"predicateType":"` + predicateType + `","predicate":` + predicate + `}`)
}

// dsse is the JSON of a DSSE envelope of payload, signed by each of by.
func dsse(t *testing.T, payloadType string, payload []byte, by ...signer) map[string]any {
	t.Helper()
	sigs := []map[string]string{}
	for _, s := range by {
		sigs = append(sigs, map[string]string{"keyid": "", "sig": base64.StdEncoding.EncodeToString(s.sign(t, pae(payloadType, payload)))})
	}
	return map[string]any{"payloadType": payloadType, "payload": base64.StdEncoding.EncodeToString(payload), "signatures": sigs}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bundleOf is a sigstore bundle v0.3 around the envelope env, with a public key hint, or a
// certificate when keyless.
func bundleOf(t *testing.T, env map[string]any, keyless bool) []byte {
	t.Helper()
	material := map[string]any{"publicKey": map[string]string{"hint": ""}}
	if keyless {
		material = map[string]any{"certificate": map[string]string{"rawBytes": base64.StdEncoding.EncodeToString([]byte("not a certificate"))}}
	}
	return mustJSON(t, map[string]any{"mediaType": bundleV3, "verificationMaterial": material, "dsseEnvelope": env})
}

// TestVerify is the verification table of #89: a trusted key, an untrusted key, no signature,
// and each way an attachment fails, in each form cosign writes.
func TestVerify(t *testing.T) {
	subject := digest.FromString("the version's manifest")
	other := digest.FromString("another manifest")
	scanner, ci, stranger := newSigner(t, "p256"), newSigner(t, "p256"), newSigner(t, "p256")
	trust, err := NewTrust([]Entry{
		{Name: "scanner", PublicKey: scanner.pem(t), PredicateTypes: []string{cyclonedx, vuln}},
		{Name: "ci", PublicKey: ci.pem(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sbom := statementOf(subject, cyclonedx, `{"bomFormat":"CycloneDX"}`)
	sigPayload := statementOf(subject, PredicateCosignSign, `{}`)
	simple := []byte(`{"critical":{"identity":{"docker-reference":"x"},"image":{"docker-manifest-digest":"` + subject.String() +
		`"},"type":"cosign container image signature"},"optional":null}`)
	simpleOther := []byte(strings.ReplaceAll(string(simple), subject.String(), other.String()))
	tampered := dsse(t, payloadTypeInToto, sbom, scanner)
	tampered["payload"] = base64.StdEncoding.EncodeToString(statementOf(subject, cyclonedx, `{"bomFormat":"forged"}`))
	layer := func(mediaType string, annotations ...string) ocispec.Descriptor {
		d := ocispec.Descriptor{MediaType: mediaType, Annotations: map[string]string{}}
		for i := 0; i+1 < len(annotations); i += 2 {
			d.Annotations[annotations[i]] = annotations[i+1]
		}
		return d
	}
	sig := func(s signer, msg []byte) string { return base64.StdEncoding.EncodeToString(s.sign(t, msg)) }
	for _, tt := range []struct {
		name      string
		layer     ocispec.Descriptor
		content   []byte
		annotated string
		want      string
		signedBy  string
		predicate string // the PredicateType of the result
		reason    string // a part of Reason
	}{
		{"bundle, a trusted key", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sbom, scanner), false), "", Verified, "scanner", cyclonedx, ""},
		{"bundle, a key trusted for every type", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sbom, ci), false), "", Verified, "ci", cyclonedx, ""},
		{"bundle, two signatures, one trusted", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sbom, stranger, scanner), false), "", Verified, "scanner", cyclonedx, ""},
		{"bundle, older media type", layer("application/vnd.dev.sigstore.bundle+json;version=0.2"), bundleOf(t, dsse(t, payloadTypeInToto, sbom, ci), false), "", Verified, "ci", cyclonedx, ""},
		{"bundle, cosign v3's signature", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sigPayload, ci), false), PredicateCosignSign, Verified, "ci", PredicateCosignSign, ""},
		{"bundle, an untrusted key", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sbom, stranger), false), "", Untrusted, "", cyclonedx, "no trusted key"},
		{"bundle, a key not trusted for the type", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sigPayload, scanner), false), "", Untrusted, "", PredicateCosignSign, "not trusted for the predicate type"},
		{"bundle, no signature", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sbom), false), cyclonedx, Unsigned, "", cyclonedx, "no signature"},
		{"bundle, a tampered payload", layer(bundleV3), bundleOf(t, tampered, false), "", Untrusted, "", cyclonedx, "no trusted key"},
		{"bundle, another subject", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, statementOf(other, cyclonedx, `{}`), scanner), false), "", Invalid, "", cyclonedx, "does not name " + subject.String()},
		{"bundle, keyless", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sbom, stranger), true), "", Unsupported, "", cyclonedx, "keyless"},
		{"bundle, keyless and a trusted key", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, sbom, scanner), true), "", Verified, "scanner", cyclonedx, ""},
		{"bundle, a message signature", layer(bundleV3), []byte(`{"mediaType":"` + bundleV3 + `","messageSignature":{"signature":"AA=="}}`), "x", Unsupported, "", "x", "message signature"},
		{"bundle, empty", layer(bundleV3), []byte(`{"mediaType":"` + bundleV3 + `"}`), "", Invalid, "", "", "neither"},
		{"bundle, not JSON", layer(bundleV3), []byte("garbage"), "x", Invalid, "", "x", "does not parse"},
		{"bundle, not in-toto", layer(bundleV3), bundleOf(t, dsse(t, "text/plain", sbom, scanner), false), "", Invalid, "", "", "payload type"},
		{"bundle, not a statement", layer(bundleV3), bundleOf(t, dsse(t, payloadTypeInToto, []byte(`{"a":1}`), scanner), false), "", Invalid, "", "", "not an in-toto statement"},
		{"bundle, payload not base64", layer(bundleV3), []byte(`{"dsseEnvelope":{"payloadType":"` + payloadTypeInToto + `","payload":"!!","signatures":[]}}`), "", Invalid, "", "", "base64"},
		{"legacy DSSE layer, trusted", layer(mediaTypeDSSE, "predicateType", cyclonedx), mustJSON(t, dsse(t, payloadTypeInToto, sbom, scanner)), cyclonedx, Verified, "scanner", cyclonedx, ""},
		{"legacy DSSE layer, keyless", layer(mediaTypeDSSE, annotationCertificate, "x"), mustJSON(t, dsse(t, payloadTypeInToto, sbom, stranger)), "", Unsupported, "", cyclonedx, "keyless"},
		{"legacy DSSE layer, not JSON", layer(mediaTypeDSSE), []byte("{"), "", Invalid, "", "", "does not parse"},
		{"simple signing, trusted", layer(mediaTypeSimpleSigning, annotationSignature, sig(ci, simple)), simple, "", Verified, "ci", "", ""},
		{"simple signing, a key trusted for attestations only", layer(mediaTypeSimpleSigning, annotationSignature, sig(scanner, simple)), simple, "", Untrusted, "", "", "not trusted for the predicate type"},
		{"simple signing, untrusted", layer(mediaTypeSimpleSigning, annotationSignature, sig(stranger, simple)), simple, "", Untrusted, "", "", "no trusted key"},
		{"simple signing, keyless", layer(mediaTypeSimpleSigning, annotationSignature, sig(stranger, simple), annotationCertificate, "x"), simple, "", Unsupported, "", "", "keyless"},
		{"simple signing, another manifest", layer(mediaTypeSimpleSigning, annotationSignature, sig(ci, simpleOther)), simpleOther, "", Invalid, "", "", "not " + subject.String()},
		{"simple signing, no signature", layer(mediaTypeSimpleSigning), simple, "", Unsigned, "", "", annotationSignature},
		{"simple signing, signature not base64", layer(mediaTypeSimpleSigning, annotationSignature, "!!"), simple, "", Invalid, "", "", "base64"},
		{"simple signing, payload not JSON", layer(mediaTypeSimpleSigning, annotationSignature, "AA=="), []byte("x"), "", Invalid, "", "", "simple signing"},
		{"notation", layer("application/jose+json"), nil, "", Unsupported, "", "", "notation"},
		{"raw SBOM", layer("application/spdx+json"), nil, "", Unsigned, "", "", "no signature"},
		{"unknown layer", layer("application/octet-stream"), nil, "x", Unsigned, "", "x", "no signature"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := trust.Verify(Input{Subject: subject, Layer: tt.layer, Content: tt.content, Annotated: tt.annotated})
			if got.Verification != tt.want || got.SignedBy != tt.signedBy || got.PredicateType != tt.predicate {
				t.Errorf("Verify = %s by %q of %q (%s), want %s by %q of %q", got.Verification, got.SignedBy, got.PredicateType, got.Reason,
					tt.want, tt.signedBy, tt.predicate)
			}
			if (got.Reason == "") != (tt.want == Verified) || !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("Reason = %q, want one with %q", got.Reason, tt.reason)
			}
		})
	}
	t.Run("the predicate", func(t *testing.T) {
		got := trust.Verify(Input{Subject: subject, Layer: layer(bundleV3), Content: bundleOf(t, dsse(t, payloadTypeInToto, sbom, scanner), false)})
		if string(got.Predicate) != `{"bomFormat":"CycloneDX"}` {
			t.Errorf("Predicate = %s", got.Predicate)
		}
	})
	t.Run("no trust", func(t *testing.T) {
		var none *Trust
		got := none.Verify(Input{Subject: subject, Layer: layer(bundleV3), Content: bundleOf(t, dsse(t, payloadTypeInToto, sbom, scanner), false)})
		if got.Verification != Untrusted {
			t.Errorf("Verify without a trust = %s, want %s", got.Verification, Untrusted)
		}
	})
}

// TestKeyAlgorithms: each algorithm of manifest-profile.md §6.3 verifies its own signature and
// refuses another key's.
func TestKeyAlgorithms(t *testing.T) {
	subject := digest.FromString("m")
	payload := statementOf(subject, cyclonedx, `{}`)
	for _, alg := range []string{"p256", "p384", "p521", "ed25519", "rsa"} {
		t.Run(alg, func(t *testing.T) {
			s, other := newSigner(t, alg), newSigner(t, alg)
			trust, err := NewTrust([]Entry{{Name: alg, PublicKey: s.pem(t)}})
			if err != nil {
				t.Fatal(err)
			}
			in := Input{Subject: subject, Layer: ocispec.Descriptor{MediaType: bundleV3}}
			in.Content = bundleOf(t, dsse(t, payloadTypeInToto, payload, s), false)
			if got := trust.Verify(in); got.Verification != Verified || got.SignedBy != alg {
				t.Errorf("its own signature: %s (%s)", got.Verification, got.Reason)
			}
			in.Content = bundleOf(t, dsse(t, payloadTypeInToto, payload, other), false)
			if got := trust.Verify(in); got.Verification != Untrusted {
				t.Errorf("another key's signature: %s", got.Verification)
			}
		})
	}
}

// TestNewTrust: what a trust refuses, and that its hash depends on its entries, not their order.
func TestNewTrust(t *testing.T) {
	a, b := newSigner(t, "p256"), newSigner(t, "ed25519")
	priv, err := x509.MarshalPKCS8PrivateKey(a.priv)
	if err != nil {
		t.Fatal(err)
	}
	dsa := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not a key")})
	for _, tt := range []struct {
		name    string
		entries []Entry
		want    string
	}{
		{"no name", []Entry{{PublicKey: a.pem(t)}}, "no name"},
		{"a name twice", []Entry{{Name: "x", PublicKey: a.pem(t)}, {Name: "x", PublicKey: b.pem(t)}}, "twice"},
		{"not PEM", []Entry{{Name: "x", PublicKey: []byte("ssh-ed25519 AAAA")}}, "not PEM"},
		{"a private key", []Entry{{Name: "x", PublicKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})}}, "not PUBLIC KEY"},
		{"two blocks", []Entry{{Name: "x", PublicKey: append(a.pem(t), b.pem(t)...)}}, "more than one"},
		{"garbage in the block", []Entry{{Name: "x", PublicKey: dsa}}, `"x"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTrust(tt.entries); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("NewTrust = %v, want an error with %q", err, tt.want)
			}
		})
	}

	hash := func(entries ...Entry) string {
		t.Helper()
		trust, err := NewTrust(entries)
		if err != nil {
			t.Fatal(err)
		}
		return trust.Hash()
	}
	ea := Entry{Name: "a", PublicKey: a.pem(t), PredicateTypes: []string{vuln, cyclonedx}}
	eb := Entry{Name: "b", PublicKey: b.pem(t)}
	base := hash(ea, eb)
	if got := hash(eb, Entry{Name: "a", PublicKey: a.pem(t), PredicateTypes: []string{cyclonedx, vuln, vuln}}); got != base {
		t.Errorf("the hash depends on the order of the entries or the types: %s, %s", got, base)
	}
	for name, h := range map[string]string{
		"a type less":   hash(Entry{Name: "a", PublicKey: a.pem(t), PredicateTypes: []string{vuln}}, eb),
		"renamed":       hash(Entry{Name: "c", PublicKey: a.pem(t), PredicateTypes: ea.PredicateTypes}, eb),
		"another key":   hash(Entry{Name: "a", PublicKey: b.pem(t), PredicateTypes: ea.PredicateTypes}, eb),
		"an entry less": hash(ea),
		"none":          hash(),
	} {
		if h == base {
			t.Errorf("%s: the same hash", name)
		}
	}
	var none *Trust
	if none.Hash() != hash() || (&Trust{}).Hash() != hash() {
		t.Errorf("nil and empty trusts hash as %s and %s, want %s", none.Hash(), (&Trust{}).Hash(), hash())
	}
}

// TestReads: Verify needs the bytes of the envelopes it opens, and only those.
func TestReads(t *testing.T) {
	for mt, want := range map[string]bool{
		bundleV3: true, "application/vnd.dev.sigstore.bundle+json;version=0.1": true, mediaTypeDSSE: true,
		mediaTypeSimpleSigning: true, "application/spdx+json": false, "application/jose+json": false, "": false,
	} {
		if got := Reads(mt); got != want {
			t.Errorf("Reads(%q) = %t, want %t", mt, got, want)
		}
	}
}

// TestPAE is DSSE v1's example.
func TestPAE(t *testing.T) {
	if got := string(pae("http://example.com/HelloWorld", []byte("hello world"))); got != "DSSEv1 29 http://example.com/HelloWorld 11 hello world" {
		t.Errorf("pae = %q", got)
	}
}
