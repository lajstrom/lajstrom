package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
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

// noAttachments are the counts of a variant without attachments.
const noAttachments = `{"signature": 0, "sbom": 0, "scan": 0, "provenance": 0, "attestation": 0, "other": 0}`

// TestVersionFacts: the list of versions and the version detail count each variant's files,
// add up their sizes and count its attachments by kind, the degraded sdist's too; the rows an
// attach writes (ingest.PutReferrer) add to the counts, and those the delete of a referrer
// removes take from them, at the next request.
func TestVersionFacts(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	ctx := context.Background()
	pkg, err := f.st.GetPackage(ctx, repo.ID, "requests")
	must(t, err)
	wheel, err := f.st.GetVersion(ctx, pkg.ID, "1.0", "py3-none-any")
	must(t, err)
	sdist, err := f.st.GetVersion(ctx, pkg.ID, "1.0", "sdist")
	must(t, err)
	attach := func(v *index.Version, referrer string, kinds ...string) {
		t.Helper()
		rows := make([]index.Attachment, len(kinds))
		for i, kind := range kinds {
			rows[i] = index.Attachment{Layer: digest.FromString(referrer + kind + string(rune('a'+i))), MediaType: "application/json",
				Kind: kind, Verification: index.AttachmentUnsigned, IndexedAt: testNow}
		}
		must(t, f.st.ReplaceAttachments(ctx, v.ID, digest.FromString(referrer), rows))
	}
	type facts struct {
		Size, FileCount int64
		Degraded        bool
		Attachments     map[string]int64
	}
	counts := func(some map[string]int64) map[string]int64 {
		m := map[string]int64{"signature": 0, "sbom": 0, "scan": 0, "provenance": 0, "attestation": 0, "other": 0}
		maps.Copy(m, some)
		return m
	}
	check := func(what string, want map[string]facts) {
		t.Helper()
		var list VersionList
		decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/requests/versions", "", admin), http.StatusOK, &list)
		var detail VersionDetail
		decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/requests/versions/1.0", "", admin), http.StatusOK, &detail)
		got := map[string]facts{}
		for _, e := range list.Versions[0].Variants {
			got[e.Variant] = facts{e.Size, e.FileCount, e.Degraded, e.Attachments}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s, the list: %+v, want %+v", what, got, want)
		}
		got = map[string]facts{}
		for _, d := range detail.Variants {
			got[d.Variant] = facts{d.Size, d.FileCount, d.Degraded, d.Attachments}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s, the detail: %+v, want %+v", what, got, want)
		}
	}
	check("before any attach", map[string]facts{"py3-none-any": {65138, 2, false, counts(nil)}, "sdist": {131072, 1, true, counts(nil)}})

	attach(wheel, "sig", index.AttachmentSignature)
	attach(wheel, "bom", index.AttachmentSBOM, index.AttachmentSBOM)
	attach(sdist, "slsa", index.AttachmentProvenance)
	attach(sdist, "vex", "vex") // a kind of a newer binary
	check("after the attaches", map[string]facts{
		"py3-none-any": {65138, 2, false, counts(map[string]int64{"signature": 1, "sbom": 2})},
		"sdist":        {131072, 1, true, counts(map[string]int64{"provenance": 1, "vex": 1})},
	})

	_, err = f.st.DeleteAttachments(ctx, repo.ID, digest.FromString("bom"))
	must(t, err)
	check("after the SBOM's delete", map[string]facts{
		"py3-none-any": {65138, 2, false, counts(map[string]int64{"signature": 1})},
		"sdist":        {131072, 1, true, counts(map[string]int64{"provenance": 1, "vex": 1})},
	})
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
		"artifactType": "application/vnd.lajstrom.pypi.v1", "attrs": {"lajstrom.pypi.nonstandard": "true"}, "degraded": false,
		"size": 65138, "fileCount": 2, "attachments": ` + noAttachments + `,
		"metadata": {"name": "Requests", "summary": "HTTP"}, "indexedAt": "2026-01-02T12:00:01Z",
		"files": [
		  {"name": "requests-1.0-py3-none-any.whl", "mediaType": "application/vnd.lajstrom.pypi.wheel.v1+zip", "digest": "` + dg("whl") + `",
		   "size": 64928, "attrs": {"org.opencontainers.image.title": "requests-1.0-py3-none-any.whl"}},
		  {"name": "requests-1.0-py3-none-any.whl.metadata", "mediaType": "application/vnd.lajstrom.pypi.core-metadata.v1", "digest": "` + dg("md") + `",
		   "size": 210, "attrs": {}}], "ociRepository": "pypi/libs/requests"}`
	sdist := `{"variant": "sdist", "tag": "1.0__sdist", "digest": "` + dg("sdist") + `", "publishedAt": "2025-12-30T12:00:00Z",
		"artifactType": "application/vnd.lajstrom.pypi.v1", "attrs": {}, "degraded": true, "size": 131072, "fileCount": 1,
		"attachments": ` + noAttachments + `, "metadata": null, "indexedAt": "2026-01-02T12:00:00Z",
		"files": [{"name": "requests-1.0.tar.gz", "mediaType": "application/vnd.lajstrom.pypi.sdist.v1+gzip", "digest": "` + dg("tgz") + `",
		   "size": 131072, "attrs": {}}], "ociRepository": "pypi/libs/requests"}`
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

// TestGetVersionSplit: the variants of a version split across two forms of its package path
// (#642) each name the OCI repository that holds their manifest, index.ManifestPath, and the
// version names that of its first variant: with ?variant=, of the one it keeps.
func TestGetVersionSplit(t *testing.T) {
	f := newFixture(t, Options{})
	repo := f.create(t, "pypi", "libs")
	ctx := context.Background()
	pkg := index.Package{RepoID: repo.ID, Name: "manifests", NameOCI: "manifest_73"} // ADR 0017; "manifests" before it
	must(t, f.st.UpsertPackage(ctx, &pkg))
	for _, v := range []index.Version{
		{Variant: "py3-none-any", Tag: "1.0__py3-none-any"},
		{Variant: "sdist", Tag: "1.0__sdist", OCIRepo: "manifests"}, // found by reindex at the earlier form
	} {
		v.PackageID, v.Version, v.Digest, v.ArtifactType = pkg.ID, "1.0", digest.FromString(v.Variant), "application/vnd.lajstrom.pypi.v1"
		v.CreatedAt, v.IndexedAt = testNow, testNow
		must(t, f.st.UpsertVersion(ctx, &v))
	}
	target := "/api/v1/repos/pypi/libs/packages/manifests/versions/1.0"
	for _, tt := range []struct {
		query, version string
		variants       []string
	}{
		{"", "pypi/libs/manifest_73", []string{"pypi/libs/manifest_73", "pypi/libs/manifests"}},
		{"?variant=sdist", "pypi/libs/manifests", []string{"pypi/libs/manifests"}},
		{"?variant=py3-none-any", "pypi/libs/manifest_73", []string{"pypi/libs/manifest_73"}},
	} {
		var got VersionDetail
		decodeJSON(t, f.do(http.MethodGet, target+tt.query, "", admin), http.StatusOK, &got)
		variants := make([]string, len(got.Variants))
		for i, v := range got.Variants {
			variants[i] = v.OCIRepository
		}
		if got.OCIRepository != tt.version || !slices.Equal(variants, tt.variants) {
			t.Errorf("version%s: ociRepository %q, of the variants %q; want %q, %q", tt.query, got.OCIRepository, variants, tt.version, tt.variants)
		}
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
