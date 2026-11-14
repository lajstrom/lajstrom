package attest_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sigstore/sigstore-go/pkg/testing/data"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
)

const (
	cyclonedx = "https://cyclonedx.org/bom"
	slsa      = "https://slsa.dev/provenance/v1"
	github    = "https://token.actions.githubusercontent.com"
)

// bundleLayer is the layer of a cosign v3 bundle.
var bundleLayer = ocispec.Descriptor{MediaType: attesttest.BundleMediaType}

// TestVerifyKeyless is the keyless table of #89 (manifest-profile.md §6.3), on a Sigstore of the
// test: a trusted identity, an identity trusted for other predicate types, a subject that matches
// a regular expression, an identity nobody trusts, another issuer, another Sigstore, a tampered
// payload, a statement of another manifest, the SCTs the certificate lacks, a trust without
// keyless, and cosign's legacy layer.
func TestVerifyKeyless(t *testing.T) {
	sg, elsewhere := attesttest.NewSigstore(t), attesttest.NewSigstore(t)
	subject, other := digest.FromString("the version's manifest"), digest.FromString("another manifest")
	sbom := attesttest.Statement(subject, cyclonedx, `{"bomFormat":"CycloneDX"}`)
	provenance := attesttest.Statement(subject, slsa, `{"buildDefinition":{}}`)
	keyless := attest.Keyless{TrustedRoot: sg.TrustedRoot(t), Identities: []attest.Identity{
		{Name: "ci", Issuer: github, Subject: "ci@example.com"},
		{Name: "release", Issuer: github, SubjectRegex: `^release@(build\.)?example\.com$`, PredicateTypes: []string{slsa}},
	}}
	trust, err := attest.NewTrust(nil, attest.WithKeyless(keyless))
	if err != nil {
		t.Fatal(err)
	}
	strict := keyless
	strict.SignedCertificateTimestamps = 1
	withSCT, err := attest.NewTrust(nil, attest.WithKeyless(strict))
	if err != nil {
		t.Fatal(err)
	}
	keysOnly, err := attest.NewTrust(nil)
	if err != nil {
		t.Fatal(err)
	}
	tampered := tamper(t, sg.Bundle(t, github, "ci@example.com", sbom))
	legacy := ocispec.Descriptor{MediaType: attesttest.DSSEMediaType, Annotations: map[string]string{"dev.sigstore.cosign/certificate": "x"}}
	for _, tt := range []struct {
		name      string
		trust     *attest.Trust
		layer     ocispec.Descriptor
		content   []byte
		want      string
		signedBy  string
		predicate string
		reason    string
	}{
		{"a trusted identity", trust, bundleLayer, sg.Bundle(t, github, "ci@example.com", sbom), attest.Verified, "ci", cyclonedx, ""},
		{"a subject matching the regex, its type", trust, bundleLayer, sg.Bundle(t, github, "release@build.example.com", provenance), attest.Verified, "release", slsa, ""},
		{"the regex's identity, another type", trust, bundleLayer, sg.Bundle(t, github, "release@example.com", sbom), attest.Untrusted, "", cyclonedx,
			`the identity release signed it, and is not trusted for the predicate type "https://cyclonedx.org/bom"`},
		{"an identity nobody trusts", trust, bundleLayer, sg.Bundle(t, github, "stranger@example.com", sbom), attest.Untrusted, "", cyclonedx,
			`issuer "https://token.actions.githubusercontent.com" and subject "stranger@example.com", is not a trusted identity`},
		{"another issuer", trust, bundleLayer, sg.Bundle(t, "https://accounts.example.com", "ci@example.com", sbom), attest.Untrusted, "", cyclonedx,
			`issuer "https://accounts.example.com"`},
		{"another Sigstore", trust, bundleLayer, elsewhere.Bundle(t, github, "ci@example.com", sbom), attest.Untrusted, "", cyclonedx,
			"does not verify against the trusted root"},
		{"a tampered payload", trust, bundleLayer, tampered, attest.Untrusted, "", cyclonedx, "does not verify against the trusted root"},
		{"a statement of another manifest", trust, bundleLayer, sg.Bundle(t, github, "ci@example.com", attesttest.Statement(other, cyclonedx, `{}`)),
			attest.Invalid, "", cyclonedx, "does not name " + subject.String()},
		{"no SCT where one is wanted", withSCT, bundleLayer, sg.Bundle(t, github, "ci@example.com", sbom), attest.Untrusted, "", cyclonedx,
			"does not verify against the trusted root"},
		{"no keyless trust", keysOnly, bundleLayer, sg.Bundle(t, github, "ci@example.com", sbom), attest.Unsupported, "", cyclonedx,
			"attachments.keyless is not configured"},
		{"cosign's legacy keyless layer", trust, legacy, attesttest.Envelope(t, sbom, attesttest.NewKey(t)), attest.Unsupported, "", cyclonedx, "legacy keyless"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.trust.Verify(attest.Input{Subject: subject, Layer: tt.layer, Content: tt.content})
			if got.Verification != tt.want || got.SignedBy != tt.signedBy || got.PredicateType != tt.predicate {
				t.Errorf("Verify = %s by %q of %q (%s), want %s by %q of %q", got.Verification, got.SignedBy, got.PredicateType, got.Reason,
					tt.want, tt.signedBy, tt.predicate)
			}
			if (got.Reason == "") != (tt.want == attest.Verified) || !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("Reason = %q, want one with %q", got.Reason, tt.reason)
			}
		})
	}
}

// tamper replaces the statement in the DSSE envelope of the bundle b with another, keeping the
// signature.
func tamper(t *testing.T, b []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	env := m["dsseEnvelope"].(map[string]any)
	env["payload"] = base64.StdEncoding.EncodeToString(attesttest.Statement(digest.FromString("the version's manifest"), cyclonedx, `{"forged":true}`))
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestVerifyKeylessPublicGood verifies a real keyless bundle of the public-good Sigstore offline:
// the npm provenance of sigstore.js 2.0.0, signed in GitHub Actions, from sigstore-go's own test
// data (pkg/testing/data, Apache-2.0), against the public-good trusted root of the same data,
// with its SCT. Its subject is the tarball's sha512, which stands for the version here.
func TestVerifyKeylessPublicGood(t *testing.T) {
	b, err := data.Bundle(t, "sigstore.js@2.0.0-provenance.sigstore.json").MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	tr, err := data.TrustedRoot(t, "public-good.json").MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	subject, predicateType := statementOf(t, b)
	for _, tt := range []struct {
		name     string
		identity attest.Identity
		want     string
	}{
		{"the release workflow", attest.Identity{Name: "sigstore-js", Issuer: github,
			SubjectRegex: `^https://github\.com/sigstore/sigstore-js/\.github/workflows/release\.yml@refs/heads/main$`}, attest.Verified},
		{"another repository", attest.Identity{Name: "acme", Issuer: github, SubjectRegex: `^https://github\.com/acme/`}, attest.Untrusted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			trust, err := attest.NewTrust(nil, attest.WithKeyless(attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{tt.identity},
				SignedCertificateTimestamps: 1}))
			if err != nil {
				t.Fatal(err)
			}
			got := trust.Verify(attest.Input{Subject: subject, Layer: ocispec.Descriptor{MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.1"},
				Content: b})
			if got.Verification != tt.want || got.PredicateType != predicateType {
				t.Errorf("Verify = %s of %q (%s), want %s of %q", got.Verification, got.PredicateType, got.Reason, tt.want, predicateType)
			}
		})
	}
}

// statementOf returns the first subject of the bundle's statement, as a digest, and its
// predicate type.
func statementOf(t *testing.T, b []byte) (digest.Digest, string) {
	t.Helper()
	var bundle struct {
		DSSEEnvelope struct {
			Payload string `json:"payload"`
		} `json:"dsseEnvelope"`
	}
	if err := json.Unmarshal(b, &bundle); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Subject []struct {
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string `json:"predicateType"`
	}
	if err := json.Unmarshal(payload, &st); err != nil || len(st.Subject) == 0 {
		t.Fatalf("the statement of the bundle: %v", err)
	}
	for alg, hex := range st.Subject[0].Digest {
		return digest.NewDigestFromEncoded(digest.Algorithm(alg), hex), st.PredicateType
	}
	t.Fatal("the statement's subject has no digest")
	return "", ""
}

// TestNewTrustKeyless: what WithKeyless refuses, and that the hash moves with every part of the
// keyless trust.
func TestNewTrustKeyless(t *testing.T) {
	sg := attesttest.NewSigstore(t)
	key := attesttest.NewKey(t)
	tr := sg.TrustedRoot(t)
	ci := attest.Identity{Name: "ci", Issuer: github, Subject: "ci@example.com"}
	for _, tt := range []struct {
		name string
		k    attest.Keyless
		want string
	}{
		{"not a trusted root", attest.Keyless{TrustedRoot: []byte("{}")}, "the trusted root"},
		{"no name", attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{{Issuer: github, Subject: "a"}}}, "has no name"},
		{"a key's name", attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{{Name: "scanner", Issuer: github, Subject: "a"}}}, "twice"},
		{"no issuer", attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{{Name: "x", Subject: "a"}}}, "no issuer"},
		{"no subject", attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{{Name: "x", Issuer: github}}}, "one of a subject"},
		{"both subjects", attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{{Name: "x", Issuer: github, Subject: "a", SubjectRegex: "a"}}},
			"one of a subject"},
		{"a regex that does not compile", attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{{Name: "x", Issuer: github, SubjectRegex: "("}}},
			`"x"`},
		{"negative SCTs", attest.Keyless{TrustedRoot: tr, SignedCertificateTimestamps: -1}, "negative"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := attest.NewTrust([]attest.Entry{key.Entry(t, "scanner")}, attest.WithKeyless(tt.k))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("NewTrust = %v, want an error with %q", err, tt.want)
			}
		})
	}

	hash := func(k *attest.Keyless) string {
		t.Helper()
		var opts []attest.Option
		if k != nil {
			opts = append(opts, attest.WithKeyless(*k))
		}
		trust, err := attest.NewTrust([]attest.Entry{key.Entry(t, "scanner")}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return trust.Hash()
	}
	base := attest.Keyless{TrustedRoot: tr, Identities: []attest.Identity{ci, {Name: "rel", Issuer: github, SubjectRegex: "^r", PredicateTypes: []string{slsa}}}}
	reordered := base
	reordered.Identities = []attest.Identity{base.Identities[1], ci}
	if hash(&base) != hash(&reordered) {
		t.Error("the hash depends on the order of the identities")
	}
	changed := map[string]attest.Keyless{}
	k := base
	k.Identities = []attest.Identity{ci}
	changed["an identity less"] = k
	k = base
	k.Identities = []attest.Identity{{Name: "ci", Issuer: github, Subject: "other@example.com"}, base.Identities[1]}
	changed["another subject"] = k
	k = base
	k.Identities = []attest.Identity{ci, {Name: "rel", Issuer: github, SubjectRegex: "^r"}}
	changed["a type less"] = k
	k = base
	k.TrustedRoot = attesttest.NewSigstore(t).TrustedRoot(t)
	changed["another trusted root"] = k
	k = base
	k.SignedCertificateTimestamps = 1
	changed["an SCT"] = k
	for name, k := range changed {
		if hash(&k) == hash(&base) {
			t.Errorf("%s: the same hash", name)
		}
	}
	if hash(nil) == hash(&attest.Keyless{TrustedRoot: tr}) {
		t.Error("a trust with a trusted root hashes as one without")
	}
}
