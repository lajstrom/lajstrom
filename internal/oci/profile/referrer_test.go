package profile

import (
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestReferrers is the detection table of #28: what the manifest hook takes for a referrer,
// which passes through ingest unindexed, and its kind; and what it takes for an artifact.
func TestReferrers(t *testing.T) {
	subject := digest.FromString("the subject")
	cosign := "sha256-" + subject.Encoded()
	m := func(artifactType, configType, layerType string, withSubject bool, annotations ...string) ocispec.Manifest {
		m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
			ArtifactType: artifactType, Config: ocispec.Descriptor{MediaType: configType},
			Layers: []ocispec.Descriptor{{MediaType: layerType}}, Annotations: map[string]string{}}
		if withSubject {
			m.Subject = &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject}
		}
		for i := 0; i+1 < len(annotations); i += 2 {
			m.Annotations[annotations[i]] = annotations[i+1]
		}
		return m
	}
	const (
		empty       = ocispec.MediaTypeEmptyJSON
		imageConfig = ocispec.MediaTypeImageConfig
		bundle      = "application/vnd.dev.sigstore.bundle.v0.3+json"
		predicate   = "dev.sigstore.bundle.predicateType"
		octets      = "application/octet-stream"
	)
	for _, tt := range []struct {
		name string
		tag  string
		m    ocispec.Manifest
		kind string // "" when it is not a referrer
	}{
		{"cosign legacy signature", cosign + ".sig", m("", imageConfig, "application/vnd.dev.cosign.simplesigning.v1+json", false), ReferrerSignature},
		{"cosign legacy attestation", cosign + ".att", m("", imageConfig, "application/vnd.dsse.envelope.v1+json", false), ReferrerAttestation},
		{"cosign legacy SBOM", cosign + ".sbom", m("", imageConfig, "text/spdx", false), ReferrerSBOM},
		{"cosign v3 signature, a sigstore bundle", "", m(bundle, empty, bundle, true, predicate, "https://sigstore.dev/cosign/sign/v1"), ReferrerSignature},
		{"cosign v3 SPDX attestation", "", m(bundle, empty, bundle, true, predicate, "https://spdx.dev/Document/v2.3"), ReferrerSBOM},
		{"cosign v3 CycloneDX attestation", "", m(bundle, empty, bundle, true, predicate, "https://cyclonedx.org/bom"), ReferrerSBOM},
		{"cosign v3 SLSA provenance", "", m(bundle, empty, bundle, true, predicate, "https://slsa.dev/provenance/v1"), ReferrerProvenance},
		{"cosign v3 vulnerability report", "", m(bundle, empty, bundle, true, predicate, "https://cosign.sigstore.dev/attestation/vuln/v1"), ReferrerScan},
		{"cosign v2 OCI 1.1 signature", "", m("application/vnd.dev.cosign.artifact.sig.v1+json", empty, "application/vnd.dev.cosign.simplesigning.v1+json", true), ReferrerSignature},
		{"notation", "", m("application/vnd.cncf.notary.signature", empty, "application/jose+json", true), ReferrerSignature},
		{"SPDX", "", m("application/spdx+json", empty, "application/spdx+json", true), ReferrerSBOM},
		{"CycloneDX", "", m("application/vnd.cyclonedx+json", empty, "application/vnd.cyclonedx+json", true), ReferrerSBOM},
		{"in-toto", "", m("application/vnd.in-toto+json", empty, "application/vnd.in-toto+json", true), ReferrerAttestation},
		{"DSSE", "", m("application/vnd.dsse.envelope.v1+json", empty, "application/vnd.dsse.envelope.v1+json", true), ReferrerAttestation},
		{"unknown with a subject", "", m("application/vnd.example.scan.v1", empty, octets, true), ReferrerOther},
		{"oras's default type with a subject", "", m("application/vnd.unknown.artifact.v1", empty, octets, true), ReferrerOther},
		{"no artifactType, a config type", "", m("", "application/vnd.example.config.v1+json", octets, true), ReferrerOther},
		{"a Lajstrom artifact with a subject", "", m(ArtifactType("raw"), empty, MediaTypeRawFile, true), ""},
		{"no subject", "", m(bundle, empty, bundle, false), ""},
		{"no subject, the tag schema's tag", cosign, m(bundle, empty, bundle, false), ""},
		{"no subject, a tag like cosign's", "sha256-abc.sig", m("", imageConfig, "application/vnd.dev.cosign.simplesigning.v1+json", false), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReferrer(tt.tag, tt.m); got != (tt.kind != "") {
				t.Fatalf("IsReferrer = %t, want %t", got, tt.kind != "")
			}
			if tt.kind == "" {
				return
			}
			if got := ReferrerKind(tt.tag, tt.m); got != tt.kind {
				t.Errorf("ReferrerKind = %s, want %s", got, tt.kind)
			}
			if d, _, ok := CosignTag(tt.tag); ok && d != subject {
				t.Errorf("CosignTag names %s, want %s", d, subject)
			}
		})
	}
}
