package profile

import (
	"cmp"
	"regexp"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The kinds of referrer and of attachment (docs/spec/manifest-profile.md §6.1) that ReferrerKind
// and Attachments tell apart: in the audit row of a referrer push, and in the attachments of a
// version (#89).
const (
	ReferrerSignature   = "signature"
	ReferrerSBOM        = "sbom"
	ReferrerScan        = "scan"
	ReferrerProvenance  = "provenance"
	ReferrerAttestation = "attestation"
	ReferrerOther       = "other"
)

// cosignTag is cosign's legacy tag of a signature, an attestation or an SBOM of the manifest
// sha256:<hex>; the manifest under it has no subject.
var cosignTag = regexp.MustCompile(`^sha256-([0-9a-f]{64})\.(sig|att|sbom)$`)

// CosignTag reports whether tag is cosign's legacy tag sha256-<hex>.sig, .att or .sbom, and
// returns the manifest it names and the kind of its suffix.
func CosignTag(tag string) (subject digest.Digest, kind string, ok bool) {
	m := cosignTag.FindStringSubmatch(tag)
	if m == nil {
		return "", "", false
	}
	kind = map[string]string{"sig": ReferrerSignature, "att": ReferrerAttestation, "sbom": ReferrerSBOM}[m[2]]
	return digest.NewDigestFromEncoded(digest.SHA256, m[1]), kind, true
}

// IsReferrer reports whether the manifest m, pushed under tag ("" for a push by digest), is a
// referrer rather than a Lajstrom artifact: a manifest with a subject whose artifact type (or
// config media type) is not Lajstrom's, or a manifest under cosign's legacy tag. A Lajstrom
// artifact with a subject is an artifact.
func IsReferrer(tag string, m ocispec.Manifest) bool {
	if _, _, ok := CosignTag(tag); ok {
		return true
	}
	return m.Subject != nil && !strings.HasPrefix(cmp.Or(m.ArtifactType, m.Config.MediaType), ArtifactTypePrefix)
}

// referrerTypes are the artifact and layer media types of the referrers ReferrerKind knows, by
// prefix, and their kind. A sigstore bundle, DSSE and in-toto are attestations unless their
// predicate type says more (predicateKind).
var referrerTypes = []struct{ prefix, kind string }{
	{"application/vnd.dev.cosign.simplesigning.", ReferrerSignature}, // the payload of cosign's signatures
	{"application/vnd.dev.cosign.artifact.sig.", ReferrerSignature},  // cosign v2, OCI 1.1 mode
	{"application/vnd.dev.cosign.artifact.sbom.", ReferrerSBOM},
	{"application/vnd.cncf.notary.signature", ReferrerSignature},
	{"application/spdx", ReferrerSBOM},
	{"text/spdx", ReferrerSBOM},
	{"application/vnd.cyclonedx", ReferrerSBOM},
	{"application/vnd.dev.sigstore.bundle.", ReferrerAttestation}, // cosign v3: a signature is a bundle too
	{"application/vnd.dsse.envelope.", ReferrerAttestation},
	{"application/vnd.in-toto", ReferrerAttestation},
}

// The annotations that carry the predicate type of an attestation: cosign v3's bundle annotation
// on the manifest, cosign's legacy annotation on the layer.
const (
	annotationBundlePredicate = "dev.sigstore.bundle.predicateType"
	annotationLayerPredicate  = "predicateType"
)

// predicateAnnotations are both, the manifest's first.
var predicateAnnotations = []string{annotationBundlePredicate, annotationLayerPredicate}

// ReferrerKind is the kind of the referrer m pushed under tag, from the manifest alone: what the
// predicate type of an attestation names (PredicateKind), else the kind of cosign's tag suffix,
// else that of the first of its artifact type, config and layer media types that it knows, else
// ReferrerOther.
func ReferrerKind(tag string, m ocispec.Manifest) string {
	descs := append([]ocispec.Descriptor{{MediaType: m.ArtifactType, Annotations: m.Annotations}, m.Config}, m.Layers...)
	for _, d := range descs {
		for _, key := range predicateAnnotations {
			if p := d.Annotations[key]; p != "" {
				return PredicateKind(p)
			}
		}
	}
	if _, kind, ok := CosignTag(tag); ok {
		return kind
	}
	for _, d := range descs {
		for _, rt := range referrerTypes {
			if strings.HasPrefix(d.MediaType, rt.prefix) {
				return rt.kind
			}
		}
	}
	return ReferrerOther
}
