package policy

import (
	"errors"
	"strings"
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

// TestViewRequirements: a render leaves out a version that misses a requirement, whatever an
// allow rule says, and so does the render of a group of its member's (policy.md §11), whose
// rules hold through the group too; the group's Until is the earliest of its members'.
func TestViewRequirements(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.d.o.Trust = trustOf(t, map[string][]string{"release": nil})
	member := e.repo(t, "deb", "main", index.KindHosted, requiring(t, index.Requirement{PredicateType: spdx, SignedBy: []string{"release"}}))
	group := index.Repo{Format: "deb", Name: "all", Kind: index.KindGroup}
	must(t, e.st.CreateRepo(ctx, &group))
	must(t, e.st.SetGroupMembers(ctx, group.ID, []int64{member.ID}))
	pkg := index.Package{RepoID: member.ID, Name: "app", NameOCI: "app"}
	must(t, e.st.UpsertPackage(ctx, &pkg))
	sbom := [3]string{index.AttachmentSBOM, spdx + "/v2.3", "release"}
	e.version(t, member, pkg, "1.0", "amd64", sbom)
	e.version(t, member, pkg, "1.0", "arm64")
	e.version(t, member, pkg, "2.0", "amd64", sbom)
	e.version(t, member, pkg, "3.0", "amd64")
	for _, version := range []string{"2.0", "3.0"} {
		v, err := e.st.GetVersion(ctx, pkg.ID, version, "amd64")
		must(t, err)
		must(t, e.st.ReplaceFiles(ctx, v.ID, []index.File{{Name: "app_" + version + "_amd64.deb", MediaType: "application/vnd.debian.binary-package",
			Digest: digest.FromString(version), Size: 1}}))
	}
	e.rule(t, "deb/*", "app", "*", "allow", "ours")
	expires := e.now.Add(time.Hour)
	r := index.PolicyRule{RepoMask: "deb/main", Name: "app", Version: "==2.0", Variant: "*", Effect: "deny", Reason: "bad",
		CreatedBy: "user:u", ExpiresAt: &expires}
	must(t, e.st.CreatePolicyRule(ctx, &r))

	for _, repo := range []index.Repo{member, group} {
		view, err := e.d.View(ctx, e.st, repo)
		must(t, err)
		rd := view.Reader(e.st)
		pkgs, err := rd.ListPackages(ctx, member.ID, "")
		must(t, err)
		vs, err := rd.ListVersions(ctx, pkgs[0].ID)
		must(t, err)
		var got []string
		for _, v := range vs {
			got = append(got, v.Version+"/"+v.Variant)
		}
		if strings.Join(got, " ") != "1.0/amd64 2.0/amd64" {
			t.Errorf("the versions through the view of %s: %v, want those with the SBOM, the allow rule's exception to the deny rule", repo.Name, got)
		}
		for file, want := range map[string]int{"app_2.0_amd64.deb": 1, "app_3.0_amd64.deb": 0} {
			ms, err := rd.FindFiles(ctx, member.ID, file)
			must(t, err)
			if len(ms) != want {
				t.Errorf("FindFiles of %s through the view of %s: %d matches, want %d", file, repo.Name, len(ms), want)
			}
		}
		// The deny rule that the allow rule overrides expires: the documents of the member, and
		// the group's, are rendered again then.
		if u := view.Until(); u == nil || !u.Equal(expires) {
			t.Errorf("the Until of %s: %v, want the member's rule's expiry %v", repo.Name, u, expires)
		}
		if repo.Kind == index.KindGroup {
			if s := view.Decide(Subject{Repo: group, Name: "app", Version: "3.0"}); !s.Allowed() {
				t.Errorf("a group's own decision: %+v; a group has no policy", s)
			}
		}
	}
}
