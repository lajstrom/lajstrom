package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// publishDetailed adds the variants of requests 1.0 to a repo, as the version detail reads
// them: a wheel with attrs, metadata and two files, and an sdist written degraded, without
// metadata, attrs or file attrs, published three days before. A dist-tag names the version.
func publishDetailed(t *testing.T, st index.Store, repoID int64) {
	t.Helper()
	ctx := context.Background()
	pkg := index.Package{RepoID: repoID, Name: "requests", NameOCI: "requests"}
	must(t, st.UpsertPackage(ctx, &pkg))
	wheel := index.Version{PackageID: pkg.ID, Version: "1.0", Variant: "py3-none-any", Tag: "1.0__py3-none-any",
		Digest: digest.FromString("wheel"), ArtifactType: "application/vnd.lajstrom.pypi.v1",
		Attrs:    map[string]string{"lajstrom.pypi.nonstandard": "true"},
		Metadata: json.RawMessage(`{"name": "Requests", "summary": "HTTP"}`), CreatedAt: testNow, IndexedAt: testNow.Add(time.Second)}
	sdist := index.Version{PackageID: pkg.ID, Version: "1.0", Variant: "sdist", Tag: "1.0__sdist",
		Digest: digest.FromString("sdist"), ArtifactType: "application/vnd.lajstrom.pypi.v1",
		CreatedAt: testNow.Add(-72 * time.Hour), IndexedAt: testNow, Degraded: true}
	for _, v := range []*index.Version{&wheel, &sdist} {
		must(t, st.UpsertVersion(ctx, v))
	}
	must(t, st.ReplaceFiles(ctx, wheel.ID, []index.File{
		{Name: "requests-1.0-py3-none-any.whl", MediaType: "application/vnd.lajstrom.pypi.wheel.v1+zip", Digest: digest.FromString("whl"),
			Size: 64928, Attrs: map[string]string{"org.opencontainers.image.title": "requests-1.0-py3-none-any.whl"}},
		{Name: "requests-1.0-py3-none-any.whl.metadata", MediaType: "application/vnd.lajstrom.pypi.core-metadata.v1", Digest: digest.FromString("md"), Size: 210},
	}))
	must(t, st.ReplaceFiles(ctx, sdist.ID, []index.File{
		{Name: "requests-1.0.tar.gz", MediaType: "application/vnd.lajstrom.pypi.sdist.v1+gzip", Digest: digest.FromString("tgz"), Size: 131072},
	}))
	must(t, st.ReplaceJournalState(ctx, pkg.ID, []index.JournalState{{Kind: index.JournalDistTag, Key: "latest", Value: "1.0", Seq: 1}}))
}

// TestGetVersion: one version with every variant by variant, each with its row of the index and
// its files in order, and the OCI repository; ?variant= keeps one variant and leaves the
// version's times those of all its variants.
func TestGetVersion(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	dg := func(s string) string { return digest.FromString(s).String() }
	wheel := `{"variant": "py3-none-any", "tag": "1.0__py3-none-any", "digest": "` + dg("wheel") + `", "publishedAt": "2026-01-02T12:00:00Z",
		"artifactType": "application/vnd.lajstrom.pypi.v1", "attrs": {"lajstrom.pypi.nonstandard": "true"},
		"metadata": {"name": "Requests", "summary": "HTTP"}, "degraded": false, "indexedAt": "2026-01-02T12:00:01Z",
		"files": [
		  {"name": "requests-1.0-py3-none-any.whl", "mediaType": "application/vnd.lajstrom.pypi.wheel.v1+zip", "digest": "` + dg("whl") + `",
		   "size": 64928, "attrs": {"org.opencontainers.image.title": "requests-1.0-py3-none-any.whl"}},
		  {"name": "requests-1.0-py3-none-any.whl.metadata", "mediaType": "application/vnd.lajstrom.pypi.core-metadata.v1", "digest": "` + dg("md") + `",
		   "size": 210, "attrs": {}}]}`
	sdist := `{"variant": "sdist", "tag": "1.0__sdist", "digest": "` + dg("sdist") + `", "publishedAt": "2025-12-30T12:00:00Z",
		"artifactType": "application/vnd.lajstrom.pypi.v1", "attrs": {}, "metadata": null, "degraded": true, "indexedAt": "2026-01-02T12:00:00Z",
		"files": [{"name": "requests-1.0.tar.gz", "mediaType": "application/vnd.lajstrom.pypi.sdist.v1+gzip", "digest": "` + dg("tgz") + `",
		   "size": 131072, "attrs": {}}]}`
	version := func(variants string) string {
		return `{"version": "1.0", "publishedAt": "2025-12-30T12:00:00Z", "lastModified": "2026-01-02T12:00:00Z",
			"variants": [` + variants + `], "pointers": [{"kind": "dist-tag", "name": "latest"}], "ociRepository": "pypi/libs/requests"}`
	}
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0"
	for _, tt := range []struct{ query, want string }{
		{"", version(wheel + "," + sdist)},
		{"?variant=sdist", version(sdist)},
		{"?variant=py3-none-any", version(wheel)},
	} {
		var got any
		decodeJSON(t, f.do(http.MethodGet, target+tt.query, "", admin), http.StatusOK, &got)
		wantJSON(t, "version"+tt.query, got, tt.want)
	}
}

// TestGetVersionNotFound: a repo, package, version or variant that does not exist is 404 naming
// it, and so is a version of a group, though a member has it.
func TestGetVersionNotFound(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	for _, tt := range []struct{ target, want string }{
		{"/api/v1/repos/pypi/none/packages/requests/versions/1.0", "repo pypi/none does not exist"},
		{"/api/v1/repos/pypi/libs/packages/flask/versions/1.0", `pypi/libs has no package "flask"`},
		{"/api/v1/repos/pypi/libs/packages/requests/versions/2.0", `package "requests" of pypi/libs has no version "2.0"`},
		{"/api/v1/repos/pypi/libs/packages/requests/versions/1.0?variant=", `has no version "1.0" with the variant ""`},
		{"/api/v1/repos/pypi/libs/packages/requests/versions/1.0?variant=cp311", `with the variant "cp311"`},
	} {
		wantError(t, f.do(http.MethodGet, tt.target, "", admin), http.StatusNotFound, codeNotFound, tt.want)
	}

	g := newGroupFixture(t)
	wantStatus(t, g.do(http.MethodPost, "/api/v1/repos",
		`{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["internal", "npmjs"]}}`, admin), http.StatusCreated)
	internal, err := g.st.GetRepo(context.Background(), "npm", "internal")
	must(t, err)
	publish(t, g.st, internal.ID, "left-pad", "1.0", testNow)
	wantStatus(t, g.do(http.MethodGet, "/api/v1/repos/npm/internal/packages/left-pad/versions/1.0", "", admin), http.StatusOK)
	wantError(t, g.do(http.MethodGet, "/api/v1/repos/npm/all/packages/left-pad/versions/1.0", "", admin),
		http.StatusNotFound, codeNotFound, `npm/all has no package "left-pad"`)
}

// TestGetVersionAccess: the version detail needs read, by the rule of docs/spec/rbac.md §3 (401
// for anonymous, 404 for a principal that cannot read the repo); with RBAC, anonymous reads it
// when both auth.anonymousRead and the repo's anonymousRead allow it.
func TestGetVersionAccess(t *testing.T) {
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0"
	f := newFixture(t, Options{Auth: fakeAuth{grants: map[string]bool{"user:alice read pypi/libs": true}}})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	wantStatus(t, f.do(http.MethodGet, target, "", alice), http.StatusOK)
	wantError(t, f.do(http.MethodGet, target, "", bob), http.StatusNotFound, codeNotFound)
	wantError(t, f.do(http.MethodGet, target, "", auth.Anonymous), http.StatusUnauthorized, codeUnauthenticated)
	wantError(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/flask/versions/1.0", "", auth.Anonymous),
		http.StatusUnauthorized, codeUnauthenticated) // before the package is looked up

	on := true
	if _, err := f.repos.Update(context.Background(), "pypi", "libs", UpdateRequest{AnonymousRead: &on}, Origin{}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		server bool
		status int
	}{{true, http.StatusOK}, {false, http.StatusUnauthorized}} {
		h := recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log,
			Auth: auth.NewRBAC(auth.RBACOptions{Index: f.st, AnonymousRead: tt.server, Log: f.repos.Log})}))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != tt.status {
			t.Errorf("anonymous with auth.anonymousRead %v: %d, want %d; body %s", tt.server, rr.Code, tt.status, rr.Body)
		}
	}
}
