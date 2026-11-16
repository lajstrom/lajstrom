package ingest_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/policy"
)

// verdictKey is the key that signs the verdicts of a test, which its pipeline trusts for them.
type verdictKey struct {
	priv *ecdsa.PrivateKey
	der  []byte
}

func newVerdictKey(t *testing.T) *verdictKey {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	must(t, err)
	return &verdictKey{priv: priv, der: der}
}

func (k *verdictKey) sign(_ context.Context, msg []byte) ([]byte, error) {
	sum := sha256.Sum256(msg)
	return ecdsa.SignASN1(rand.Reader, k.priv, sum[:])
}

func (k *verdictKey) entry() attest.Entry {
	return attest.Entry{Name: "lajstrom", PublicKey: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.der}),
		PredicateTypes: []string{attest.VerdictPredicateType}}
}

// blob is a BlobSource of b.
func blob(d ocispec.Descriptor, b []byte) format.BlobSource {
	return format.BlobSource{Descriptor: d, Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

// attachVerdict builds the verdict v of subject at path of the env's repo and attaches it.
func (e *env) attachVerdict(t *testing.T, k *verdictKey, path string, subject ocispec.Descriptor, v attest.Verdict) ingest.Referrer {
	t.Helper()
	ref, err := attest.BuildVerdict(t.Context(), e.repo.Format+"/"+e.repo.Name+"/"+path, subject, v, k.der, k.sign, now)
	must(t, err)
	r := ingest.Referrer{Path: path, Manifest: ref.Manifest, Desc: ref.Desc, Subject: subject.Digest}
	_, err = e.pipe.Attach(t.Context(), e.repo, r, []format.BlobSource{blob(ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data),
		blob(ref.BundleDesc, ref.Bundle)}, alice)
	must(t, err)
	return r
}

// auditActions returns the actions of the env's audit rows that begin with prefix, oldest first.
func (e *env) auditActions(t *testing.T, prefix string) []string {
	t.Helper()
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{})
	must(t, err)
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		if strings.HasPrefix(evs[i].Action, prefix) {
			out = append(out, evs[i].Action+" "+evs[i].Subject)
		}
	}
	return out
}

// TestVerdictsManaged: a verdict attached to a version (Attach, as the deny of the API does) is a
// verified attachment and a verdicts row, with its audit row policy.verdict.create; the delete of
// its referrer takes the row, with policy.verdict.delete; a verdict signed by a key not trusted
// for the predicate type is an attachment and no verdict.
func TestVerdictsManaged(t *testing.T) {
	e := newEnv(t)
	k := newVerdictKey(t)
	trust, err := attest.NewTrust([]attest.Entry{k.entry()})
	must(t, err)
	opts := e.options(e.prov)
	opts.Trust = trust
	e.pipe = ingest.New(opts)
	ctx := t.Context()
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "the subject"}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: res.Digest, Size: int64(len(raw))}

	r := e.attachVerdict(t, k, pkgPath, subject, attest.Verdict{Verdict: "deny", Reason: "CVE-2026-1234", Source: policy.SourceManual,
		Scope: "raw/scratch"})
	vs, err := e.st.ListVerdicts(ctx, index.VerdictFilter{RepoID: e.repo.ID})
	must(t, err)
	if len(vs) != 1 || vs[0].Verdict != "deny" || vs[0].Subject != res.Digest || vs[0].Referrer != r.Desc.Digest ||
		vs[0].SignedBy != "lajstrom" || vs[0].Scope != "raw/scratch" || vs[0].AttachmentID == 0 {
		t.Fatalf("the verdicts: %+v", vs)
	}
	if got := e.auditActions(t, "policy."); len(got) != 1 || got[0] != "policy.verdict.create notes 1.0" {
		t.Errorf("the audit rows: %v", got)
	}

	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: pkgPath, Reference: r.Desc.Digest.String()}, alice)
	must(t, err)
	vs, err = e.st.ListVerdicts(ctx, index.VerdictFilter{RepoID: e.repo.ID})
	must(t, err)
	if len(vs) != 0 {
		t.Errorf("the verdicts after the delete of the referrer: %+v", vs)
	}
	if got := e.auditActions(t, "policy."); len(got) != 2 || got[1] != "policy.verdict.delete notes 1.0" {
		t.Errorf("the audit rows after the delete: %v", got)
	}

	// Another key, not trusted for verdicts: an attachment, untrusted, and no verdict.
	stranger := newVerdictKey(t)
	e.attachVerdict(t, stranger, pkgPath, subject, attest.Verdict{Verdict: "deny", Reason: "spoof", Source: "x"})
	vs, err = e.st.ListVerdicts(ctx, index.VerdictFilter{RepoID: e.repo.ID})
	must(t, err)
	if len(vs) != 0 {
		t.Errorf("an untrusted verdict is a verdict: %+v", vs)
	}
}

// TestVerdictsOCI: a verdict of a manifest of an oci repo is a verdicts row without an
// attachment, and the delete of its referrer takes it.
func TestVerdictsOCI(t *testing.T) {
	e := newEnv(t)
	k := newVerdictKey(t)
	trust, err := attest.NewTrust([]attest.Entry{k.entry()})
	must(t, err)
	ctx := t.Context()
	e.repo = index.Repo{Format: "oci", Name: "images", Registry: "local", Prefix: "oci/images", Kind: "hosted"}
	must(t, e.st.CreateRepo(ctx, &e.repo))
	opts := e.options(e.prov)
	opts.Trust = trust
	e.pipe = ingest.New(opts)
	rr, err := e.prov.Repository(ctx, "oci/images/app")
	must(t, err)
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	must(t, rr.Push(ctx, ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)))
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(body), Size: int64(len(body))}
	must(t, rr.PushReference(ctx, subject, bytes.NewReader(body), "v1"))

	r := e.attachVerdict(t, k, "app", subject, attest.Verdict{Verdict: "deny", Reason: "malware", Source: "trivy"})
	vs, err := e.st.ListVerdicts(ctx, index.VerdictFilter{RepoID: e.repo.ID})
	must(t, err)
	if len(vs) != 1 || vs[0].AttachmentID != 0 || vs[0].Path != "app" || vs[0].Subject != subject.Digest {
		t.Fatalf("the verdicts of the oci repo: %+v", vs)
	}
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "app", Reference: r.Desc.Digest.String()}, alice)
	must(t, err)
	vs, err = e.st.ListVerdicts(ctx, index.VerdictFilter{RepoID: e.repo.ID})
	must(t, err)
	if len(vs) != 0 {
		t.Errorf("the verdicts after the delete: %+v", vs)
	}
	if got := e.auditActions(t, "policy."); len(got) != 2 || got[0] != "policy.verdict.create app" {
		t.Errorf("the audit rows: %v", got)
	}
}

// policyKey is a verdictKey as an ingest.VerdictSigner.
type policyKey struct{ *verdictKey }

func (k policyKey) Sign(ctx context.Context, msg []byte) ([]byte, error) { return k.sign(ctx, msg) }
func (k policyKey) PublicDER() []byte                                    { return k.der }

// TestLiftQuarantine: the allow verdict of a quarantine over is attached once, signed by the
// policy key, with the source lajstrom:quarantine and the repo as its scope.
func TestLiftQuarantine(t *testing.T) {
	e := newEnv(t)
	k := newVerdictKey(t)
	trust, err := attest.NewTrust([]attest.Entry{k.entry()})
	must(t, err)
	ctx := t.Context()
	e.repo.Policy = []byte(`{"quarantineNewVersions": "72h"}`)
	must(t, e.st.UpdateRepo(ctx, &e.repo))
	opts := e.options(e.prov)
	opts.Trust = trust
	e.pipe = ingest.New(opts)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "the subject"}))
	_, err = e.ingest(t, built, raw, pkgPath)
	must(t, err)
	ms, err := e.st.FindFiles(ctx, e.repo.ID, "notes")
	must(t, err)
	if len(ms) != 1 {
		t.Fatalf("%d files", len(ms))
	}
	proxy := e.repo
	proxy.Kind = index.KindProxy
	e.pipe.LiftQuarantine(ctx, proxy, ms[0], policyKey{k}, alice)
	e.pipe.LiftQuarantine(ctx, proxy, ms[0], policyKey{k}, alice)
	vs, err := e.st.ListVerdicts(ctx, index.VerdictFilter{RepoID: e.repo.ID})
	must(t, err)
	if len(vs) != 1 || vs[0].Verdict != "allow" || vs[0].Source != policy.SourceQuarantine || vs[0].Scope != "raw/scratch" ||
		vs[0].Reason != "quarantine of 72h over" {
		t.Errorf("the verdicts: %+v", vs)
	}
}
