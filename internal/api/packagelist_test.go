package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// debRow is a manifest of an apt repo for the tests of the package list: published at, with its
// suite and arch, its summary and one file of size bytes.
type debRow struct {
	name, version, suite, arch, summary string
	at                                  time.Time
	size                                int64
}

// putDeb writes rows to the repo repoID.
func putDeb(t *testing.T, st index.Store, repoID int64, rows ...debRow) {
	t.Helper()
	ctx := context.Background()
	for _, r := range rows {
		pkg := index.Package{RepoID: repoID, Name: r.name, NameOCI: r.name}
		must(t, st.UpsertPackage(ctx, &pkg))
		variant := r.suite + "/main/" + r.arch
		v := index.Version{PackageID: pkg.ID, Version: r.version, Variant: variant, Tag: r.version + "__" + strings.ReplaceAll(variant, "/", "_"),
			Digest: digest.FromString(r.name + r.version + variant), ArtifactType: "application/vnd.lajstrom.deb.v1",
			Attrs:   map[string]string{"lajstrom.deb.suite": r.suite, "lajstrom.deb.component": "main", "lajstrom.deb.arch": r.arch},
			Summary: r.summary, CreatedAt: r.at}
		must(t, st.UpsertVersion(ctx, &v))
		must(t, st.ReplaceFiles(ctx, v.ID, []index.File{{Name: r.name + ".deb", MediaType: "application/vnd.lajstrom.deb.package.v1",
			Digest: digest.FromString(r.name + r.version + variant + "file"), Size: r.size}}))
	}
}

// pagesOf follows the cursors of the package list from target and returns the names of every
// page, the first query's parameters on each.
func pagesOf(t *testing.T, f *fixture, target string) []string {
	t.Helper()
	got := []string{}
	for next := ""; ; {
		u := target
		if next != "" {
			u += "&cursor=" + url.QueryEscape(next)
		}
		var l PackageList
		decodeJSON(t, f.do(http.MethodGet, u, "", admin), http.StatusOK, &l)
		got = append(got, names(l)...)
		if next = l.Next; next == "" || len(got) > 20 {
			return got
		}
	}
}

// TestListPackagesQuery: the package list by each sort, paged one package at a time with the
// cursor of the sort; ?attr, ?q and ?prefix keep packages, and ?facet counts the whole repo.
func TestListPackagesQuery(t *testing.T) {
	f := newFixture(t, Options{})
	f.repos.Formats = oneOf("apt")
	repo := f.create(t, "apt", "ubuntu")
	day := 24 * time.Hour
	putDeb(t, f.st, repo.ID,
		debRow{"acme-agent", "2.5.0", "jammy", "amd64", "Acme monitoring agent", testNow.Add(-1 * day), 400},
		debRow{"acme-agent", "2.5.0", "jammy", "arm64", "Acme monitoring agent", testNow.Add(-1 * day), 380},
		debRow{"acme-agent", "2.4.0", "focal", "amd64", "Acme monitoring agent", testNow.Add(-9 * day), 350},
		debRow{"hello", "1.0", "noble", "amd64", "example package", testNow.Add(-3 * day), 10},
		debRow{"zlib1g", "1.3", "jammy", "amd64", "compression library", testNow.Add(-2 * day), 90},
		debRow{"zlib1g", "1.2", "jammy", "amd64", "compression library", testNow.Add(-5 * day), 80},
	)
	target := "/api/v1/repos/apt/ubuntu/packages?limit=1"
	for _, tt := range []struct {
		query string
		want  []string
	}{
		{"", []string{"acme-agent", "hello", "zlib1g"}},
		{"&order=desc", []string{"zlib1g", "hello", "acme-agent"}},
		{"&sort=updated", []string{"acme-agent", "zlib1g", "hello"}},
		{"&sort=updated&order=asc", []string{"hello", "zlib1g", "acme-agent"}},
		{"&sort=versions", []string{"zlib1g", "acme-agent", "hello"}}, // 2, 2 by name descending, 1
		{"&sort=versions&order=asc", []string{"hello", "acme-agent", "zlib1g"}},
		{"&sort=size", []string{"acme-agent", "zlib1g", "hello"}},
		{"&attr=lajstrom.deb.suite:jammy", []string{"acme-agent", "zlib1g"}},
		{"&attr=lajstrom.deb.suite:jammy&attr=lajstrom.deb.suite:noble&sort=updated", []string{"acme-agent", "zlib1g", "hello"}},
		{"&attr=lajstrom.deb.suite:focal&attr=lajstrom.deb.arch:arm64", []string{}}, // no one manifest has both
		{"&q=ACME", []string{"acme-agent"}},
		{"&q=library", []string{"zlib1g"}}, // the summary
		{"&q=e&prefix=h", []string{"hello"}},
	} {
		if got := pagesOf(t, f, target+tt.query); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.query, got, tt.want)
		}
	}

	// The cursor holds the place of the page's last package: one published meanwhile that
	// comes before it is on no later page.
	var first PackageList
	decodeJSON(t, f.do(http.MethodGet, target+"&sort=updated", "", admin), http.StatusOK, &first)
	putDeb(t, f.st, repo.ID, debRow{"hello", "1.1", "noble", "amd64", "example package", testNow, 12})
	var rest []string
	for next := first.Next; next != ""; {
		var l PackageList
		decodeJSON(t, f.do(http.MethodGet, target+"&sort=updated&cursor="+url.QueryEscape(next), "", admin), http.StatusOK, &l)
		rest, next = append(rest, names(l)...), l.Next
	}
	if !slices.Equal(rest, []string{"zlib1g"}) {
		t.Errorf("the pages after a newer publish: %v, want [zlib1g]", rest)
	}

	var got any
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/apt/ubuntu/packages?attr=lajstrom.deb.arch:arm64&facet=lajstrom.deb.suite&facet=lajstrom.deb.arch&facet=lajstrom.none", "", admin),
		http.StatusOK, &got)
	wantJSON(t, "facets", got, `{"packages": [{"name": "acme-agent", "nameOci": "acme-agent",
		"createdAt": "2025-12-24T12:00:00Z", "updatedAt": "2026-01-01T12:00:00Z", "latestVersion": "2.5.0",
		"latestVariants": ["jammy/main/amd64", "jammy/main/arm64"], "lastPublished": "2.5.0", "versions": 2, "manifests": 3, "size": 1130,
		"summary": "Acme monitoring agent", "attrs": {"lajstrom.deb.arch": "amd64", "lajstrom.deb.component": "main", "lajstrom.deb.suite": "jammy"},
		"pointers": []}],
		"facets": {
		  "lajstrom.deb.suite": [{"value": "focal", "count": 1}, {"value": "jammy", "count": 2}, {"value": "noble", "count": 1}],
		  "lajstrom.deb.arch": [{"value": "amd64", "count": 3}, {"value": "arm64", "count": 1}],
		  "lajstrom.none": []}}`)
}

// TestListPackagesQueryInvalid: a parameter of the package list that breaks its rule is 400
// naming it, and so is a cursor of another sort.
func TestListPackagesQueryInvalid(t *testing.T) {
	f := newFixture(t, Options{})
	f.repos.Formats = oneOf("apt")
	repo := f.create(t, "apt", "ubuntu")
	putDeb(t, f.st, repo.ID, debRow{"a", "1", "jammy", "amd64", "", testNow, 1}, debRow{"b", "1", "jammy", "amd64", "", testNow, 1})
	var byName PackageList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/apt/ubuntu/packages?limit=1", "", admin), http.StatusOK, &byName)
	target := "/api/v1/repos/apt/ubuntu/packages?"
	for _, tt := range []struct{ query, want string }{
		{"sort=stars", `sort: "stars"`},
		{"order=up", `order: "up"`},
		{"attr=deb.suite:jammy", `attr: "deb.suite:jammy"`},
		{"attr=lajstrom.deb.suite", `attr: "lajstrom.deb.suite" is not <key>:<value>`},
		{"attr=lajstrom.deb%20suite:jammy", "attr: "},
		{strings.Repeat("attr=lajstrom.a:b&", 17), "attr: 17 of them, at most 16"},
		{"facet=lajstrom.", `facet: "lajstrom."`},
		{strings.Repeat("facet=lajstrom.a&", 17), "facet: 17 of them"},
		{"sort=size&cursor=" + byName.Next, "cursor: "},
		{"sort=updated&cursor=" + cursor("x|a"), "cursor: "},
	} {
		wantError(t, f.do(http.MethodGet, target+tt.query, "", admin), http.StatusBadRequest, codeInvalid, tt.want)
	}
}

// TestListPackagesAccess: the package list needs read, by the rule of docs/spec/rbac.md §3, with
// its parameters too: 404 for a principal that cannot read the repo, before they are read, and
// 401 for anonymous.
func TestListPackagesAccess(t *testing.T) {
	f := newFixture(t, Options{Auth: fakeAuth{grants: map[string]bool{"user:alice read apt/ubuntu": true}}})
	f.repos.Formats = oneOf("apt")
	repo := f.create(t, "apt", "ubuntu")
	putDeb(t, f.st, repo.ID, debRow{"a", "1", "jammy", "amd64", "", testNow, 1})
	for _, query := range []string{"", "?sort=updated&facet=lajstrom.deb.suite", "?sort=stars"} {
		target := "/api/v1/repos/apt/ubuntu/packages" + query
		wantError(t, f.do(http.MethodGet, target, "", bob), http.StatusNotFound, codeNotFound)
		wantError(t, f.do(http.MethodGet, target, "", auth.Anonymous), http.StatusUnauthorized, codeUnauthenticated)
	}
	wantStatus(t, f.do(http.MethodGet, "/api/v1/repos/apt/ubuntu/packages?sort=updated&facet=lajstrom.deb.suite", "", alice), http.StatusOK)
}
