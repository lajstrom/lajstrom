package attest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestBuildVerdict: a verdict that BuildVerdict makes is a referrer of its subject in cosign's
// form, verifies with its key for the verdict's predicate type and with no other, and its
// predicate reads back.
func TestBuildVerdict(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(_ context.Context, msg []byte) ([]byte, error) {
		sum := sha256.Sum256(msg)
		return ecdsa.SignASN1(rand.Reader, priv, sum[:])
	}
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("subject"), Size: 559}
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	v := Verdict{Verdict: "deny", Reason: "CVE-2026-1234", Source: "lajstrom:manual", Scope: "apt/internal", ValidUntil: &until}
	created := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	ref, err := BuildVerdict(t.Context(), "apt/internal/openssl", subject, v, der, sign, created)
	if err != nil {
		t.Fatal(err)
	}
	checkReferrer(t, ref, subject)
	checkVerified(t, ref, subject, der, v)
}

// checkReferrer checks the referrer manifest of ref: cosign's form, of subject.
func checkReferrer(t *testing.T, ref *VerdictReferrer, subject ocispec.Descriptor) {
	t.Helper()
	var m ocispec.Manifest
	if err := json.Unmarshal(ref.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Subject == nil || m.Subject.Digest != subject.Digest || m.ArtifactType != BundleMediaType || len(m.Layers) != 1 ||
		m.Layers[0].Digest != ref.BundleDesc.Digest || m.Annotations[annotationPredicate] != VerdictPredicateType ||
		m.Annotations[ocispec.AnnotationCreated] != "2026-11-15T10:00:00Z" || ref.Desc.Digest != digest.FromBytes(ref.Manifest) {
		t.Errorf("the referrer: %s", ref.Manifest)
	}
}

// checkVerified checks that ref verifies with der for verdicts, on subject alone, and reads back v.
func checkVerified(t *testing.T, ref *VerdictReferrer, subject ocispec.Descriptor, der []byte, v Verdict) {
	t.Helper()
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	trust, err := NewTrust([]Entry{{Name: "lajstrom", PublicKey: pemKey, PredicateTypes: []string{VerdictPredicateType}}})
	if err != nil {
		t.Fatal(err)
	}
	res := trust.Verify(Input{Subject: subject.Digest, Layer: ref.BundleDesc, Content: ref.Bundle})
	if res.Verification != Verified || res.SignedBy != "lajstrom" || res.PredicateType != VerdictPredicateType {
		t.Fatalf("Verify: %+v", res)
	}
	got, err := ParseVerdict(res.Predicate)
	if err != nil || got.Verdict != "deny" || got.Reason != v.Reason || got.Scope != v.Scope || got.ValidUntil == nil ||
		!got.ValidUntil.Equal(*v.ValidUntil) {
		t.Errorf("the predicate read back: %+v, %v", got, err)
	}
	if res := trust.Verify(Input{Subject: digest.FromString("another"), Layer: ref.BundleDesc, Content: ref.Bundle}); res.Verification == Verified {
		t.Error("the verdict verifies on another subject")
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherDER, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
	untrusted, _ := NewTrust([]Entry{{Name: "x", PublicKey: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: otherDER})}})
	if res := untrusted.Verify(Input{Subject: subject.Digest, Layer: ref.BundleDesc, Content: ref.Bundle}); res.Verification != Untrusted {
		t.Errorf("another key: %+v", res)
	}
}

// TestParseVerdict: the predicates that make no verdict.
func TestParseVerdict(t *testing.T) {
	for _, p := range []string{`[]`, `{"verdict":"block"}`, `{"verdict":"deny","scope":"x"}`, `{"verdict":"deny","validUntil":"soon"}`} {
		if _, err := ParseVerdict(json.RawMessage(p)); !errors.Is(err, ErrNotVerdict) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if v, err := ParseVerdict(json.RawMessage(`{"verdict":"quarantine","extra":1}`)); err != nil || v.Verdict != "quarantine" {
		t.Errorf("a quarantine with another key: %+v, %v", v, err)
	}
}
