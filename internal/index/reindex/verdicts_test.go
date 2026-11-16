package reindex_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// verdictSigner is a key the env's pipeline trusts for verdicts.
type verdictSigner struct {
	priv *ecdsa.PrivateKey
	der  []byte
}

// verdicts makes e's pipeline trust a new key for verdicts and its worker read through the
// pipeline itself, and returns the key.
func (e *env) verdicts(t *testing.T) *verdictSigner {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	must(t, err)
	trust, err := attest.NewTrust([]attest.Entry{{Name: "lajstrom", PublicKey: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}),
		PredicateTypes: []string{attest.VerdictPredicateType}}})
	must(t, err)
	log := slog.New(slog.DiscardHandler)
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	e.pipe = ingest.New(ingest.Options{Providers: e.providers, Index: e.st, Plugins: plugins, Log: log, Trust: trust})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: e.providers, Inspector: e.pipe, Repairer: e.pipe, Log: log,
		Now: e.clock.now, Attacher: e.pipe})
	return &verdictSigner{priv: priv, der: der}
}

// pushVerdict pushes a deny verdict of subject at name straight to the registry and returns the
// referrer's digest.
func (e *env) pushVerdict(t *testing.T, k *verdictSigner, name string, subject ocispec.Descriptor, reason string) digest.Digest {
	t.Helper()
	ctx := t.Context()
	sign := func(_ context.Context, msg []byte) ([]byte, error) {
		sum := sha256.Sum256(msg)
		return ecdsa.SignASN1(rand.Reader, k.priv, sum[:])
	}
	ref, err := attest.BuildVerdict(ctx, name, subject, attest.Verdict{Verdict: "deny", Reason: reason, Source: "trivy"}, k.der, sign,
		time.Now())
	must(t, err)
	rr, err := e.fake.Repository(ctx, name)
	must(t, err)
	must(t, rr.Push(ctx, ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)))
	must(t, rr.Push(ctx, ref.BundleDesc, bytes.NewReader(ref.Bundle)))
	must(t, rr.Push(ctx, ref.Desc, bytes.NewReader(ref.Manifest)))
	return ref.Desc.Digest
}

// verdictRows returns the verdicts of repo by referrer.
func (e *env) verdictRows(t *testing.T, repo index.Repo) map[digest.Digest]index.Verdict {
	t.Helper()
	rows, err := e.st.ListVerdicts(t.Context(), index.VerdictFilter{RepoID: repo.ID})
	must(t, err)
	out := map[digest.Digest]index.Verdict{}
	for _, v := range rows {
		out[v.Referrer] = v
	}
	return out
}

// TestVerdictsRebuild is the last criterion of #101: reindex rebuilds the verdicts of a managed
// repo with its attachments and those of an oci repo from the referrers of its tagged manifests,
// after the derived rows were lost, while the rules stay; a referrer deleted past Lajstrom is
// swept.
func TestVerdictsRebuild(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	k := e.verdicts(t)
	pub := e.publish(t, "hello", "hello")
	rr, err := e.fake.Repository(ctx, pub.RepoPath)
	must(t, err)
	subject, err := rr.Resolve(ctx, pub.Digest.String())
	must(t, err)
	managed := e.pushVerdict(t, k, pub.RepoPath, subject, "on hello")

	oci := index.Repo{Format: "oci", Name: "images", Registry: "local", Prefix: "oci/images", Kind: "hosted"}
	must(t, e.st.CreateRepo(ctx, &oci))
	image := e.pushManifest(t, "oci/images/app", "v1", ocispec.Manifest{Versioned: specs2, MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{}}, ocispec.MediaTypeImageManifest)
	ociVerdict := e.pushVerdict(t, k, "oci/images/app", image, "on the image")
	must(t, e.st.CreatePolicyRule(ctx, &index.PolicyRule{RepoMask: "*/*", Name: "evil", Version: "*", Variant: "*", Effect: "deny",
		Reason: "r", CreatedBy: "user:u"}))

	e.run(t, reindex.Request{})
	e.run(t, reindex.Request{Repo: &oci})
	got := e.verdictRows(t, e.repo)
	if v, ok := got[managed]; !ok || v.Reason != "on hello" || v.AttachmentID == 0 || v.Subject != pub.Digest {
		t.Fatalf("the managed repo's verdicts: %+v", got)
	}
	gotOCI := e.verdictRows(t, oci)
	if v, ok := gotOCI[ociVerdict]; !ok || v.Reason != "on the image" || v.AttachmentID != 0 || v.Path != "app" {
		t.Fatalf("the oci repo's verdicts: %+v", gotOCI)
	}

	must(t, e.st.ResetDerived(ctx, 0))
	if n := len(e.verdictRows(t, oci)); n != 0 {
		t.Fatalf("ResetDerived left %d verdicts", n)
	}
	e.run(t, reindex.Request{Prune: true})
	e.run(t, reindex.Request{Repo: &oci, Prune: true})
	if _, ok := e.verdictRows(t, e.repo)[managed]; !ok {
		t.Error("the managed repo's verdict is not rebuilt")
	}
	if _, ok := e.verdictRows(t, oci)[ociVerdict]; !ok {
		t.Error("the oci repo's verdict is not rebuilt")
	}
	rules, err := e.st.ListPolicyRules(ctx)
	must(t, err)
	if len(rules) != 1 {
		t.Errorf("the rules after the reset: %d", len(rules))
	}

	// Deleted past Lajstrom: the next run sweeps it.
	ociRR, err := e.fake.Repository(ctx, "oci/images/app")
	must(t, err)
	must(t, ociRR.Delete(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: ociVerdict}))
	e.clock.later(time.Minute)
	e.run(t, reindex.Request{Repo: &oci})
	if n := len(e.verdictRows(t, oci)); n != 0 {
		t.Errorf("a verdict deleted past Lajstrom is kept: %d rows", n)
	}
}
