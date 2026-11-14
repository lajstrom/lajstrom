// Package attest verifies the attachments of a version (docs/spec/manifest-profile.md §6.3): it
// says whether a public key the operator trusts signed a sigstore bundle, a DSSE envelope or
// cosign's simple-signing payload, and whether what it signed names the version's manifest. It
// never signs and never fetches: the caller reads the layer and passes its bytes.
//
// The package is pure Go over the standard library (crypto/ecdsa, crypto/ed25519, crypto/rsa,
// crypto/x509): no sigstore library, which docs/spec/layout.md §3 does not list. Keyless
// signatures (a Fulcio certificate), notation and message signatures are labelled unsupported.
package attest

import (
	"bytes"
	"cmp"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512" // crypto.SHA384 and crypto.SHA512, for ECDSA over P-384 and P-521
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The labels of an attachment (manifest-profile.md §6.3); index.Attachment* has the same values.
const (
	Verified    = "verified"
	Untrusted   = "untrusted"
	Unsigned    = "unsigned"
	Invalid     = "invalid"
	Unsupported = "unsupported"
)

// PredicateCosignSign is the predicate type of cosign v3's signatures, and the one a signature
// counts as for the predicate types of a trust entry.
const PredicateCosignSign = "https://sigstore.dev/cosign/sign/v1"

// The media types and annotations Verify reads.
const (
	mediaTypeBundle        = "application/vnd.dev.sigstore.bundle" // v0.3+json, and +json;version=0.x
	mediaTypeDSSE          = "application/vnd.dsse.envelope.v1+json"
	mediaTypeSimpleSigning = "application/vnd.dev.cosign.simplesigning.v1+json"
	payloadTypeInToto      = "application/vnd.in-toto+json"
	annotationSignature    = "dev.cosignproject.cosign/signature"
	annotationCertificate  = "dev.sigstore.cosign/certificate"
	statementTypePrefix    = "https://in-toto.io/Statement/"
)

// notationTypes are the media types of notation's signature envelopes, which Verify does not
// check.
var notationTypes = []string{"application/jose+json", "application/cose"}

// Entry is one entry of attachments.trust.
type Entry struct {
	// Name is the signedBy of what its key verifies; unique in a trust.
	Name string
	// PublicKey is a PEM block PUBLIC KEY (PKIX): ECDSA P-256, P-384 or P-521, Ed25519, or RSA.
	PublicKey []byte
	// PredicateTypes are the predicate types the key is trusted for; empty: every one,
	// signatures included (they count as PredicateCosignSign).
	PredicateTypes []string
}

// key is a parsed entry.
type key struct {
	name  string
	pub   crypto.PublicKey
	types map[string]bool // nil: every predicate type
}

// Trust is the keys an operator trusts. The zero value and nil trust no key.
type Trust struct {
	keys []key
	hash string
}

// NewTrust parses the entries of a trust. An entry without a name, a name given twice, or a key
// that is not a PEM PUBLIC KEY of a supported algorithm is an error. No entry is a trust in which
// nothing verifies.
func NewTrust(entries []Entry) (*Trust, error) {
	t := &Trust{}
	type hashed struct {
		name  string
		der   []byte
		types []string
	}
	var parts []hashed
	seen := map[string]bool{}
	for i, e := range entries {
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("attest: trust entry %d has no name", i+1)
		case seen[e.Name]:
			return nil, fmt.Errorf("attest: the trust names %q twice", e.Name)
		}
		seen[e.Name] = true
		pub, der, err := parseKey(e.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("attest: the key of %q: %w", e.Name, err)
		}
		k := key{name: e.Name, pub: pub}
		types := slices.Clone(e.PredicateTypes)
		slices.Sort(types)
		types = slices.Compact(types)
		if len(types) > 0 {
			k.types = map[string]bool{}
			for _, p := range types {
				k.types[p] = true
			}
		}
		t.keys = append(t.keys, k)
		parts = append(parts, hashed{e.Name, der, types})
	}
	slices.SortFunc(parts, func(a, b hashed) int { return strings.Compare(a.name, b.name) })
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%s\x00%x\x00%s\n", strconv.Quote(p.name), p.der, strconv.Quote(strings.Join(p.types, "\x00")))
	}
	t.hash = hex.EncodeToString(h.Sum(nil))[:32]
	return t, nil
}

// parseKey returns the public key of a PEM PUBLIC KEY block and its PKIX bytes.
func parseKey(b []byte) (crypto.PublicKey, []byte, error) {
	block, rest := pem.Decode(b)
	switch {
	case block == nil:
		return nil, nil, errors.New("not PEM")
	case block.Type != "PUBLIC KEY":
		return nil, nil, fmt.Errorf("a PEM block %q, not PUBLIC KEY", block.Type)
	case len(bytes.TrimSpace(rest)) > 0:
		return nil, nil, errors.New("more than one PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if hashOf(k.Curve) == 0 {
			return nil, nil, fmt.Errorf("an ECDSA key on %s; P-256, P-384 and P-521 are supported", k.Curve.Params().Name)
		}
	case ed25519.PublicKey, *rsa.PublicKey:
	default:
		return nil, nil, fmt.Errorf("a key of type %T; ECDSA, Ed25519 and RSA are supported", pub)
	}
	return pub, block.Bytes, nil
}

// hashOf is the hash an ECDSA signature on curve c is made over, as cosign and sigstore make it.
func hashOf(c elliptic.Curve) crypto.Hash {
	switch c {
	case elliptic.P256():
		return crypto.SHA256
	case elliptic.P384():
		return crypto.SHA384
	case elliptic.P521():
		return crypto.SHA512
	}
	return 0
}

// Hash is a hex digest of the trust's entries (names, the keys' PKIX bytes and predicate types),
// whatever their order: the trust column of attachments, which reindex compares to label rows
// again. Nil and an empty trust have the hash of no entry.
func (t *Trust) Hash() string {
	if t == nil || t.hash == "" {
		return emptyHash
	}
	return t.hash
}

// emptyHash is the hash of a trust without entries.
var emptyHash = hex.EncodeToString(sha256.New().Sum(nil))[:32]

// Reads reports whether Verify needs the bytes of a layer of mediaType: a sigstore bundle, a DSSE
// envelope or a simple-signing payload. A layer of another type is judged without them.
func Reads(mediaType string) bool {
	return strings.HasPrefix(mediaType, mediaTypeBundle) || mediaType == mediaTypeDSSE || mediaType == mediaTypeSimpleSigning
}

// Input is one attachment to judge.
type Input struct {
	// Subject is the version's manifest, which what is signed must name.
	Subject digest.Digest
	// Layer is the attachment's layer, with its annotations: a legacy signature is there.
	Layer ocispec.Descriptor
	// Content is the layer's bytes, which the caller checked against its digest; nil when Reads
	// says Verify does not need them.
	Content []byte
	// Annotated is the predicate type cosign's annotations name, "" when none.
	Annotated string
}

// Result is the label of an attachment.
type Result struct {
	// Verification is Verified, Untrusted, Unsigned, Invalid or Unsupported.
	Verification string
	// SignedBy is the name of the entry whose key verified it, when it is Verified.
	SignedBy string
	// Reason says why it is not verified; "" when it is.
	Reason string
	// PredicateType is the statement's predicate type when there is a statement, else Annotated.
	PredicateType string
	// Predicate is the statement's predicate, nil when there is none.
	Predicate json.RawMessage
}

// Verify judges one attachment (manifest-profile.md §6.3). It returns no error: what does not
// parse is Invalid, what it cannot check is Unsupported.
func (t *Trust) Verify(in Input) Result {
	mt := in.Layer.MediaType
	switch {
	case strings.HasPrefix(mt, mediaTypeBundle):
		return t.bundle(in)
	case mt == mediaTypeDSSE:
		var env envelope
		if err := json.Unmarshal(in.Content, &env); err != nil {
			return Result{Verification: Invalid, Reason: "the DSSE envelope does not parse: " + err.Error(), PredicateType: in.Annotated}
		}
		return t.envelope(in, env, in.Layer.Annotations[annotationCertificate] != "")
	case mt == mediaTypeSimpleSigning:
		return t.simpleSigning(in)
	case slices.Contains(notationTypes, mt):
		return Result{Verification: Unsupported, Reason: "notation signatures are not verified", PredicateType: in.Annotated}
	}
	return Result{Verification: Unsigned, Reason: "it carries no signature", PredicateType: in.Annotated}
}

// bundle is what Verify reads of a sigstore bundle (protobuf JSON: bytes in standard base64).
type bundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		Certificate          json.RawMessage `json:"certificate"`
		X509CertificateChain json.RawMessage `json:"x509CertificateChain"`
	} `json:"verificationMaterial"`
	DSSEEnvelope     *envelope       `json:"dsseEnvelope"`
	MessageSignature json.RawMessage `json:"messageSignature"`
}

// envelope is a DSSE envelope.
type envelope struct {
	PayloadType string `json:"payloadType"`
	Payload     string `json:"payload"`
	Signatures  []struct {
		KeyID string `json:"keyid"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

// statement is what Verify reads of an in-toto statement.
type statement struct {
	Type    string `json:"_type"`
	Subject []struct {
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     json.RawMessage `json:"predicate"`
}

// names reports whether the statement's subject has the digest d.
func (s statement) names(d digest.Digest) bool {
	for _, sub := range s.Subject {
		if v, ok := sub.Digest[d.Algorithm().String()]; ok && strings.EqualFold(v, d.Encoded()) {
			return true
		}
	}
	return false
}

// bundle judges a sigstore bundle: its DSSE envelope; a bundle with a certificate is keyless.
func (t *Trust) bundle(in Input) Result {
	var b bundle
	if err := json.Unmarshal(in.Content, &b); err != nil {
		return Result{Verification: Invalid, Reason: "the sigstore bundle does not parse: " + err.Error(), PredicateType: in.Annotated}
	}
	keyless := len(b.VerificationMaterial.Certificate) > 0 && string(b.VerificationMaterial.Certificate) != "null" ||
		len(b.VerificationMaterial.X509CertificateChain) > 0 && string(b.VerificationMaterial.X509CertificateChain) != "null"
	switch {
	case b.DSSEEnvelope != nil:
		return t.envelope(in, *b.DSSEEnvelope, keyless)
	case len(b.MessageSignature) > 0:
		return Result{Verification: Unsupported, Reason: "a bundle with a message signature is not verified", PredicateType: in.Annotated}
	}
	return Result{Verification: Invalid, Reason: "the sigstore bundle has neither a DSSE envelope nor a message signature", PredicateType: in.Annotated}
}

// envelope judges a DSSE envelope whose payload must be an in-toto statement naming in.Subject;
// keyless says that it came with a certificate rather than a key.
func (t *Trust) envelope(in Input, env envelope, keyless bool) Result {
	r := Result{PredicateType: in.Annotated}
	if env.PayloadType != payloadTypeInToto {
		return r.with(Invalid, fmt.Sprintf("the payload type is %q, not %s", env.PayloadType, payloadTypeInToto))
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return r.with(Invalid, "the payload is not base64: "+err.Error())
	}
	var st statement
	if err := json.Unmarshal(payload, &st); err != nil || !strings.HasPrefix(st.Type, statementTypePrefix) {
		return r.with(Invalid, "the payload is not an in-toto statement")
	}
	r.PredicateType, r.Predicate = cmp.Or(st.PredicateType, in.Annotated), st.Predicate
	if len(env.Signatures) == 0 {
		return r.with(Unsigned, "the envelope has no signature")
	}
	sigs := make([][]byte, 0, len(env.Signatures))
	for _, s := range env.Signatures {
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			return r.with(Invalid, "a signature is not base64: "+err.Error())
		}
		sigs = append(sigs, sig)
	}
	r = t.judge(r, r.PredicateType, pae(env.PayloadType, payload), sigs, keyless)
	if r.Verification == Verified && !st.names(in.Subject) {
		return r.with(Invalid, fmt.Sprintf("%s signed a statement that does not name %s", r.SignedBy, in.Subject))
	}
	return r
}

// simpleSigning judges a layer of cosign's legacy signature: the signature in its annotation,
// over the layer's bytes, whose docker-manifest-digest must be in.Subject.
func (t *Trust) simpleSigning(in Input) Result {
	r := Result{PredicateType: in.Annotated}
	sig64 := in.Layer.Annotations[annotationSignature]
	if sig64 == "" {
		return r.with(Unsigned, "the layer has no "+annotationSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(sig64)
	if err != nil {
		return r.with(Invalid, "the signature is not base64: "+err.Error())
	}
	var p struct {
		Critical struct {
			Image struct {
				Digest string `json:"docker-manifest-digest"`
			} `json:"image"`
		} `json:"critical"`
	}
	if err := json.Unmarshal(in.Content, &p); err != nil {
		return r.with(Invalid, "the payload is not cosign's simple signing: "+err.Error())
	}
	r = t.judge(r, PredicateCosignSign, in.Content, [][]byte{sig}, in.Layer.Annotations[annotationCertificate] != "")
	if r.Verification == Verified && p.Critical.Image.Digest != in.Subject.String() {
		return r.with(Invalid, fmt.Sprintf("%s signed %q, not %s", r.SignedBy, p.Critical.Image.Digest, in.Subject))
	}
	return r
}

// judge labels r by the signatures sigs of msg: Verified by the first key trusted for
// predicateType that verifies one; Untrusted when only a key trusted for other predicate types
// does, or none and it is not keyless; Unsupported when none does and it is keyless.
func (t *Trust) judge(r Result, predicateType string, msg []byte, sigs [][]byte, keyless bool) Result {
	var other string // a key that verifies but is not trusted for predicateType
	if t != nil {
		for _, k := range t.keys {
			if !slices.ContainsFunc(sigs, func(sig []byte) bool { return verify(k.pub, msg, sig) }) {
				continue
			}
			if k.types == nil || k.types[predicateType] {
				r.Verification, r.SignedBy, r.Reason = Verified, k.name, ""
				return r
			}
			if other == "" {
				other = k.name
			}
		}
	}
	switch {
	case other != "":
		return r.with(Untrusted, fmt.Sprintf("the key of %s signed it, and is not trusted for the predicate type %q", other, predicateType))
	case keyless:
		return r.with(Unsupported, "keyless signatures (a Fulcio certificate) are not verified")
	}
	return r.with(Untrusted, "no trusted key verifies its signature")
}

// with returns r labelled v for reason.
func (r Result) with(v, reason string) Result {
	r.Verification, r.Reason = v, reason
	if v != Verified {
		r.SignedBy = ""
	}
	return r
}

// verify reports whether sig is pub's signature of msg: ECDSA (ASN.1) over the hash of its curve,
// Ed25519 over msg, RSA PKCS #1 v1.5 over SHA-256.
func verify(pub crypto.PublicKey, msg, sig []byte) bool {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		h := hashOf(k.Curve)
		if h == 0 {
			return false
		}
		sum := h.New()
		sum.Write(msg)
		return ecdsa.VerifyASN1(k, sum.Sum(nil), sig)
	case ed25519.PublicKey:
		return ed25519.Verify(k, msg, sig)
	case *rsa.PublicKey:
		s := sha256.Sum256(msg)
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, s[:], sig) == nil
	}
	return false
}

// pae is DSSE v1's pre-authentication encoding of a payload type and a payload, what a DSSE
// signature signs.
func pae(payloadType string, payload []byte) []byte {
	var b bytes.Buffer
	_, _ = fmt.Fprintf(&b, "DSSEv1 %d %s %d ", len(payloadType), payloadType, len(payload))
	b.Write(payload)
	return b.Bytes()
}
