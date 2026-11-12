package facade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// listing follows a listing from target through its Link headers and returns every page.
func (e *env) listing(h http.Handler, target string, field string, hdr ...string) [][]string {
	e.t.Helper()
	var pages [][]string
	for target != "" && len(pages) < 20 {
		rr := e.do(h, http.MethodGet, target, nil, hdr...)
		wantStatus(e.t, "GET "+target, rr, http.StatusOK)
		var body map[string]json.RawMessage
		must(e.t, json.Unmarshal(rr.Body.Bytes(), &body))
		var page []string
		must(e.t, json.Unmarshal(body[field], &page))
		if page == nil {
			e.t.Errorf("GET %s: %s is null, want a list", target, field)
		}
		pages = append(pages, page)
		target = nextLink(e.t, rr)
	}
	return pages
}

// nextLink returns the target of the rel="next" Link, or "".
func nextLink(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	link := rr.Header().Get("Link")
	if link == "" {
		return ""
	}
	var target string
	if _, err := fmt.Sscanf(link, "<%s", &target); err != nil || len(target) < 2 || !bytes.HasSuffix([]byte(link), []byte(`>; rel="next"`)) {
		t.Fatalf("Link %q", link)
	}
	return target[:len(target)-2] // drop ">;"
}

// seedPackage writes the package name of the raw repo with a version per tag.
func (e *env) seedPackage(name string, tags ...string) {
	e.t.Helper()
	ctx := e.t.Context()
	pkg := &index.Package{RepoID: e.raw.ID, Name: name, NameOCI: name}
	must(e.t, e.st.UpsertPackage(ctx, pkg))
	for _, tag := range tags {
		must(e.t, e.st.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: tag, Tag: tag, Digest: digest.FromString(tag),
			ArtifactType: "application/vnd.lajstrom.raw.v1"}))
	}
	must(e.t, e.st.UpsertOCIRepo(ctx, &index.OCIRepo{RepoID: e.raw.ID, Path: "raw/assets/" + name, SeenAt: e.clock.Now()}))
}

func TestTagsFromTheIndex(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.seedPackage("hello", "c3", "a1", "b2", "e5", "d4")
	list := "/v2/raw/assets/hello/tags/list"
	rr := e.do(e.h, http.MethodGet, list, nil)
	var got tagList
	must(t, json.Unmarshal(rr.Body.Bytes(), &got))
	if got.Name != "raw/assets/hello" || !slices.Equal(got.Tags, []string{"a1", "b2", "c3", "d4", "e5"}) {
		t.Errorf("tags/list = %+v", got)
	}
	pages := e.listing(e.h, list+"?n=2", "tags")
	if want := [][]string{{"a1", "b2"}, {"c3", "d4"}, {"e5"}}; !equalPages(pages, want) {
		t.Errorf("pages %q, want %q", pages, want)
	}
	if pages := e.listing(e.h, list+"?n=2&last=b2", "tags"); !equalPages(pages, [][]string{{"c3", "d4"}, {"e5"}}) {
		t.Errorf("pages after b2: %q", pages)
	}
	if pages := e.listing(e.h, list+"?last=c3", "tags"); !equalPages(pages, [][]string{{"d4", "e5"}}) {
		t.Errorf("after c3 without n: %q", pages)
	}
	rr = e.do(e.h, http.MethodGet, list+"?n=0", nil)
	wantHeader(t, "n=0", rr, "Link", "")
	if pages := e.listing(e.h, list+"?n=0", "tags"); !equalPages(pages, [][]string{{}}) {
		t.Errorf("n=0: %q", pages)
	}
	if pages := e.listing(e.h, list+"?n=9", "tags"); !equalPages(pages, [][]string{{"a1", "b2", "c3", "d4", "e5"}}) {
		t.Errorf("n=9: %q", pages)
	}

	// A package the index does not have: an empty list when oci_repos knows the repository.
	wantError(t, "unknown package", e.do(e.h, http.MethodGet, "/v2/raw/assets/nothing/tags/list", nil), http.StatusNotFound, codeNameUnknown)
	must(t, e.st.UpsertOCIRepo(t.Context(), &index.OCIRepo{RepoID: e.raw.ID, Path: "raw/assets/foreign", SeenAt: e.clock.Now()}))
	if pages := e.listing(e.h, "/v2/raw/assets/foreign/tags/list", "tags"); !equalPages(pages, [][]string{{}}) {
		t.Errorf("foreign repository: %q", pages)
	}
}

func TestTagsFromTheBackend(t *testing.T) {
	e := newEnv(t, envOptions{})
	for i, tag := range []string{"v3", "v1", "latest", "v2"} {
		body, _ := manifest(t, []byte("layer"), fmt.Sprint(i))
		rr := e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/"+tag, bytes.NewReader(body), "Content-Type", ocispecManifest)
		wantStatus(t, "PUT "+tag, rr, http.StatusCreated)
	}
	list := "/v2/oci/images/app/tags/list"
	if pages := e.listing(e.h, list, "tags"); !equalPages(pages, [][]string{{"latest", "v1", "v2", "v3"}}) {
		t.Errorf("tags %q", pages)
	}
	if pages := e.listing(e.h, list+"?n=3", "tags"); !equalPages(pages, [][]string{{"latest", "v1", "v2"}, {"v3"}}) {
		t.Errorf("pages of 3: %q", pages)
	}
	if pages := e.listing(e.h, list+"?n=1&last=v1", "tags"); !equalPages(pages, [][]string{{"v2"}, {"v3"}}) {
		t.Errorf("pages after v1: %q", pages)
	}
	if pages := e.listing(e.h, list+"?n=0", "tags"); !equalPages(pages, [][]string{{}}) {
		t.Errorf("n=0: %q", pages)
	}
	wantError(t, "a repository the backend does not have", e.do(e.h, http.MethodGet, "/v2/oci/images/none/tags/list", nil),
		http.StatusNotFound, codeNameUnknown)
}

func TestCatalog(t *testing.T) {
	z := authorizer{"dev": {"oci/images": {auth.ActionRead, auth.ActionWrite}}}
	e := newEnv(t, envOptions{authz: z})
	e.seedPackage("hello", "1")
	e.seedPackage("docs/readme.md", "1")
	// Repositories of the raw repo that oci_repos records and that hold no version: one whose
	// versions were all deleted, which the registry keeps (Zot), sorting between the others, and
	// a foreign one. Neither is listed, nor counted in a page (#728).
	for _, path := range []string{"raw/assets/docs/gone", "raw/assets/foreign"} {
		must(t, e.st.UpsertOCIRepo(t.Context(), &index.OCIRepo{RepoID: e.raw.ID, Path: path, SeenAt: e.clock.Now()}))
	}
	for _, name := range []string{"app", "base/tools"} {
		body, _ := manifest(t, []byte(name), name)
		rr := e.do(e.h, http.MethodPut, "/v2/oci/images/"+name+"/manifests/1", bytes.NewReader(body),
			"Content-Type", ocispecManifest, principalHeader, "dev")
		wantStatus(t, "PUT "+name, rr, http.StatusCreated)
	}
	all := []string{"oci/images/app", "oci/images/base/tools", "raw/assets/docs/readme.md", "raw/assets/hello"}

	open := New(Options{Index: e.st, Providers: provider.Map{"local": e.prov}, Tokens: e.tokens}) // no Authorizer
	bearer := "Bearer " + e.getToken(open, "", "registry:catalog:*")
	if pages := e.listing(open, "/v2/_catalog", "repositories", "Authorization", bearer); !equalPages(pages, [][]string{all}) {
		t.Errorf("catalog without an Authorizer: %q", pages)
	}
	if pages := e.listing(open, "/v2/_catalog?n=3", "repositories", "Authorization", bearer); !equalPages(pages, [][]string{all[:3], all[3:]}) {
		t.Errorf("catalog pages: %q", pages)
	}
	if pages := e.listing(open, "/v2/_catalog?n=1&last="+all[1], "repositories", "Authorization", bearer); !equalPages(pages, [][]string{{all[2]}, {all[3]}}) {
		t.Errorf("catalog after %s: %q", all[1], pages)
	}
	if pages := e.listing(e.h, "/v2/_catalog", "repositories", principalHeader, "dev"); !equalPages(pages, [][]string{all[:2]}) {
		t.Errorf("catalog of dev: %q", pages)
	}
}

// TestCatalogChallengesAnonymous checks that GET /v2/_catalog without credentials is 401 with the
// challenge of registry:catalog:*, as GET /v2/ is: crane catalog and oras repo ls authenticate
// only when challenged, so a 200 would list what anonymous may read to a client that has logged
// in. The client then lists with a token: of its user, or of anonymous, which reads what
// anonymous read allows.
func TestCatalogChallengesAnonymous(t *testing.T) {
	z := authorizer{"dev": {"oci/images": {auth.ActionRead, auth.ActionWrite}}, "": {"raw/assets": {auth.ActionRead}}}
	e := newEnv(t, envOptions{authz: z})
	e.seedPackage("hello", "1")
	body, _ := manifest(t, []byte("app"), "app")
	wantStatus(t, "PUT app", e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/1", bytes.NewReader(body),
		"Content-Type", ocispecManifest, principalHeader, "dev"), http.StatusCreated)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rr := e.do(e.h, method, "/v2/_catalog", nil)
		wantStatus(t, method+" /v2/_catalog as anonymous", rr, http.StatusUnauthorized)
		wantHeader(t, method+" /v2/_catalog as anonymous", rr, "WWW-Authenticate",
			`Bearer realm="http://example.com/v2/token",service="example.com",scope="registry:catalog:*"`)
	}
	wantError(t, "GET /v2/_catalog as anonymous", e.do(e.h, http.MethodGet, "/v2/_catalog", nil), http.StatusUnauthorized, codeUnauthorized)

	for _, tt := range []struct {
		user string
		want []string
	}{
		{"dev", []string{"oci/images/app"}},
		{"", []string{"raw/assets/hello"}}, // the anonymous token: what anonymous may read
	} {
		tok := e.getToken(e.h, tt.user, "registry:catalog:*")
		if pages := e.listing(e.h, "/v2/_catalog", "repositories", "Authorization", "Bearer "+tok); !equalPages(pages, [][]string{tt.want}) {
			t.Errorf("catalog with the token of %q: %q, want %q", tt.user, pages, tt.want)
		}
	}
	if pages := e.listing(e.h, "/v2/_catalog", "repositories", principalHeader, "dev"); !equalPages(pages, [][]string{{"oci/images/app"}}) {
		t.Errorf("catalog of dev by Basic: %q", pages)
	}
}

// equalPages reports whether two listings are equal.
func equalPages(a, b [][]string) bool {
	return slices.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}
