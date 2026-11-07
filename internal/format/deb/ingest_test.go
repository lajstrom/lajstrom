package deb

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// alice is the principal of the uploads through the pipeline.
var alice = auth.Principal{Kind: auth.KindUser, Name: "alice"}

// pipeline is the real ingest pipeline over the real SQLite store in memory and the fake
// registry "local", with the apt repo ubuntu.
type pipeline struct {
	st   *sqlite.Store
	prov *providertest.Fake
	pipe *ingest.Pipeline
	repo index.Repo
}

func newPipeline(t *testing.T) *pipeline {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:deb-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	p := &pipeline{st: st, prov: &providertest.Fake{RegistryName: "local"},
		repo: index.Repo{Format: Name, Name: "ubuntu", Registry: "local", Prefix: "apt/ubuntu", Kind: "hosted"}}
	must(t, st.CreateRepo(ctx, &p.repo))
	p.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": p.prov}, Index: st, Log: slog.New(slog.DiscardHandler),
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	return p
}

// upload uploads deb to suite/main as alice.
func (p *pipeline) upload(t *testing.T, deb []byte, suite string) *format.Published {
	t.Helper()
	return p.uploadTo(t, deb, suite, "main")
}

// uploadTo uploads deb to suite/component as alice.
func (p *pipeline) uploadTo(t *testing.T, deb []byte, suite, component string) *format.Published {
	t.Helper()
	pub, err := p.pipe.Upload(t.Context(), p.repo, upload([]format.UploadedFile{debFile(deb)}, ParamSuite, suite, ParamComponent, component), alice)
	if err != nil {
		t.Fatalf("upload to %s/%s: %v", suite, component, err)
	}
	return pub
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestUploadToTwoSuites checks that the same .deb uploaded to jammy and to jammy-updates is two
// versions of the package, both listed by ListVersions, and two manifests over the same blobs:
// the second upload pushes its manifest and no blob, and the lookup by file name (#42) finds
// both versions with one digest. A third upload to jammy is the idempotent repeat.
func TestUploadToTwoSuites(t *testing.T) {
	p := newPipeline(t)
	ctx := t.Context()
	deb, info := testutil.Deb(testutil.DebOptions{})
	var pubs []*format.Published
	var pushes []int // the blobs and the manifests pushed so far, after each upload
	for _, suite := range []string{"jammy", "jammy-updates", "jammy"} {
		pubs = append(pubs, p.upload(t, deb, suite))
		pushes = append(pushes, len(p.prov.Pushes()), len(p.prov.PushReferences()))
	}
	if !slices.Equal(pushes, []int{3, 1, 3, 2, 3, 2}) {
		t.Errorf("blobs and manifests pushed after each upload: %v, want the config, the .deb and the metadata once, "+
			"a manifest per suite", pushes)
	}
	if pubs[0].Tag != "1.0.0-1__jammy_2fmain_2famd64" || pubs[1].Tag != "1.0.0-1__jammy-updates_2fmain_2famd64" ||
		pubs[0].Digest == pubs[1].Digest || pubs[2].Digest != pubs[0].Digest {
		t.Errorf("published %s %s, %s %s, %s %s", pubs[0].Tag, pubs[0].Digest, pubs[1].Tag, pubs[1].Digest, pubs[2].Tag, pubs[2].Digest)
	}

	pkg, err := p.st.GetPackage(ctx, p.repo.ID, info.Package)
	must(t, err)
	vs, err := p.st.ListVersions(ctx, pkg.ID)
	must(t, err)
	var got []string // variant and suite of each version
	for _, v := range vs {
		got = append(got, v.Variant+" "+v.Attrs[profile.AnnotationDebSuite])
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"jammy-updates/main/amd64 jammy-updates", "jammy/main/amd64 jammy"}) {
		t.Errorf("ListVersions: variant and suite %q", got)
	}
	matches, err := p.st.FindFiles(ctx, p.repo.ID, info.Filename)
	must(t, err)
	fileDigest := digest.Digest("sha256:" + info.SHA256)
	if len(matches) != 2 || matches[0].File.Digest != fileDigest || matches[1].File.Digest != fileDigest {
		t.Errorf("FindFiles(%s) = %d matches, want two with the digest %s", info.Filename, len(matches), fileDigest)
	}
}

// TestIngestArchitectureVariant checks that an oras push in the shape of #39, the architecture
// alone as the variant and the tag of it, over blobs the registry has, is ErrInvalid and says
// what the variant and the tag must be.
func TestIngestArchitectureVariant(t *testing.T) {
	p := newPipeline(t)
	deb, info := testutil.Deb(testutil.DebOptions{})
	p.upload(t, deb, "jammy")
	b, err := Plugin{}.Build(t.Context(), p.repo, upload([]format.UploadedFile{debFile(deb)}))
	must(t, err)
	m := b.Manifest
	m.Annotations[profile.AnnotationVariant] = info.Architecture
	raw, _, err := profile.Encode(m)
	must(t, err)
	_, err = p.pipe.Ingest(t.Context(), p.repo, "hello", "1.0.0-1__amd64", m, raw, alice)
	want := `lajstrom.variant "amd64" is the architecture alone; the variant of a deb manifest is lajstrom.deb.suite, ` +
		`lajstrom.deb.component and lajstrom.deb.arch joined with "/": "jammy/main/amd64", and its tag 1.0.0-1__jammy_2fmain_2famd64`
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Errorf("Ingest of the variant amd64 = %v, want ErrInvalid containing %q", err, want)
	}
}
