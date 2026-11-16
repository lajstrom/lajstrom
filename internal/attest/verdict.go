package attest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// VerdictPredicateType is the predicate type of a verdict (docs/spec/manifest-profile.md §6.4,
// ADR 0008).
const VerdictPredicateType = "https://lajstrom.dev/verdict/v1"

// The forms of a sigstore bundle v0.3 as cosign v3 writes an attestation.
const (
	BundleMediaType      = "application/vnd.dev.sigstore.bundle.v0.3+json"
	inTotoPayloadType    = "application/vnd.in-toto+json"
	statementType        = "https://in-toto.io/Statement/v0.1"
	annotationContent    = "dev.sigstore.bundle.content"
	annotationPredicate  = "dev.sigstore.bundle.predicateType"
	contentDSSE          = "dsse-envelope"
	verdictAllow         = "allow"
	verdictDeny          = "deny"
	verdictQuarantine    = "quarantine"
	maxVerdictReasonSize = 4096
)

// Verdict is the predicate of a verdict: {verdict, reason, source, scope, validUntil}.
type Verdict struct {
	Verdict    string     `json:"verdict"`
	Reason     string     `json:"reason"`
	Source     string     `json:"source"`
	Scope      string     `json:"scope,omitempty"`
	ValidUntil *time.Time `json:"validUntil,omitempty"`
}

// ErrNotVerdict is wrapped by ParseVerdict: the predicate makes no verdict.
var ErrNotVerdict = errors.New("attest: not a verdict")

// ParseVerdict reads a verdict's predicate (manifest-profile.md §6.4): a verdict of allow, deny
// or quarantine, a reason, a source, a scope that is a repo mask or empty, and a validUntil in
// RFC 3339 or absent. Other keys are passed by. Anything else wraps ErrNotVerdict. Whether the
// scope is a mask of rbac.md §3 is the caller's to check.
func ParseVerdict(predicate json.RawMessage) (Verdict, error) {
	var raw struct {
		Verdict    string `json:"verdict"`
		Reason     string `json:"reason"`
		Source     string `json:"source"`
		Scope      string `json:"scope"`
		ValidUntil string `json:"validUntil"`
	}
	if err := json.Unmarshal(predicate, &raw); err != nil {
		return Verdict{}, fmt.Errorf("%w: %w", ErrNotVerdict, err)
	}
	v := Verdict{Verdict: raw.Verdict, Reason: raw.Reason, Source: raw.Source, Scope: raw.Scope}
	switch {
	case v.Verdict != verdictAllow && v.Verdict != verdictDeny && v.Verdict != verdictQuarantine:
		return Verdict{}, fmt.Errorf("%w: verdict %q is none of allow, deny and quarantine", ErrNotVerdict, v.Verdict)
	case len(v.Reason) > maxVerdictReasonSize:
		return Verdict{}, fmt.Errorf("%w: a reason of more than %d bytes", ErrNotVerdict, maxVerdictReasonSize)
	case v.Scope != "" && strings.Count(v.Scope, "/") != 1:
		return Verdict{}, fmt.Errorf("%w: scope %q is not a repo mask", ErrNotVerdict, v.Scope)
	}
	if raw.ValidUntil != "" {
		t, err := time.Parse(time.RFC3339, raw.ValidUntil)
		if err != nil {
			return Verdict{}, fmt.Errorf("%w: validUntil %q: %w", ErrNotVerdict, raw.ValidUntil, err)
		}
		t = t.UTC()
		v.ValidUntil = &t
	}
	return v, nil
}

// SignFunc signs msg: an ECDSA signature in ASN.1 DER of its SHA-256 (keys.PolicyKey.Sign).
type SignFunc func(ctx context.Context, msg []byte) ([]byte, error)

// VerdictReferrer is a verdict made by BuildVerdict: the referrer manifest and its blobs.
type VerdictReferrer struct {
	// Manifest is the referrer's bytes, Desc its descriptor.
	Manifest []byte
	Desc     ocispec.Descriptor
	// Bundle is the layer's bytes, the sigstore bundle; BundleDesc its descriptor.
	Bundle     []byte
	BundleDesc ocispec.Descriptor
}

// BuildVerdict makes the verdict v about the manifest subject of the OCI repository name, in the
// form cosign attest --key --tlog-upload=false gives (manifest-profile.md §6.4): an in-toto
// statement signed in a DSSE envelope by sign, in a sigstore bundle v0.3 that names the public
// key publicDER (PKIX) by its hint, the base64 of its SHA-256, as the one layer of a referrer
// manifest with the empty config, created at created.
func BuildVerdict(ctx context.Context, name string, subject ocispec.Descriptor, v Verdict, publicDER []byte, sign SignFunc,
	created time.Time) (*VerdictReferrer, error) {
	pred, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	statement, err := json.Marshal(struct {
		Type    string `json:"_type"`
		Subject []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string          `json:"predicateType"`
		Predicate     json.RawMessage `json:"predicate"`
	}{Type: statementType, Subject: []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	}{{Name: name, Digest: map[string]string{subject.Digest.Algorithm().String(): subject.Digest.Encoded()}}},
		PredicateType: VerdictPredicateType, Predicate: pred})
	if err != nil {
		return nil, err
	}
	sig, err := sign(ctx, pae(inTotoPayloadType, statement))
	if err != nil {
		return nil, fmt.Errorf("attest: sign the verdict of %s: %w", subject.Digest, err)
	}
	hint := sha256.Sum256(publicDER)
	var b bundleOut
	b.MediaType = BundleMediaType
	b.VerificationMaterial.PublicKey.Hint = base64.StdEncoding.EncodeToString(hint[:])
	b.DSSEEnvelope = envelopeOut{Payload: base64.StdEncoding.EncodeToString(statement), PayloadType: inTotoPayloadType,
		Signatures: []signatureOut{{Sig: base64.StdEncoding.EncodeToString(sig)}}}
	bundleBytes, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	bd := ocispec.Descriptor{MediaType: BundleMediaType, Digest: digest.FromBytes(bundleBytes), Size: int64(len(bundleBytes))}
	bd.Annotations = map[string]string{ocispec.AnnotationTitle: "sha256-" + bd.Digest.Encoded() + ".sigstore.json"}
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: BundleMediaType, Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{bd},
		Subject: &ocispec.Descriptor{MediaType: subject.MediaType, Digest: subject.Digest, Size: subject.Size},
		Annotations: map[string]string{annotationContent: contentDSSE, annotationPredicate: VerdictPredicateType,
			ocispec.AnnotationCreated: created.UTC().Format(time.RFC3339)}}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &VerdictReferrer{Manifest: raw, Desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest,
		Digest: digest.FromBytes(raw), Size: int64(len(raw))}, Bundle: bundleBytes, BundleDesc: bd}, nil
}

// bundleOut is a sigstore bundle v0.3 of a key without a transparency log entry, as BuildVerdict
// writes it.
type bundleOut struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		PublicKey struct {
			Hint string `json:"hint"`
		} `json:"publicKey"`
	} `json:"verificationMaterial"`
	DSSEEnvelope envelopeOut `json:"dsseEnvelope"`
}

// envelopeOut is the DSSE envelope of a bundleOut.
type envelopeOut struct {
	Payload     string         `json:"payload"`
	PayloadType string         `json:"payloadType"`
	Signatures  []signatureOut `json:"signatures"`
}

// signatureOut is a signature of an envelopeOut.
type signatureOut struct {
	Sig string `json:"sig"`
}
