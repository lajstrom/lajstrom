package policy

import (
	"errors"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// TestViewReader: the reader of a view leaves out what it refuses in every method that returns
// a version, and its Until is the earliest expiry of the decisions it took.
func TestViewReader(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	repo := e.repo(t, "pypi", "proxy-pypi", index.KindProxy, `{"quarantineNewVersions": "72h"}`)
	pkg := index.Package{RepoID: repo.ID, Name: "requests", NameOCI: "requests"}
	must(t, e.st.UpsertPackage(ctx, &pkg))
	versions := map[string]index.Version{}
	for i, version := range []string{"2.31.0", "2.32.0", "2.32.1", "2.33.0"} {
		v := index.Version{PackageID: pkg.ID, Version: version, Tag: version, Digest: digest.FromString(version),
			ArtifactType: "application/vnd.lajstrom.pypi.v1", CreatedAt: e.now.Add(-time.Duration(100-i) * time.Hour)}
		if version == "2.33.0" {
			v.CreatedAt = e.now.Add(-time.Hour) // inside the quarantine
		}
		must(t, e.st.UpsertVersion(ctx, &v))
		must(t, e.st.ReplaceFiles(ctx, v.ID, []index.File{{Name: "requests-" + version + ".tar.gz", MediaType: "application/gzip",
			Digest: digest.FromString("file " + version), Size: 1}}))
		versions[version] = v
	}
	expires := e.now.Add(30 * time.Minute)
	r := index.PolicyRule{RepoMask: "pypi/proxy-pypi", Name: "requests", Version: "==2.32.0", Variant: "*", Effect: "deny",
		Reason: "CVE-2026-1234", CreatedBy: "user:u", ExpiresAt: &expires}
	must(t, e.st.CreatePolicyRule(ctx, &r))

	view, err := e.d.View(ctx, e.st, repo)
	must(t, err)
	rd := view.Reader(e.st)
	p, err := rd.GetPackage(ctx, repo.ID, "requests")
	must(t, err)
	vs, err := rd.ListVersions(ctx, p.ID)
	must(t, err)
	var got []string
	for _, v := range vs {
		got = append(got, v.Version)
	}
	if len(got) != 2 || got[0] != "2.31.0" || got[1] != "2.32.1" {
		t.Errorf("the versions through the view: %v, want 2.31.0 and 2.32.1", got)
	}
	if _, err := rd.GetVersion(ctx, p.ID, "2.32.0", ""); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("GetVersion of the denied version: %v", err)
	}
	if _, err := rd.GetVersionByDigest(ctx, repo.ID, versions["2.33.0"].Digest); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("GetVersionByDigest of the quarantined version: %v", err)
	}
	if v, err := rd.GetVersionByTag(ctx, p.ID, "2.31.0"); err != nil || v.Version != "2.31.0" {
		t.Errorf("GetVersionByTag of an allowed version: %v, %v", v, err)
	}
	ms, err := rd.FindFiles(ctx, repo.ID, "requests-2.32.0.tar.gz")
	must(t, err)
	if len(ms) != 0 {
		t.Errorf("FindFiles of the denied file: %d matches", len(ms))
	}
	ms, err = rd.FindFiles(ctx, repo.ID, "requests-2.32.1.tar.gz")
	must(t, err)
	if len(ms) != 1 {
		t.Errorf("FindFiles of an allowed file: %d matches", len(ms))
	}
	if u := view.Until(); u == nil || !u.Equal(expires) {
		t.Errorf("Until: %v, want the rule's expiry %v", u, expires)
	}
	viewFresh(t, e, repo, pkg)
}

// viewFresh: a reader that met no package first learns the names from the repo's list, and a
// nil view reads everything.
func viewFresh(t *testing.T, e *env, repo index.Repo, pkg index.Package) {
	ctx := t.Context()

	// A reader that met no package first learns the names from the repo's list.
	fresh, err := e.d.View(ctx, e.st, repo)
	must(t, err)
	vs, err := fresh.Reader(e.st).ListVersions(ctx, pkg.ID)
	must(t, err)
	if len(vs) != 2 {
		t.Errorf("ListVersions without a package read first: %d versions", len(vs))
	}
	// A nil view reads everything.
	var none *View
	vs, err = none.Reader(e.st).ListVersions(ctx, pkg.ID)
	must(t, err)
	if len(vs) != 4 || none.Until() != nil || !none.Decide(Subject{}).Allowed() {
		t.Errorf("a nil view: %d versions", len(vs))
	}
}
