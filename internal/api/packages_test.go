package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// publish adds version with variants (none: the variant "") to the package name of a repo,
// published at, and returns the package; a variant's digest is that of name, version and
// variant, its tag version__variant.
func publish(t *testing.T, st index.Store, repoID int64, name, version string, at time.Time, variants ...string) index.Package {
	t.Helper()
	ctx := context.Background()
	pkg := index.Package{RepoID: repoID, Name: name, NameOCI: url.PathEscape(name)}
	must(t, st.UpsertPackage(ctx, &pkg))
	if len(variants) == 0 {
		variants = []string{""}
	}
	for _, variant := range variants {
		v := index.Version{PackageID: pkg.ID, Version: version, Variant: variant, Tag: version + "__" + variant,
			Digest: digest.FromString(name + version + variant), ArtifactType: "application/vnd.lajstrom.pypi.v1", CreatedAt: at}
		must(t, st.UpsertVersion(ctx, &v))
	}
	return pkg
}

// TestListPackages: packages by name, a page at a time; the cursor holds the place of a name,
// so a package published between two pages before that name shifts nothing; prefix keeps the
// names that start with it.
func TestListPackages(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	for _, n := range []string{"flask", "attrs", "requests", "django"} {
		publish(t, f.st, repo.ID, n, "1.0", testNow)
	}
	target := "/api/v1/repos/pypi/libs/packages"
	var first PackageList
	decodeJSON(t, f.do(http.MethodGet, target+"?limit=2", "", admin), http.StatusOK, &first)
	if got := names(first); !slices.Equal(got, []string{"attrs", "django"}) || first.Next == "" {
		t.Fatalf("first page %v, next %q", got, first.Next)
	}
	if !first.Packages[0].CreatedAt.Equal(testNow) {
		t.Errorf("createdAt %v, want %v", first.Packages[0].CreatedAt, testNow)
	}
	publish(t, f.st, repo.ID, "boto", "1.0", testNow) // before the cursor's name: on no later page
	var second PackageList
	decodeJSON(t, f.do(http.MethodGet, target+"?limit=2&cursor="+first.Next, "", admin), http.StatusOK, &second)
	if got := names(second); !slices.Equal(got, []string{"flask", "requests"}) || second.Next != "" {
		t.Errorf("second page %v, next %q", got, second.Next)
	}
	for _, tt := range []struct {
		prefix string
		want   []string
	}{{"fl", []string{"flask"}}, {"zz", []string{}}} {
		var l PackageList
		decodeJSON(t, f.do(http.MethodGet, target+"?prefix="+tt.prefix, "", admin), http.StatusOK, &l)
		if got := names(l); !slices.Equal(got, tt.want) || l.Packages == nil {
			t.Errorf("prefix %s: %v, want %v", tt.prefix, got, tt.want)
		}
	}
}

// names are the names of the packages of l.
func names(l PackageList) []string {
	out := []string{}
	for _, p := range l.Packages {
		out = append(out, p.Name)
	}
	return out
}

// TestListVersions: versions newest first by the publish time of their first variant, then by
// the version string; each with its variants sorted, the times of its first and its newest
// variant, and the dist-tags that name it.
func TestListVersions(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	day := 24 * time.Hour
	publish(t, f.st, repo.ID, "requests", "1.0", testNow.Add(-3*day), "sdist")
	publish(t, f.st, repo.ID, "requests", "1.0", testNow, "py3-none-any") // a wheel added later
	publish(t, f.st, repo.ID, "requests", "2.0", testNow.Add(-2*day))
	pkg := publish(t, f.st, repo.ID, "requests", "1.5", testNow.Add(-2*day)) // as old as 2.0
	must(t, f.st.ReplaceJournalState(context.Background(), pkg.ID, []index.JournalState{
		{Kind: index.JournalDistTag, Key: "beta", Value: "2.0", Seq: 2},
		{Kind: index.JournalDistTag, Key: "latest", Value: "2.0", Seq: 1},
		{Kind: index.JournalDeprecated, Key: "1.0", Value: "use 2.0", Seq: 3},
	}))
	dg := func(s string) string { return digest.FromString("requests" + s).String() }
	var all VersionList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/requests/versions", "", admin), http.StatusOK, &all)
	wantJSON(t, "versions", all, `{"versions": [
		{"version": "2.0", "publishedAt": "2025-12-31T12:00:00Z", "lastModified": "2025-12-31T12:00:00Z",
		 "variants": [{"variant": "", "tag": "2.0__", "digest": "`+dg("2.0")+`", "publishedAt": "2025-12-31T12:00:00Z"}],
		 "pointers": [{"kind": "dist-tag", "name": "beta"}, {"kind": "dist-tag", "name": "latest"}]},
		{"version": "1.5", "publishedAt": "2025-12-31T12:00:00Z", "lastModified": "2025-12-31T12:00:00Z",
		 "variants": [{"variant": "", "tag": "1.5__", "digest": "`+dg("1.5")+`", "publishedAt": "2025-12-31T12:00:00Z"}],
		 "pointers": []},
		{"version": "1.0", "publishedAt": "2025-12-30T12:00:00Z", "lastModified": "2026-01-02T12:00:00Z",
		 "variants": [
		   {"variant": "py3-none-any", "tag": "1.0__py3-none-any", "digest": "`+dg("1.0py3-none-any")+`", "publishedAt": "2026-01-02T12:00:00Z"},
		   {"variant": "sdist", "tag": "1.0__sdist", "digest": "`+dg("1.0sdist")+`", "publishedAt": "2025-12-30T12:00:00Z"}],
		 "pointers": []}
	]}`)
}

// TestListVersionsPages: the cursor holds the place of a version, so a version published
// between two pages, newer than all of them, is on none of the later pages and shifts nothing.
func TestListVersionsPages(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	for i, v := range []string{"1.0", "1.1", "1.2", "1.3", "1.4"} {
		publish(t, f.st, repo.ID, "requests", v, testNow.Add(time.Duration(i)*time.Hour))
	}
	target := "/api/v1/repos/pypi/libs/packages/requests/versions?limit=2"
	var got []string
	for cur, n := "", 0; n == 0 || cur != ""; n++ {
		var l VersionList
		q := target
		if cur != "" {
			q += "&cursor=" + cur
		}
		decodeJSON(t, f.do(http.MethodGet, q, "", admin), http.StatusOK, &l)
		for _, v := range l.Versions {
			got = append(got, v.Version)
		}
		if n == 0 {
			publish(t, f.st, repo.ID, "requests", "2.0", testNow.Add(24*time.Hour))
		}
		cur = l.Next
	}
	if !slices.Equal(got, []string{"1.4", "1.3", "1.2", "1.1", "1.0"}) {
		t.Errorf("the pages: %v", got)
	}
}

// TestListEveryLimit: with every limit, the pages of both lists are the list of one page, in
// byte order, with versions as new as each other and a variant published after its version.
func TestListEveryLimit(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	for _, v := range []string{"1.10", "1.9", "Z", "a", "é"} {
		publish(t, f.st, repo.ID, v, v, testNow)
	}
	publish(t, f.st, repo.ID, "a", "0.1", testNow.Add(-time.Hour), "sdist")
	publish(t, f.st, repo.ID, "a", "0.1", testNow.Add(time.Hour), "wheel")
	publish(t, f.st, repo.ID, "a", "0.2", testNow.Add(-time.Minute))
	base := "/api/v1/repos/pypi/libs/packages"
	for _, target := range []string{base, base + "/a/versions"} {
		pages := func(limit int) (all []any) {
			for cur, n := "", 0; n == 0 || cur != ""; n++ {
				var l struct {
					Packages []any  `json:"packages"`
					Versions []any  `json:"versions"`
					Next     string `json:"next"`
				}
				decodeJSON(t, f.do(http.MethodGet, fmt.Sprintf("%s?limit=%d&cursor=%s", target, limit, cur), "", admin), http.StatusOK, &l)
				all, cur = append(append(all, l.Packages...), l.Versions...), l.Next
			}
			return all
		}
		whole := pages(maxLimit)
		for limit := 1; limit <= len(whole); limit++ {
			if got := pages(limit); !reflect.DeepEqual(got, whole) {
				t.Errorf("%s, pages of %d: %v, want %v", target, limit, got, whole)
			}
		}
	}
}

// TestListErrors: a repo or a package that does not exist is 404 naming it, a cursor that is
// not one of the list's is 400, and both lists need read.
func TestListErrors(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	publish(t, f.st, repo.ID, "requests", "1.0", testNow)
	for _, tt := range []struct {
		target, want string
		status       int
	}{
		{"/api/v1/repos/pypi/none/packages", "repo pypi/none does not exist", http.StatusNotFound},
		{"/api/v1/repos/pypi/libs/packages/flask/versions", `pypi/libs has no package "flask"`, http.StatusNotFound},
		{"/api/v1/repos/pypi/libs/packages/requests/versions?cursor=bm9wZQ", "cursor:", http.StatusBadRequest},
	} {
		wantError(t, f.do(http.MethodGet, tt.target, "", admin), tt.status, codeOf(tt.status), tt.want)
	}
	f = newFixture(t, Options{Auth: fakeAuth{grants: map[string]bool{"user:admin read pypi/libs": true}}})
	repo = f.create(t, "pypi", "libs")
	publish(t, f.st, repo.ID, "requests", "1.0", testNow)
	for _, target := range []string{"/api/v1/repos/pypi/libs/packages", "/api/v1/repos/pypi/libs/packages/requests/versions"} {
		wantError(t, f.do(http.MethodGet, target, "", bob), http.StatusNotFound, codeNotFound)
		wantError(t, f.do(http.MethodGet, target, "", auth.Principal{}), http.StatusUnauthorized, codeUnauthenticated)
		wantStatus(t, f.do(http.MethodGet, target, "", admin), http.StatusOK)
	}
}
