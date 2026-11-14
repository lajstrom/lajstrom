package profile

import (
	"cmp"
	"encoding/json"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The annotations a producer may set on a referrer manifest, the identity of its result
// (docs/spec/manifest-profile.md §6.2). All are optional and unsigned.
const (
	AnnotationAttachmentTool        = "lajstrom.attachment.tool"
	AnnotationAttachmentToolVersion = "lajstrom.attachment.toolVersion"
	AnnotationAttachmentDatabase    = "lajstrom.attachment.database"
	AnnotationAttachmentInput       = "lajstrom.attachment.input"
)

// The predicate types that PredicateKind tells apart beyond their prefixes (manifest-profile.md
// §6.1).
const (
	PredicateCosignSign = "https://sigstore.dev/cosign/sign/v1"
	PredicateCosignVuln = "https://cosign.sigstore.dev/attestation/vuln/v1"
	PredicateSPDX       = "https://spdx.dev/Document"
	PredicateCycloneDX  = "https://cyclonedx.org/bom"
	predicateSLSA       = "https://slsa.dev/provenance/"
)

// Attachment is one attachment of a referrer manifest (manifest-profile.md §6.1): one of its
// layers, or the manifest itself when it has none.
type Attachment struct {
	// Layer is the attachment's layer, as the manifest lists it. For a manifest without layers
	// it has no digest, and its media type is the manifest's artifact type, else its config's.
	Layer ocispec.Descriptor
	// Kind is one of the Referrer* kinds, from the manifest alone (ReferrerKind's rules for the
	// layer).
	Kind string
	// PredicateType is the predicate type cosign's annotations name, "" when none.
	PredicateType string
}

// Attachments lists the attachments of the referrer m pushed under tag ("" for a push by digest),
// in the order of its layers: one per layer that is not the empty descriptor, or one for the
// manifest when it has no other. The kind of each is that of its predicate type (cosign's
// annotation on the layer, or on the manifest), else of cosign's legacy tag, else of the first of
// its layer's, the manifest's artifact and config media types that ReferrerKind knows, else
// ReferrerOther.
func Attachments(tag string, m ocispec.Manifest) []Attachment {
	artifactType := cmp.Or(m.ArtifactType, m.Config.MediaType)
	var out []Attachment
	for _, l := range m.Layers {
		if l.MediaType == ocispec.MediaTypeEmptyJSON {
			continue
		}
		p := cmp.Or(l.Annotations[annotationLayerPredicate], m.Annotations[annotationBundlePredicate])
		out = append(out, Attachment{Layer: l, PredicateType: p, Kind: attachmentKind(tag, p, l.MediaType, m.ArtifactType, m.Config.MediaType)})
	}
	if len(out) == 0 {
		p := m.Annotations[annotationBundlePredicate]
		out = append(out, Attachment{Layer: ocispec.Descriptor{MediaType: artifactType}, PredicateType: p,
			Kind: attachmentKind(tag, p, m.ArtifactType, m.Config.MediaType)})
	}
	return out
}

// attachmentKind is the kind of an attachment of predicate type p (cosign's annotation) pushed
// under tag whose media types are types, the first known first.
func attachmentKind(tag, p string, types ...string) string {
	if p != "" {
		return PredicateKind(p)
	}
	if _, kind, ok := CosignTag(tag); ok {
		return kind
	}
	for _, t := range types {
		if kind := mediaTypeKind(t); kind != "" {
			return kind
		}
	}
	return ReferrerOther
}

// mediaTypeKind is the kind of the media type t in referrerTypes, "" when it has none.
func mediaTypeKind(t string) string {
	for _, rt := range referrerTypes {
		if t != "" && strings.HasPrefix(t, rt.prefix) {
			return rt.kind
		}
	}
	return ""
}

// PredicateKind is the kind of an attestation of the predicate type p (manifest-profile.md §6.1):
// a signature for cosign's own, a scan for cosign's vulnerability report, an SBOM for SPDX and
// CycloneDX, provenance for SLSA's, an attestation for any other.
func PredicateKind(p string) string {
	switch {
	case p == PredicateCosignSign:
		return ReferrerSignature
	case p == PredicateCosignVuln:
		return ReferrerScan
	case isType(p, PredicateSPDX), isType(p, PredicateCycloneDX):
		return ReferrerSBOM
	case strings.HasPrefix(p, predicateSLSA):
		return ReferrerProvenance
	}
	return ReferrerAttestation
}

// isType reports whether the predicate type p is base or one of its versions, base/<version>.
func isType(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

// StatementKind is the kind of an attestation whose statement has the predicate type p and the
// predicate predicate: PredicateKind's, but a scan for a CycloneDX BOM that lists
// vulnerabilities (a combined document, #89).
func StatementKind(p string, predicate json.RawMessage) string {
	kind := PredicateKind(p)
	if kind != ReferrerSBOM || !isType(p, PredicateCycloneDX) || len(predicate) == 0 {
		return kind
	}
	var bom struct {
		Vulnerabilities []json.RawMessage `json:"vulnerabilities"`
	}
	if json.Unmarshal(predicate, &bom) == nil && len(bom.Vulnerabilities) > 0 {
		return ReferrerScan
	}
	return kind
}
