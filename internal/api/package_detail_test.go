package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// TestGetPackage: one package with the facts of its manifests, those of its head manifest (the
// latest version's first variant: the wheel), its pointers, its OCI repository and the head's
// metadata; the list has the same row without the last two. A package without versions has the
// zero facts.
func TestGetPackage(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	ctx := context.Background()
	pkg, err := f.st.GetPackage(ctx, repo.ID, "requests")
	must(t, err)
	wheel, err := f.st.GetVersion(ctx, pkg.ID, "1.0", "py3-none-any")
	must(t, err)
	wheel.Summary = "Python HTTP for Humans."
	must(t, f.st.UpsertVersion(ctx, wheel))
	empty := index.Package{RepoID: repo.ID, Name: "empty", NameOCI: "empty", CreatedAt: testNow}
	must(t, f.st.UpsertPackage(ctx, &empty))

	row := `"name": "requests", "nameOci": "requests", "createdAt": "2025-12-30T12:00:00Z", "updatedAt": "2026-01-02T12:00:00Z",
		"latestVersion": "1.0", "latestVariants": ["py3-none-any", "sdist"], "lastPublished": "1.0", "versions": 1, "manifests": 2,
		"size": 196210, "summary": "Python HTTP for Humans.", "attrs": {"lajstrom.pypi.nonstandard": "true"},
		"pointers": [{"kind": "dist-tag", "name": "latest", "version": "1.0"}]`
	var got any
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/requests", "", admin), http.StatusOK, &got)
	wantJSON(t, "package", got, `{`+row+`, "ociRepository": "pypi/libs/requests",
		"latestMetadata": {"name": "Requests", "summary": "HTTP"}}`)

	emptyRow := `"name": "empty", "nameOci": "empty", "createdAt": "2026-01-02T12:00:00Z", "updatedAt": "2026-01-02T12:00:00Z",
		"latestVersion": null, "latestVariants": [], "lastPublished": null, "versions": 0, "manifests": 0, "size": 0,
		"summary": null, "attrs": {}, "pointers": []`
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/empty", "", admin), http.StatusOK, &got)
	wantJSON(t, "a package without versions", got, `{`+emptyRow+`, "ociRepository": "pypi/libs/empty", "latestMetadata": null}`)

	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages", "", admin), http.StatusOK, &got)
	wantJSON(t, "list", got, `{"packages": [{`+emptyRow+`}, {`+row+`}]}`)
}

// pickOldest is the format.LatestPicker of TestPackageLatest: the oldest version, or none when
// the state yanks it; it keeps what it was given.
type pickOldest struct {
	format.Plugin
	versions []string
	state    []index.JournalState
}

func (p *pickOldest) LatestVersion(versions []string, state []index.JournalState) string {
	p.versions, p.state = versions, state
	oldest := versions[len(versions)-1]
	if yankOf(state, oldest) != nil {
		return ""
	}
	return oldest
}

// TestPackageLatest: latestVersion is the version the plugin of the format picks, given the
// versions newest first and the journal state, and lastPublished the newest; pointers are the
// dist-tags that name a version of the package. A plugin that picks none leaves latestVersion
// null.
func TestPackageLatest(t *testing.T) {
	f := newFixture(t, Options{})
	picker := &pickOldest{}
	f.repos.Latest = func(name string) (format.LatestPicker, bool) { return picker, name == "pypi" }
	repo := f.create(t, "pypi", "libs")
	publish(t, f.st, repo.ID, "requests", "1.0", testNow.Add(-2*time.Hour), "py3-none-any", "sdist")
	pkg := publish(t, f.st, repo.ID, "requests", "2.0rc1", testNow.Add(-time.Hour))
	ctx := context.Background()
	must(t, f.st.ReplaceJournalState(ctx, pkg.ID, []index.JournalState{
		{Kind: index.JournalDistTag, Key: "next", Value: "2.0rc1", Seq: 1},
		{Kind: index.JournalDistTag, Key: "gone", Value: "9.9", Seq: 2},
	}))
	var got PackageDetail
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/requests", "", admin), http.StatusOK, &got)
	wantLatest(t, "the pick", got.PackageEntry, "1.0", []string{"py3-none-any", "sdist"}, "2.0rc1")
	if want := []PackagePointer{{Pointer: format.Pointer{Kind: "dist-tag", Name: "next"}, Version: "2.0rc1"}}; !slices.Equal(got.Pointers, want) {
		t.Errorf("pointers %+v, want %+v", got.Pointers, want)
	}
	if !slices.Equal(picker.versions, []string{"2.0rc1", "1.0"}) || len(picker.state) != 2 {
		t.Errorf("the picker was given %q and %d rows of state, want [2.0rc1 1.0] and 2", picker.versions, len(picker.state))
	}

	must(t, f.st.ReplaceJournalState(ctx, pkg.ID, []index.JournalState{{Kind: index.JournalYanked, Key: "1.0", Seq: 3}}))
	var list PackageList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages", "", admin), http.StatusOK, &list)
	wantLatest(t, "no pick", list.Packages[0], "", []string{}, "2.0rc1")
}

// TestPackageLatestRaw: a format whose plugin picks no latest version has the newest.
func TestPackageLatestRaw(t *testing.T) {
	f := newFixture(t, Options{})
	f.repos.Latest = func(string) (format.LatestPicker, bool) { return nil, false }
	raw := f.create(t, "raw", "files")
	publish(t, f.st, raw.ID, "tool", "a1", testNow.Add(-time.Hour))
	publish(t, f.st, raw.ID, "tool", "b2", testNow)
	var list PackageList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/raw/files/packages", "", admin), http.StatusOK, &list)
	wantLatest(t, "raw", list.Packages[0], "b2", []string{}, "b2")
}

// wantLatest fails the test unless e has the latest version (null for ""), its variants and the
// last published version.
func wantLatest(t *testing.T, what string, e PackageEntry, latest string, variants []string, last string) {
	t.Helper()
	if (e.LatestVersion == nil) != (latest == "") || (e.LatestVersion != nil && *e.LatestVersion != latest) ||
		!slices.Equal(e.LatestVariants, variants) || e.LastPublished == nil || *e.LastPublished != last {
		t.Errorf("%s: latestVersion %v %q, lastPublished %v; want %q %q, %q", what, e.LatestVersion, e.LatestVariants, e.LastPublished,
			latest, variants, last)
	}
}

// TestGetPackageNotFound: a repo or a package that does not exist is 404 naming it, and so is a
// package of a group, though a member has it; a raw name travels as one segment.
func TestGetPackageNotFound(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	raw := f.create(t, "raw", "files")
	publish(t, f.st, raw.ID, "a/b/c.txt", "0123456789abcdef", testNow)
	wantStatus(t, f.do(http.MethodGet, "/api/v1/repos/raw/files/packages/a%2Fb%2Fc.txt", "", admin), http.StatusOK)
	for _, tt := range []struct{ target, want string }{
		{"/api/v1/repos/pypi/none/packages/requests", "repo pypi/none does not exist"},
		{"/api/v1/repos/pypi/libs/packages/flask", `pypi/libs has no package "flask"`},
	} {
		wantError(t, f.do(http.MethodGet, tt.target, "", admin), http.StatusNotFound, codeNotFound, tt.want)
	}

	g := newGroupFixture(t)
	wantStatus(t, g.do(http.MethodPost, "/api/v1/repos",
		`{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["internal", "npmjs"]}}`, admin), http.StatusCreated)
	internal, err := g.st.GetRepo(context.Background(), "npm", "internal")
	must(t, err)
	publish(t, g.st, internal.ID, "left-pad", "1.0", testNow)
	wantStatus(t, g.do(http.MethodGet, "/api/v1/repos/npm/internal/packages/left-pad", "", admin), http.StatusOK)
	wantError(t, g.do(http.MethodGet, "/api/v1/repos/npm/all/packages/left-pad", "", admin),
		http.StatusNotFound, codeNotFound, `npm/all has no package "left-pad"`)
}

// TestGetPackageAccess: one package needs read, by the rule of docs/spec/rbac.md §3: 401 for
// anonymous and 404 for a principal that cannot read the repo, before the package is looked up.
func TestGetPackageAccess(t *testing.T) {
	f := newFixture(t, Options{Auth: fakeAuth{grants: map[string]bool{"user:alice read pypi/libs": true}}})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	target := "/api/v1/repos/pypi/libs/packages/requests"
	wantStatus(t, f.do(http.MethodGet, target, "", alice), http.StatusOK)
	wantError(t, f.do(http.MethodGet, target, "", bob), http.StatusNotFound, codeNotFound)
	wantError(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/flask", "", bob), http.StatusNotFound, codeNotFound)
	wantError(t, f.do(http.MethodGet, target, "", auth.Anonymous), http.StatusUnauthorized, codeUnauthenticated)
}
