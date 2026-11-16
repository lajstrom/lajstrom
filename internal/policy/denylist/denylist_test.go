package denylist_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/policy/denylist"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

var memoryDBs atomic.Int64

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// signer is an ECDSA key as a denylist.Signer.
type signer struct{ priv *ecdsa.PrivateKey }

func (s signer) Sign(_ context.Context, msg []byte) ([]byte, error) {
	sum := sha256.Sum256(msg)
	return ecdsa.SignASN1(rand.Reader, s.priv, sum[:])
}
func (signer) Fingerprint() string { return "fp" }

// fixture is an index with the repos pypi/proxy-pypi and oci/images, the version requests 2.32.0
// and 2.32.1, a rule that denies the first, an expired rule, and a verdict on an image.
func fixture(t *testing.T) (*sqlite.Store, index.Repo, index.Repo) {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:denylist-%d?mode=memory&cache=shared", memoryDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	pypi := index.Repo{Format: "pypi", Name: "proxy-pypi", Registry: "local", Prefix: "pypi/proxy-pypi", Kind: index.KindProxy}
	must(t, st.CreateRepo(ctx, &pypi))
	oci := index.Repo{Format: "oci", Name: "images", Registry: "local", Prefix: "oci/images", Kind: index.KindHosted}
	must(t, st.CreateRepo(ctx, &oci))
	pkg := index.Package{RepoID: pypi.ID, Name: "requests", NameOCI: "requests"}
	must(t, st.UpsertPackage(ctx, &pkg))
	for _, version := range []string{"2.32.0", "2.32.1"} {
		v := index.Version{PackageID: pkg.ID, Version: version, Variant: "py3-none-any", Tag: version, Digest: digest.FromString(version),
			ArtifactType: "application/vnd.lajstrom.pypi.v1"}
		must(t, st.UpsertVersion(ctx, &v))
		must(t, st.ReplaceFiles(ctx, v.ID, []index.File{{Name: "requests-" + version + ".whl", MediaType: "application/zip",
			Digest: digest.FromString("file " + version), Size: 1}}))
	}
	must(t, st.CreatePolicyRule(ctx, &index.PolicyRule{RepoMask: "pypi/proxy-pypi", Name: "requests", Version: "==2.32.0", Variant: "*",
		Effect: index.PolicyDeny, Reason: "CVE-2026-1234", CreatedBy: "user:u"}))
	must(t, st.CreatePolicyRule(ctx, &index.PolicyRule{RepoMask: "*/*", Name: "old", Version: "*", Variant: "*",
		Effect: index.PolicyDeny, Reason: "gone", CreatedBy: "user:u", ExpiresAt: ptr(time.Now().Add(-time.Hour))}))
	must(t, st.ReplaceOCIVerdicts(ctx, oci.ID, digest.FromString("att"), []index.Verdict{{Path: "app", Subject: digest.FromString("image"),
		Verdict: index.PolicyDeny, Reason: "malware", Source: "trivy", SignedBy: "scanner"}}))
	return st, pypi, oci
}

func ptr[T any](v T) *T { return &v }

func locate(repo index.Repo, name, version string) (string, bool) {
	if repo.Format != "pypi" {
		return "", false
	}
	return "/pypi/" + repo.Name + "/files/" + name + "/" + version + "/", true
}

// TestBuild: the deny list holds the rules in force that deny and the verdicts that refuse, the
// digests of the manifests and files they refuse, and their paths; keep leaves out the repos it
// does not keep.
func TestBuild(t *testing.T) {
	st, _, _ := fixture(t)
	doc, err := denylist.Build(t.Context(), st, nil, locate)
	must(t, err)
	if len(doc.Rules) != 1 || doc.Rules[0].Name != "requests" || len(doc.Verdicts) != 1 || doc.Verdicts[0].OCIRepository != "oci/images/app" {
		t.Fatalf("the rules and verdicts: %+v %+v", doc.Rules, doc.Verdicts)
	}
	want := map[digest.Digest]bool{digest.FromString("2.32.0"): true, digest.FromString("file 2.32.0"): true, digest.FromString("image"): true}
	if len(doc.Digests) != len(want) {
		t.Errorf("the digests: %v", doc.Digests)
	}
	for _, d := range doc.Digests {
		if !want[d] {
			t.Errorf("a digest not refused: %s", d)
		}
	}
	prefixes := strings.Join(doc.Prefixes, " ")
	for _, p := range []string{"/pypi/proxy-pypi/files/requests/2.32.0/", "/v2/oci/images/app/manifests/" + digest.FromString("image").String(),
		"/v2/pypi/proxy-pypi/requests/manifests/" + digest.FromString("2.32.0").String()} {
		if !strings.Contains(prefixes, p) {
			t.Errorf("no prefix %s in %v", p, doc.Prefixes)
		}
	}
	if strings.Contains(prefixes, "2.32.1") {
		t.Errorf("2.32.1 is refused: %v", doc.Prefixes)
	}
	onlyOCI, err := denylist.Build(t.Context(), st, func(r index.Repo) bool { return r.Format == "oci" }, locate)
	must(t, err)
	if len(onlyOCI.Rules) != 0 || len(onlyOCI.Verdicts) != 1 || len(onlyOCI.Digests) != 1 {
		t.Errorf("the list of the oci repo alone: %+v", onlyOCI)
	}
}

// TestWriter: the writer pushes the list and its signature under a tag of its generation, which
// verifies with the key; it writes nothing while the generation stays, and again when it moves.
func TestWriter(t *testing.T) {
	st, _, _ := fixture(t)
	ctx := t.Context()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	reg := &providertest.Fake{RegistryName: "local"}
	w := &denylist.Writer{Index: st, Provider: reg, Key: signer{priv}, Locate: locate}
	tag, err := w.Sync(ctx)
	must(t, err)
	gen, ok := denylist.GenerationOf(tag)
	cur, _ := st.PolicyGeneration(ctx)
	if !ok || gen != cur {
		t.Fatalf("the tag %q: generation %d, want %d", tag, gen, cur)
	}
	rr, err := reg.Repository(ctx, denylist.Repository)
	must(t, err)
	desc, err := rr.Resolve(ctx, tag)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(read(t, rr, desc), &m))
	if m.ArtifactType != denylist.ArtifactType || len(m.Layers) != 2 || m.Layers[1].Annotations[ocispec.AnnotationTitle] != denylist.SignatureTitle {
		t.Fatalf("the manifest: %+v", m)
	}
	body, sig := read(t, rr, m.Layers[0]), read(t, rr, m.Layers[1])
	raw, err := base64.StdEncoding.DecodeString(string(sig))
	must(t, err)
	sum := sha256.Sum256(body)
	if !ecdsa.VerifyASN1(&priv.PublicKey, sum[:], raw) {
		t.Error("the signature does not verify")
	}
	if again, err := w.Sync(ctx); err != nil || again != "" {
		t.Errorf("a sync without a change: %q, %v", again, err)
	}
	must(t, st.CreatePolicyRule(ctx, &index.PolicyRule{RepoMask: "*/*", Name: "evil", Version: "*", Variant: "*",
		Effect: index.PolicyDeny, Reason: "r", CreatedBy: "user:u"}))
	next, err := w.Sync(ctx)
	must(t, err)
	if next == "" || next <= tag {
		t.Errorf("after a change: %q, want a tag greater than %q", next, tag)
	}
}

// read reads the blob or manifest desc of rr.
func read(t *testing.T, rr interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}, desc ocispec.Descriptor) []byte {
	t.Helper()
	rc, err := rr.Fetch(t.Context(), desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	var b bytes.Buffer
	_, err = b.ReadFrom(rc)
	must(t, err)
	return b.Bytes()
}
