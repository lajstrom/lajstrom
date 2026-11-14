package reindex_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// pushRawAt pushes the raw manifest of content at path straight to the OCI repository name of
// prov, as an earlier Lajstrom wrote it, and returns its tag and digest.
func pushRawAt(t *testing.T, prov provider.Provider, repo index.Repo, name, path, content string) (string, digest.Digest) {
	t.Helper()
	ctx := t.Context()
	built, err := raw.Plugin{}.Build(ctx, repo, rawUpload(path, content))
	must(t, err)
	rr, err := prov.Repository(ctx, name)
	must(t, err)
	for _, b := range built.Blobs {
		if ok, err := rr.Exists(ctx, b.Descriptor); err == nil && ok {
			continue
		}
		rc, err := b.Open(ctx)
		must(t, err)
		err = rr.Push(ctx, b.Descriptor, rc)
		_ = rc.Close()
		skipNameInvalid(t, name, err)
		must(t, err)
	}
	b, d, err := profile.Encode(built.Manifest)
	must(t, err)
	err = rr.PushReference(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(b))},
		bytes.NewReader(b), built.Tag)
	skipNameInvalid(t, name, err)
	must(t, err)
	return built.Tag, d
}

// skipNameInvalid skips the test when the registry refused the OCI name with 400 NAME_INVALID:
// Nexus's oci format and Gitea take only the grammar of distribution-spec 1.0, which refuses
// `--` and `__` in a component. Lajstrom wrote such names only before ADR 0017, so no version
// can be at one on that registry (#800).
func skipNameInvalid(t *testing.T, name string, err error) {
	t.Helper()
	var r *provider.RejectedError
	if errors.As(err, &r) && r.Code == "NAME_INVALID" {
		t.Skipf("the registry refuses the name %s, written before ADR 0017: %v", name, err)
	}
}

// split is a raw file stored at the earliest form of its OCI name (ADR 0017), as an earlier
// Lajstrom wrote it, and a version of it published while the index lacked that form, which
// ingest wrote at the newest form: one package at two OCI repositories (#609).
type split struct {
	path, newest, earlier string        // the raw path and its package paths
	oneTag                string        // the version at the earlier form
	one                   digest.Digest // its manifest
	two                   string        // the tag of the version at the newest form
	twoDigest             digest.Digest // its manifest
}

func (e *env) split(t *testing.T, p string) split {
	t.Helper()
	newest, err := raw.Plugin{}.EscapeName(p)
	must(t, err)
	forms := profile.EarlierPackagePaths(newest)
	sp := split{path: p, newest: newest, earlier: forms[len(forms)-1]}
	prov, _ := e.providers.Provider("local")
	sp.oneTag, sp.one = pushRawAt(t, prov, e.repo, e.repo.Prefix+"/"+sp.earlier, p, "one "+e.repo.Name)
	pub := e.publish(t, p, "two "+e.repo.Name)
	if pub.RepoPath != e.repo.Prefix+"/"+newest {
		t.Fatalf("%s: the publish went to %s, want %s/%s", p, pub.RepoPath, e.repo.Prefix, newest)
	}
	sp.two, sp.twoDigest = pub.Tag, pub.Digest
	return sp
}

// TestRebuildSplit is #609 for each kind of name that ADR 0017 changes (rebuildSplit).
func TestRebuildSplit(t *testing.T) {
	for _, p := range []string{"netlify/blobs", "a--b.txt", ".gitignore", "k8s/manifests/app.yaml"} {
		t.Run(p, func(t *testing.T) { rebuildSplit(t, newEnv(t), p) })
	}
}

// rebuildSplit: before #609, the version at the earlier form was indexed under the newest
// form's OCI repository, which does not hold it: its download was a 404 BLOB_UNKNOWN, as was the
// package's whenever it was the newest version; #609 left it out of the index. A run keeps the
// package where ingest wrote it and a writer finds it, indexes the version at the earlier form
// with that form in OCIRepo (#642), and lists the other form with the tags at each: the run over
// ingest's rows, a verifying run, and a run from an empty index, whichever form the catalog lists
// first, all the same; and each version downloads from where it is.
func rebuildSplit(t *testing.T, e *env, p string) {
	t.Helper()
	sp := e.split(t, p)
	f := e.frontend(t, raw.Plugin{})
	check := func(what string, r *reindex.RepoReport, added, updated int) {
		t.Helper()
		same(t, what+": the split", []reindex.Split{{Package: p, Indexed: sp.newest, IndexedTags: []string{sp.two},
			Other: sp.earlier, OtherTags: []string{sp.oneTag}}}, r.Split)
		if r.Added != added || r.Updated != updated || len(r.Drift) != 0 || len(r.Errors) != 0 {
			t.Errorf("%s: %+v", what, *r)
		}
		for d, want := range map[digest.Digest]string{sp.one: "one ", sp.twoDigest: "two "} {
			if got := f.do(t, http.MethodGet, p+"?digest="+d.String(), nil, http.StatusOK).Body.String(); got != want+e.repo.Name {
				t.Errorf("%s: GET %s pinned to %s = %q, want %q", what, p, d, got, want+e.repo.Name)
			}
		}
		for d, want := range map[digest.Digest]string{sp.one: sp.earlier, sp.twoDigest: ""} {
			if v, err := e.st.GetVersionByDigest(t.Context(), e.repo.ID, d); err != nil || v.OCIRepo != want {
				t.Errorf("%s: the version of %s: %+v, %v; want OCIRepo %q", what, d, v, err, want)
			}
		}
	}
	check("the run over ingest's rows", e.run(t, reindex.Request{Prune: true}), 1, 0)
	rows := e.derived(t)
	check("a run again", e.run(t, reindex.Request{Prune: true}), 0, 0)
	check("a verifying run", e.run(t, reindex.Request{Prune: true, Verify: true}), 0, 2)
	must(t, e.st.ResetDerived(t.Context(), e.repo.ID))
	check("the run from an empty index", e.run(t, reindex.Request{Prune: true}), 2, 0)
	same(t, "the rows rebuilt", rows, e.derived(t))
}

// TestSplitIndexedBefore: an index that a run before #609 wrote has the version at the earlier
// form under the package at the newest one, without OCIRepo. A run leaves it unseen, and prune
// removes it as a manifest gone from the package's OCI repository: pending at first, removed once
// the registry answered MANIFEST_UNKNOWN twice and the row is older than the grace age; the run
// after it indexes the version where it is (#642).
func TestSplitIndexedBefore(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	sp := e.split(t, "netlify/blobs")
	rr, err := e.fake.Repository(ctx, "raw/scratch/"+sp.earlier)
	must(t, err)
	desc, err := rr.Resolve(ctx, sp.oneTag)
	must(t, err)
	rc, err := rr.Fetch(ctx, desc)
	must(t, err)
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(b, &m))
	rows, err := e.pipe.Inspect(ctx, e.repo, sp.earlier, sp.oneTag, m, b, ingest.InspectOptions{})
	must(t, err)
	must(t, e.st.Tx(ctx, func(w index.Writer) error { return ingest.WriteRows(ctx, w, rows, time.Now()) }))
	if rows.Package.NameOCI != sp.newest {
		t.Fatalf("the row of before #609 is at %s, want %s", rows.Package.NameOCI, sp.newest)
	}

	if r := e.run(t, reindex.Request{Prune: true}); r.Pending != 1 || r.Removed != 0 || len(r.Split) != 1 {
		t.Errorf("the first run: %+v", *r)
	}
	e.clock.later(2 * time.Hour)
	if r := e.run(t, reindex.Request{Prune: true}); r.Removed != 1 || r.Pending != 0 || len(r.Split) != 1 {
		t.Errorf("the run two hours later: %+v", *r)
	}
	if _, err := e.st.GetVersionByDigest(ctx, e.repo.ID, sp.one); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the row of the version at the earlier form: %v, want ErrNotFound", err)
	}
	if r := e.run(t, reindex.Request{Prune: true}); r.Added != 1 || r.Removed != 0 || len(r.Split) != 1 {
		t.Errorf("the run after: %+v", *r)
	}
	if v, err := e.st.GetVersionByDigest(ctx, e.repo.ID, sp.one); err != nil || v.OCIRepo != sp.earlier {
		t.Errorf("the row of the version at the earlier form after: %+v, %v; want OCIRepo %s", v, err, sp.earlier)
	}
}

// splitRow writes the row of the version at the earlier form of sp as reindex indexes it since
// #642: under the package at the newest form, with the earlier form in OCIRepo.
func (e *env) splitRow(t *testing.T, sp split) {
	t.Helper()
	ctx := t.Context()
	rr, err := e.fake.Repository(ctx, e.repo.Prefix+"/"+sp.earlier)
	must(t, err)
	desc, err := rr.Resolve(ctx, sp.oneTag)
	must(t, err)
	rc, err := rr.Fetch(ctx, desc)
	must(t, err)
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(b, &m))
	rows, err := e.pipe.Inspect(ctx, e.repo, sp.earlier, sp.oneTag, m, b, ingest.InspectOptions{})
	must(t, err)
	rows.Version.OCIRepo = sp.earlier
	must(t, e.st.Tx(ctx, func(w index.Writer) error { return ingest.WriteRows(ctx, w, rows, e.clock.now()) }))
	if rows.Package.NameOCI != sp.newest {
		t.Fatalf("the package of the split row is at %s, want %s", rows.Package.NameOCI, sp.newest)
	}
}

// TestSplitRowServed: a version whose manifest is at the other form of its package's OCI name
// (versions.oci_repo, #642) downloads from there, the sweep finds it there and keeps it, and the
// delete of the file deletes it there, with the version at the newest form.
func TestSplitRowServed(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	const p = "netlify/blobs"
	sp := e.split(t, p)
	e.splitRow(t, sp)
	f := e.frontend(t, raw.Plugin{})
	get := func(what string) {
		t.Helper()
		if got := f.do(t, http.MethodGet, p+"?digest="+sp.one.String(), nil, http.StatusOK).Body.String(); got != "one "+e.repo.Name {
			t.Errorf("%s: GET %s pinned to the version at the earlier form = %q", what, p, got)
		}
	}
	get("written")

	for i, what := range []string{"the first run", "a run two hours later"} {
		if i > 0 {
			e.clock.later(2 * time.Hour)
		}
		r := e.run(t, reindex.Request{Prune: true})
		if r.Removed != 0 || r.Pending != 0 || len(r.Errors) != 0 {
			t.Errorf("%s: %+v, want the row at the earlier form kept", what, *r)
		}
		if v, err := e.st.GetVersionByDigest(ctx, e.repo.ID, sp.one); err != nil || v.OCIRepo != sp.earlier {
			t.Fatalf("%s: the row at the earlier form: %+v, %v", what, v, err)
		}
		get(what)
	}

	f.do(t, http.MethodDelete, p, nil, http.StatusNoContent)
	for _, form := range []string{sp.earlier, sp.newest} {
		rr, err := e.fake.Repository(ctx, e.repo.Prefix+"/"+form)
		must(t, err)
		if err := rr.Tags(ctx, "", func(tags []string) error {
			if len(tags) > 0 {
				t.Errorf("the delete left the tags %q at %s", tags, form)
			}
			return nil
		}); err != nil && !errors.Is(err, provider.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if _, err := e.st.GetPackage(ctx, e.repo.ID, p); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the package after the delete of both versions: %v, want ErrNotFound", err)
	}
}

// TestSplitBothForms: a version at both forms of a split keeps the row of the indexed form,
// which a run reads first from an empty index too; its tag at the other form is not indexed, and
// the split lists it in both forms' tags (#642).
func TestSplitBothForms(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	const p = "a--b.txt"
	sp := e.split(t, p)
	prov, _ := e.providers.Provider("local")
	tag, copied := pushRawAt(t, prov, e.repo, e.repo.Prefix+"/"+sp.earlier, p, "two "+e.repo.Name)
	if tag != sp.two {
		t.Fatalf("the copy of the version at the newest form has the tag %s, want %s", tag, sp.two)
	}
	want := []reindex.Split{{Package: p, Indexed: sp.newest, IndexedTags: []string{sp.two}, Other: sp.earlier,
		OtherTags: slices.Sorted(slices.Values([]string{sp.oneTag, sp.two}))}}
	for _, what := range []string{"the run over ingest's rows", "the run from an empty index"} {
		if what != "the run over ingest's rows" {
			must(t, e.st.ResetDerived(ctx, e.repo.ID))
		}
		r := e.run(t, reindex.Request{Prune: true})
		same(t, what+": the split", want, r.Split)
		if len(r.Drift) != 0 || len(r.Errors) != 0 {
			t.Errorf("%s: %+v", what, *r)
		}
		v, err := e.st.GetVersionByTag(ctx, mustPackage(t, e, p).ID, sp.two)
		if err != nil || v.OCIRepo != "" || v.Digest != sp.twoDigest {
			t.Errorf("%s: the version at both forms: %+v, %v; want the row of the newest form", what, v, err)
		}
		if copied != sp.twoDigest {
			if _, err := e.st.GetVersionByDigest(ctx, e.repo.ID, copied); !errors.Is(err, index.ErrNotFound) {
				t.Errorf("%s: the copy at the earlier form: %v, want ErrNotFound", what, err)
			}
		}
	}
}

// mustPackage returns the package name of the env's repo.
func mustPackage(t *testing.T, e *env, name string) *index.Package {
	t.Helper()
	pkg, err := e.st.GetPackage(t.Context(), e.repo.ID, name)
	must(t, err)
	return pkg
}

// TestSplitConsolidated: the operator copies the version at the other form into the indexed one,
// byte for byte, and deletes the other form (docs/operations.md); the next run finds the version
// at the indexed form, moves its row there, and reports no split (#642).
func TestSplitConsolidated(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	const p = "k8s/manifests/app.yaml"
	sp := e.split(t, p)
	if r := e.run(t, reindex.Request{Prune: true}); r.Added != 1 || len(r.Split) != 1 {
		t.Fatalf("the first run: %+v", *r)
	}
	from, err := e.fake.Repository(ctx, e.repo.Prefix+"/"+sp.earlier)
	must(t, err)
	to, err := e.fake.Repository(ctx, e.repo.Prefix+"/"+sp.newest)
	must(t, err)
	desc, err := from.Resolve(ctx, sp.oneTag)
	must(t, err)
	b := fetchAll(t, from, desc)
	var m ocispec.Manifest
	must(t, json.Unmarshal(b, &m))
	for _, d := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		must(t, to.Push(ctx, d, bytes.NewReader(fetchAll(t, from, d))))
	}
	must(t, to.PushReference(ctx, desc, bytes.NewReader(b), sp.oneTag))
	must(t, from.Delete(ctx, desc))

	r := e.run(t, reindex.Request{Prune: true})
	if len(r.Split) != 0 || r.Added != 1 || r.Removed != 0 || len(r.Drift) != 0 || len(r.Errors) != 0 {
		t.Errorf("the run after the copy: %+v", *r)
	}
	if v, err := e.st.GetVersionByDigest(ctx, e.repo.ID, sp.one); err != nil || v.OCIRepo != "" {
		t.Errorf("the copied version: %+v, %v; want its row at the indexed form", v, err)
	}
	f := e.frontend(t, raw.Plugin{})
	if got := f.do(t, http.MethodGet, p+"?digest="+sp.one.String(), nil, http.StatusOK).Body.String(); got != "one "+e.repo.Name {
		t.Errorf("GET %s pinned to the copied version = %q", p, got)
	}
}

// fetchAll returns the content of desc in rr.
func fetchAll(t *testing.T, rr provider.Repository, desc ocispec.Descriptor) []byte {
	t.Helper()
	rc, err := rr.Fetch(t.Context(), desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	must(t, err)
	return b
}

// TestSplitOtherBytes: a version indexed at the other form of a split whose tag the indexed form
// holds with other bytes keeps its row where it is, and the tag at the indexed form is drift.
func TestSplitOtherBytes(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	const p = ".gitignore"
	sp := e.split(t, p)
	if r := e.run(t, reindex.Request{Prune: true}); r.Added != 1 || len(r.Split) != 1 {
		t.Fatalf("the first run: %+v", *r)
	}
	from, err := e.fake.Repository(ctx, e.repo.Prefix+"/"+sp.earlier)
	must(t, err)
	to, err := e.fake.Repository(ctx, e.repo.Prefix+"/"+sp.newest)
	must(t, err)
	desc, err := from.Resolve(ctx, sp.oneTag)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(fetchAll(t, from, desc), &m))
	for _, d := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		must(t, to.Push(ctx, d, bytes.NewReader(fetchAll(t, from, d))))
	}
	m.Annotations[ocispec.AnnotationCreated] = "2020-01-02T03:04:05Z"
	b, other, err := profile.Encode(m)
	must(t, err)
	must(t, to.PushReference(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: other, Size: int64(len(b))},
		bytes.NewReader(b), sp.oneTag))

	r := e.run(t, reindex.Request{Prune: true})
	same(t, "the drift", []reindex.Drift{{Package: p, Tag: sp.oneTag, Index: sp.one, Registry: other}}, r.Drift)
	if r.Removed != 0 || len(r.Errors) != 0 {
		t.Errorf("the run: %+v", *r)
	}
	if v, err := e.st.GetVersionByDigest(ctx, e.repo.ID, sp.one); err != nil || v.OCIRepo != sp.earlier {
		t.Errorf("the version at the other form: %+v, %v; want its row kept there", v, err)
	}
}
