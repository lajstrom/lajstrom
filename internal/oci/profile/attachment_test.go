package profile

import (
	"encoding/json"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestAttachments is the classification table of #89 (manifest-profile.md §6.1): what each
// referrer a producer pushes holds, one attachment per layer, and the kind of each.
func TestAttachments(t *testing.T) {
	subject := digest.FromString("the subject")
	cosign := "sha256-" + subject.Encoded()
	const (
		bundle   = "application/vnd.dev.sigstore.bundle.v0.3+json"
		dsse     = "application/vnd.dsse.envelope.v1+json"
		simple   = "application/vnd.dev.cosign.simplesigning.v1+json"
		openvex  = "https://openvex.dev/ns/v0.2.0"
		slsa     = "https://slsa.dev/provenance/v1"
		spdx23   = "https://spdx.dev/Document/v2.3"
		cdx16    = "https://cyclonedx.org/bom/v1.6"
		octets   = "application/octet-stream"
		empty    = ocispec.MediaTypeEmptyJSON
		imageCfg = ocispec.MediaTypeImageConfig
	)
	layer := func(mediaType string, annotations ...string) ocispec.Descriptor {
		d := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromString(mediaType + annotationsKey(annotations)), Size: 10}
		if len(annotations) > 0 {
			d.Annotations = map[string]string{}
			for i := 0; i+1 < len(annotations); i += 2 {
				d.Annotations[annotations[i]] = annotations[i+1]
			}
		}
		return d
	}
	v3 := func(predicateType string) ocispec.Manifest {
		return ocispec.Manifest{ArtifactType: bundle, Config: ocispec.Descriptor{MediaType: empty},
			Layers:      []ocispec.Descriptor{layer(bundle)},
			Subject:     &ocispec.Descriptor{Digest: subject},
			Annotations: map[string]string{"dev.sigstore.bundle.predicateType": predicateType, "dev.sigstore.bundle.content": "dsse-envelope"}}
	}
	legacy := func(layers ...ocispec.Descriptor) ocispec.Manifest {
		return ocispec.Manifest{Config: ocispec.Descriptor{MediaType: imageCfg}, Layers: layers}
	}
	attached := func(artifactType string, layers ...ocispec.Descriptor) ocispec.Manifest {
		return ocispec.Manifest{ArtifactType: artifactType, Config: ocispec.Descriptor{MediaType: empty}, Layers: layers,
			Subject: &ocispec.Descriptor{Digest: subject}}
	}
	type want struct{ kind, predicateType string }
	for _, tt := range []struct {
		name string
		tag  string
		m    ocispec.Manifest
		want []want
	}{
		{"cosign v3 signature", "", v3(PredicateCosignSign), []want{{ReferrerSignature, PredicateCosignSign}}},
		{"cosign v3 CycloneDX", "", v3(PredicateCycloneDX), []want{{ReferrerSBOM, PredicateCycloneDX}}},
		{"cosign v3 CycloneDX 1.6", "", v3(cdx16), []want{{ReferrerSBOM, cdx16}}},
		{"cosign v3 SPDX", "", v3(spdx23), []want{{ReferrerSBOM, spdx23}}},
		{"cosign v3 vulnerability report", "", v3(PredicateCosignVuln), []want{{ReferrerScan, PredicateCosignVuln}}},
		{"cosign v3 SLSA provenance", "", v3(slsa), []want{{ReferrerProvenance, slsa}}},
		{"cosign v3 OpenVEX", "", v3(openvex), []want{{ReferrerAttestation, openvex}}},
		{"cosign v3 verdict of #101", "", v3("dev.lajstrom.verdict/v1"), []want{{ReferrerAttestation, "dev.lajstrom.verdict/v1"}}},
		{"cosign v3 bundle without a predicate type", "", v3(""), []want{{ReferrerAttestation, ""}}},
		{"cosign legacy .att, three attestations", cosign + ".att", legacy(
			layer(dsse, "predicateType", PredicateCycloneDX), layer(dsse, "predicateType", PredicateCosignVuln), layer(dsse, "predicateType", slsa)),
			[]want{{ReferrerSBOM, PredicateCycloneDX}, {ReferrerScan, PredicateCosignVuln}, {ReferrerProvenance, slsa}}},
		{"cosign legacy .att without predicate types", cosign + ".att", legacy(layer(dsse)), []want{{ReferrerAttestation, ""}}},
		{"cosign legacy .sig, two signatures", cosign + ".sig", legacy(layer(simple, "a", "1"), layer(simple, "a", "2")),
			[]want{{ReferrerSignature, ""}, {ReferrerSignature, ""}}},
		{"cosign legacy .sbom", cosign + ".sbom", legacy(layer("text/spdx")), []want{{ReferrerSBOM, ""}}},
		{"cosign v2 OCI 1.1 signature", "", attached("application/vnd.dev.cosign.artifact.sig.v1+json", layer(simple)), []want{{ReferrerSignature, ""}}},
		{"notation", "", attached("application/vnd.cncf.notary.signature", layer("application/jose+json")), []want{{ReferrerSignature, ""}}},
		{"oras attach SPDX", "", attached("application/spdx+json", layer("application/spdx+json")), []want{{ReferrerSBOM, ""}}},
		{"oras attach CycloneDX", "", attached("application/vnd.cyclonedx+json", layer("application/vnd.cyclonedx+json")), []want{{ReferrerSBOM, ""}}},
		{"an SBOM layer under an unknown type", "", attached("application/vnd.example.reports.v1", layer("application/spdx+json"), layer(octets)),
			[]want{{ReferrerSBOM, ""}, {ReferrerOther, ""}}},
		{"an unknown layer under an SBOM type", "", attached("application/spdx+json", layer(octets)), []want{{ReferrerSBOM, ""}}},
		{"in-toto", "", attached("application/vnd.in-toto+json", layer("application/vnd.in-toto+json")), []want{{ReferrerAttestation, ""}}},
		{"unknown with a subject", "", attached("application/vnd.example.scan.v1", layer(octets)), []want{{ReferrerOther, ""}}},
		{"no layers", "", attached("application/vnd.example.note.v1"), []want{{ReferrerOther, ""}}},
		{"only the empty layer", "", attached("application/vnd.cncf.notary.signature", layer(empty)), []want{{ReferrerSignature, ""}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Attachments(tt.tag, tt.m)
			if len(got) != len(tt.want) {
				t.Fatalf("Attachments = %d, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, a := range got {
				if a.Kind != tt.want[i].kind || a.PredicateType != tt.want[i].predicateType {
					t.Errorf("attachment %d: %s of %q, want %s of %q", i, a.Kind, a.PredicateType, tt.want[i].kind, tt.want[i].predicateType)
				}
			}
			checkAttachedLayers(t, tt.m, got)
			// The audit row's kind, from the manifest alone, is that of the first attachment.
			if kind := ReferrerKind(tt.tag, tt.m); kind != got[0].Kind {
				t.Errorf("ReferrerKind = %s, the first attachment's %s", kind, got[0].Kind)
			}
		})
	}
}

// checkAttachedLayers checks that the attachments got of m are its layers but the empty descriptor, in
// order, or the manifest's artifact type without a digest when it has none.
func checkAttachedLayers(t *testing.T, m ocispec.Manifest, got []Attachment) {
	t.Helper()
	var layers []ocispec.Descriptor
	for _, l := range m.Layers {
		if l.MediaType != ocispec.MediaTypeEmptyJSON {
			layers = append(layers, l)
		}
	}
	for i, a := range got {
		switch {
		case len(layers) == 0 && (a.Layer.Digest != "" || a.Layer.MediaType != m.ArtifactType):
			t.Errorf("the attachment of a manifest without layers: %+v", a.Layer)
		case len(layers) > 0 && a.Layer.Digest != layers[i].Digest:
			t.Errorf("attachment %d is the layer %s, want %s", i, a.Layer.Digest, layers[i].Digest)
		}
	}
}

// annotationsKey makes the layers of a table row distinct.
func annotationsKey(a []string) string {
	b, _ := json.Marshal(a)
	return string(b)
}

// TestStatementKind: a CycloneDX statement with vulnerabilities is a scan, any other is what its
// predicate type says.
func TestStatementKind(t *testing.T) {
	for _, tt := range []struct {
		name, predicateType, predicate, want string
	}{
		{"CycloneDX with vulnerabilities", PredicateCycloneDX, `{"bomFormat":"CycloneDX","vulnerabilities":[{"id":"CVE-2026-1"}]}`, ReferrerScan},
		{"CycloneDX 1.6 with vulnerabilities", PredicateCycloneDX + "/v1.6", `{"vulnerabilities":[{}]}`, ReferrerScan},
		{"CycloneDX without vulnerabilities", PredicateCycloneDX, `{"bomFormat":"CycloneDX","vulnerabilities":[]}`, ReferrerSBOM},
		{"CycloneDX, no predicate", PredicateCycloneDX, ``, ReferrerSBOM},
		{"CycloneDX, not an object", PredicateCycloneDX, `"x"`, ReferrerSBOM},
		{"SPDX with a vulnerabilities key", PredicateSPDX, `{"vulnerabilities":[{}]}`, ReferrerSBOM},
		{"cosign vuln", PredicateCosignVuln, `{}`, ReferrerScan},
		{"a lookalike type", "https://cyclonedx.org/bomb", `{"vulnerabilities":[{}]}`, ReferrerAttestation},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatementKind(tt.predicateType, json.RawMessage(tt.predicate)); got != tt.want {
				t.Errorf("StatementKind = %s, want %s", got, tt.want)
			}
		})
	}
}
