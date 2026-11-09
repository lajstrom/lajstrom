package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// recordingDeleter records the deletions it gets and answers each with err.
type recordingDeleter struct {
	got []format.Deletion
	err error
}

func (d *recordingDeleter) Delete(_ context.Context, _ index.Repo, del format.Deletion, _ auth.Principal) (*format.Deleted, error) {
	d.got = append(d.got, del)
	return nil, d.err
}

// addVariants adds the package name, escaped as nameOCI, with version 1.0 in the variants and
// 2.0 without one, and returns the digests of 1.0 by variant.
func addVariants(t *testing.T, st index.Store, repoID int64, name, nameOCI string, variants ...string) map[string]digest.Digest {
	t.Helper()
	ctx := context.Background()
	pkg := index.Package{RepoID: repoID, Name: name, NameOCI: nameOCI}
	must(t, st.UpsertPackage(ctx, &pkg))
	digests := map[string]digest.Digest{}
	for i, variant := range append(variants, "") {
		version := "1.0"
		if i == len(variants) {
			version = "2.0"
		}
		v := index.Version{PackageID: pkg.ID, Version: version, Variant: variant, Tag: fmt.Sprintf("%s-%d", version, i),
			Digest: digest.FromString(name + version + variant), ArtifactType: "application/vnd.lajstrom.pypi.v1"}
		must(t, st.UpsertVersion(ctx, &v))
		if version == "1.0" {
			digests[variant] = v.Digest
		}
	}
	return digests
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestDeleteVersion: every variant of a version goes to the Deleter by its digest in the
// package's OCI path, with the client's address, and the answer is 204; ?variant= keeps one.
// A raw path is one segment, its / escaped.
func TestDeleteVersion(t *testing.T) {
	d := &recordingDeleter{}
	f := newFixture(t, Options{Deleter: d})
	repo := f.create(t, "pypi", "libs")
	digests := addVariants(t, f.st, repo.ID, "requests", "requests", "py3-none-any", "sdist")
	raw := f.create(t, "raw", "files")
	rawDigest := addVariants(t, f.st, raw.ID, "docs/a b.txt", "docs/a%20b.txt", "")[""]

	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/pypi/libs/packages/requests/versions/1.0", "", admin), http.StatusNoContent)
	wantDeletions(t, d, "requests", digests["py3-none-any"], digests["sdist"])
	d.got = nil
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/pypi/libs/packages/requests/versions/1.0?variant=sdist", "", admin), http.StatusNoContent)
	wantDeletions(t, d, "requests", digests["sdist"])
	d.got = nil
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/raw/files/packages/docs%2Fa%20b.txt/versions/1.0", "", admin), http.StatusNoContent)
	wantDeletions(t, d, "docs/a%20b.txt", rawDigest)
	if d.got[0].ClientIP != "192.0.2.1" {
		t.Errorf("ClientIP = %q, want httptest's 192.0.2.1", d.got[0].ClientIP)
	}
}

// wantDeletions fails the test unless d got exactly the digests in the OCI path path.
func wantDeletions(t *testing.T, d *recordingDeleter, path string, digests ...digest.Digest) {
	t.Helper()
	var got, want []string
	for _, del := range d.got {
		got = append(got, del.Path+"@"+del.Reference)
	}
	for _, dg := range digests {
		want = append(want, path+"@"+dg.String())
	}
	if !slices.Equal(got, want) {
		t.Errorf("deletions %q, want %q", got, want)
	}
}

// TestDeleteVersionErrors: what does not exist is 404 naming it, and nothing is asked of the
// Deleter; the Deleter's errors keep their status with the API's message; without a Deleter
// the endpoint is 501.
func TestDeleteVersionErrors(t *testing.T) {
	d := &recordingDeleter{}
	f := newFixture(t, Options{Deleter: d})
	repo := f.create(t, "pypi", "libs")
	addVariants(t, f.st, repo.ID, "requests", "requests", "sdist")
	base := "/api/v1/repos/pypi/libs/packages/"
	for _, tt := range []struct{ target, want string }{
		{"/api/v1/repos/pypi/none/packages/requests/versions/1.0", "repo pypi/none does not exist"},
		{base + "flask/versions/1.0", `no package "flask"`},
		{base + "requests/versions/9.9", `no version "9.9"`},
		{base + "requests/versions/1.0?variant=py3-none-any", `with the variant "py3-none-any"`},
	} {
		wantError(t, f.do(http.MethodDelete, tt.target, "", admin), http.StatusNotFound, codeNotFound, tt.want)
	}
	if len(d.got) != 0 {
		t.Errorf("deletions %+v", d.got)
	}
	for _, tt := range []struct {
		err    error
		status int
		code   string
		want   string
	}{
		{fmt.Errorf("ingest: %w", provider.ErrUnsupported), http.StatusNotImplemented, codeUnavailable, `the registry "local" of pypi/libs does not delete manifests`},
		{fmt.Errorf("ingest: %w", provider.ErrConflict), http.StatusConflict, codeConflict, "refused to delete version 1.0"},
		{fmt.Errorf("ingest: %w", index.ErrNotFound), http.StatusNotFound, codeNotFound, "deleted meanwhile"},
	} {
		d.err = tt.err
		wantError(t, f.do(http.MethodDelete, base+"requests/versions/1.0", "", admin), tt.status, tt.code, tt.want)
	}
	wantError(t, newFixture(t, Options{}).do(http.MethodDelete, base+"requests/versions/1.0", "", admin),
		http.StatusNotImplemented, codeUnavailable, "not enabled")
}

// TestDeleteVersionAccess: delete is the action, decided before the repo is looked up: 403 to
// a reader, 404 to a principal that cannot read the repo, as for a repo that does not exist,
// 401 to anonymous.
func TestDeleteVersionAccess(t *testing.T) {
	d := &recordingDeleter{}
	f := newFixture(t, Options{Deleter: d, Auth: fakeAuth{grants: map[string]bool{
		"user:admin delete pypi/libs": true, "user:admin read pypi/libs": true, "user:alice read pypi/libs": true}}})
	repo := f.create(t, "pypi", "libs")
	addVariants(t, f.st, repo.ID, "requests", "requests", "sdist")
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0"
	wantError(t, f.do(http.MethodDelete, target, "", alice), http.StatusForbidden, codeForbidden)
	wantError(t, f.do(http.MethodDelete, target, "", bob), http.StatusNotFound, codeNotFound)
	wantError(t, f.do(http.MethodDelete, "/api/v1/repos/pypi/none/packages/requests/versions/1.0", "", bob), http.StatusNotFound, codeNotFound)
	wantError(t, f.do(http.MethodDelete, target, "", auth.Principal{}), http.StatusUnauthorized, codeForbidden)
	if len(d.got) != 0 {
		t.Errorf("deletions %+v", d.got)
	}
	wantStatus(t, f.do(http.MethodDelete, target, "", admin), http.StatusNoContent)
}
