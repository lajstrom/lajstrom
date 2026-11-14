package reindex_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/maven"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestRebuildFoldedTags is TestRebuildFormats's maven set, which has the version 1.4.0-M1, on a
// registry that folds the case of tags as Gitea and Forgejo do: the tag list names 1.4.0-m1.
// The run matches it to the tag of the manifest's annotations without case and writes the rows
// ingest wrote, the tag 1.4.0-M1 included; a second run finds every version unchanged, without
// reading its manifest again (#844).
func TestRebuildFoldedTags(t *testing.T) {
	e := newEnv(t)
	e.useFormat(t, maven.Plugin{}, provider.Map{"local": providertest.FoldCase(e.fake)})
	c := formatCases["maven"]
	rebuildFormat(t, e, c)
	pkg, err := e.st.GetPackage(t.Context(), e.repo.ID, "com.example:lib-core")
	must(t, err)
	if _, err := e.st.GetVersionByTag(t.Context(), pkg.ID, "1.4.0-M1"); err != nil {
		t.Errorf("the version under the tag ingest wrote, 1.4.0-M1: %v", err)
	}
	if r := e.run(t, reindex.Request{Verify: true}); r.Updated != c.versions || r.Added != 0 || len(r.Errors) != 0 {
		t.Errorf("a verifying run: %+v", *r)
	}
}

// mavenUpload is com.example:lib-core at version, the jar with POM or the POM alone.
func mavenUpload(version string, jar bool) format.Upload {
	pom, pi := testutil.POM(testutil.POMOptions{Version: version})
	up := format.Upload{Files: []format.UploadedFile{file(pi.Path, pom)}}
	if jar {
		b, ji := testutil.Jar(testutil.JarOptions{Version: version})
		up.Files = append(up.Files, file(ji.Path, b))
	}
	return up
}

// stage builds the manifest of up and pushes its blobs straight to the registry, as oras push
// does before it pushes the manifest. It returns what the plugin built and the manifest's bytes.
func (f *frontend) stage(t *testing.T, up format.Upload) (*format.Built, []byte) {
	t.Helper()
	ctx := t.Context()
	built, err := f.p.Build(ctx, f.e.repo, up)
	must(t, err)
	prov, _ := f.e.providers.Provider(f.e.repo.Registry)
	rr, err := prov.Repository(ctx, built.RepoPath)
	must(t, err)
	for _, b := range built.Blobs {
		rc, err := b.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, b.Descriptor, rc))
		_ = rc.Close()
	}
	raw, _, err := profile.Encode(built.Manifest)
	must(t, err)
	return built, raw
}

// straight pushes the manifest of up to the registry under tag, past Lajstrom: its blobs, then
// the manifest. It returns the manifest's descriptor and the tag of its annotations.
func (f *frontend) straight(t *testing.T, up format.Upload, tag string) (ocispec.Descriptor, string) {
	t.Helper()
	ctx := t.Context()
	built, raw := f.stage(t, up)
	prov, _ := f.e.providers.Provider(f.e.repo.Registry)
	rr, err := prov.Repository(ctx, built.RepoPath)
	must(t, err)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	must(t, rr.PushReference(ctx, desc, bytes.NewReader(raw), tag))
	return desc, built.Tag
}

// TestFoldedTagCaseVariant: on a registry that folds the case of tags, two versions that differ
// only in case are one tag. A publish of the second through Lajstrom is the 409 of a tag that
// holds another digest, and the registry keeps the first; a rebuild finds the first alone.
func TestFoldedTagCaseVariant(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.useFormat(t, maven.Plugin{}, provider.Map{"local": providertest.FoldCase(e.fake)})
	f := e.frontend(t, maven.Plugin{})
	f.push(t, mavenUpload("1.4.0-M1", true))
	before := e.derived(t)

	built, raw := f.stage(t, mavenUpload("1.4.0-m1", true))
	_, err := e.pipe.Ingest(ctx, e.repo, "com.example/lib-core", built.Tag, built.Manifest, raw, alice)
	var conflict *index.TagConflictError
	if !errors.As(err, &conflict) || conflict.Tag != "1.4.0-m1" {
		t.Fatalf("the publish of 1.4.0-m1 after 1.4.0-M1: %v, want a *index.TagConflictError on 1.4.0-m1", err)
	}
	// The fake says that it folds case, as the vendor table says of Gitea (#426).
	if want := `folds the case of tags, and "1.4.0-m1" is the tag of version 1.4.0-M1 of com.example:lib-core there`; !strings.Contains(err.Error(), want) {
		t.Errorf("the conflict: %v, want it to say %q", err, want)
	}
	same(t, "the rows after the 409", before, e.derived(t))

	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 1 || len(r.Errors) != 0 || len(r.Drift) != 0 {
		t.Errorf("the rebuild: %+v", *r)
	}
	same(t, "the rebuilt rows", before, e.derived(t))
}

// TestFoldedTagRepointed: on a registry that folds the case of tags, a manifest of 1.4.0-M1
// pushed straight to the registry re-points the tag of the version the index has; the run
// reports the drift and keeps the row, as for any tag re-pointed past Lajstrom.
func TestFoldedTagRepointed(t *testing.T) {
	e := newEnv(t)
	e.useFormat(t, maven.Plugin{}, provider.Map{"local": providertest.FoldCase(e.fake)})
	f := e.frontend(t, maven.Plugin{})
	f.push(t, mavenUpload("1.4.0-M1", false))
	before := e.derived(t)
	indexed := before.Versions["com.example:lib-core:1.4.0-M1"].Digest
	other, _ := f.straight(t, mavenUpload("1.4.0-M1", true), "1.4.0-M1")

	r := e.run(t, reindex.Request{Prune: true})
	want := []reindex.Drift{{Package: "com.example:lib-core", Tag: "1.4.0-M1", Index: indexed, Registry: other.Digest}}
	same(t, "the drift", want, r.Drift)
	if r.Added != 0 || r.Removed != 0 || len(r.Errors) != 0 {
		t.Errorf("the run: %+v", *r)
	}
	same(t, "the rows", before, e.derived(t))
}

// TestCaseExactTagMismatch: on a registry that keeps the case of tags, a manifest of 1.4.0-M1
// pushed straight under the tag 1.4.0-m1 is not stored under the tag its annotations give, and
// stays an error of the report, as ingest refuses it: matching without case needs the registry
// to resolve the annotations' tag to the manifest.
func TestCaseExactTagMismatch(t *testing.T) {
	e := newEnv(t)
	e.useFormat(t, maven.Plugin{}, e.providers)
	f := e.frontend(t, maven.Plugin{})
	desc, tag := f.straight(t, mavenUpload("1.4.0-M1", true), "1.4.0-m1")
	if tag != "1.4.0-M1" {
		t.Fatalf("the tag of the annotations: %q", tag)
	}

	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != 0 || len(r.Errors) != 1 || r.Errors[0].Tag != "1.4.0-m1" || r.Errors[0].Digest != desc.Digest ||
		!strings.Contains(r.Errors[0].Error, `"1.4.0-M1", want "1.4.0-m1"`) {
		t.Errorf("the run: %+v", *r)
	}
}
