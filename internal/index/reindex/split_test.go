package reindex_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	sp.two = pub.Tag
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
// package's whenever it was the newest version. A run keeps the package where ingest wrote it and
// a writer finds it, lists the other form with the tags at each, and leaves its version out: the
// run over ingest's rows, a verifying run, and a run from an empty index, whichever form the
// catalog lists first, all the same; and the version at the newest form downloads.
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
		if got := f.do(t, http.MethodGet, p, nil, http.StatusOK).Body.String(); got != "two "+e.repo.Name {
			t.Errorf("%s: GET %s = %q, want the version at the newest form", what, p, got)
		}
		if _, err := e.st.GetVersionByDigest(t.Context(), e.repo.ID, sp.one); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("%s: the version at the earlier form: %v, want ErrNotFound", what, err)
		}
	}
	check("the run over ingest's rows", e.run(t, reindex.Request{Prune: true}), 0, 0)
	rows := e.derived(t)
	check("a verifying run", e.run(t, reindex.Request{Prune: true, Verify: true}), 0, 1)
	must(t, e.st.ResetDerived(t.Context(), e.repo.ID))
	check("the run from an empty index", e.run(t, reindex.Request{Prune: true}), 1, 0)
	same(t, "the rows rebuilt", rows, e.derived(t))
}

// TestSplitIndexedBefore: an index that a run before #609 wrote has the version at the earlier
// form under the package at the newest one. A run leaves it unseen, and prune removes it as a
// manifest gone from the package's OCI repository: pending at first, removed once the registry
// answered MANIFEST_UNKNOWN twice and the row is older than the grace age.
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
}
