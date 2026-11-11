package npm

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// install are the routes of a plugin with the external URL, over the pipeline.
func (p *pipeline) install(externalURL string, authz auth.Authorizer) http.Handler {
	deps := format.Deps{Index: p.st, Ingest: p.pipe, Providers: provider.Map{"local": p.fake}, Auth: authz, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{ExternalURL: externalURL}.Routes(format.NewRouter(mux, Name, deps), deps)
	return mux
}

// get sends a GET as alice with the headers kv.
func get(h http.Handler, path string, kv ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), alice))
	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The Accept headers of the clients (ADR 0012): npm 10 and 11 the full packument, pnpm the
// abbreviated one, yarn 4 none.
const (
	acceptNpm  = "application/json"
	acceptPnpm = "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8, */*"
)

// publishWidgets publishes widgets 1.0.0 and @acme/widgets 1.0.0, whose tarballs have the same
// file name, through PUT, and returns the tarballs by name.
func publishWidgets(t *testing.T, h http.Handler) map[string][]byte {
	t.Helper()
	tarballs := map[string][]byte{}
	for _, o := range []testutil.NpmOptions{{}, {Name: "@acme/widgets", Dependencies: map[string]string{"widgets": "^1.0.0"}}} {
		b, info := testutil.NpmTarball(o)
		tarballs[info.Name] = b
		if rr := do(h, http.MethodPut, "/npm/internal/"+info.Name, npmBody(t, b, info, nil), false); rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: %d %s", info.Name, rr.Code, rr.Body)
		}
	}
	return tarballs
}

// TestInstallPackuments reads the packuments of @acme/widgets as the clients do: the full one
// for npm and yarn, the abbreviated one for pnpm, the full one again with ?write=true, at both
// spellings of the scoped name; ETag and 304; one version and a dist-tag.
func TestInstallPackuments(t *testing.T) {
	p := newPipeline(t)
	h := p.install("https://registry.example", nil)
	publishWidgets(t, h)
	full := get(h, "/npm/internal/@acme/widgets", "Accept", acceptNpm)
	expect(t, "status", full.Code, http.StatusOK)
	for k, want := range map[string]string{"Content-Type": contentTypeJSON, "Vary": "Accept", "Cache-Control": "no-cache"} {
		expect(t, k, full.Header().Get(k), want)
	}
	for _, tc := range []struct{ path, accept, contentType string }{
		{"/npm/internal/@acme%2fwidgets", "", contentTypeJSON},
		{"/npm/internal/@acme%2Fwidgets", acceptPnpm, contentTypeAbbreviated},
		{"/npm/internal/@acme/widgets?write=true", acceptPnpm, contentTypeJSON},
		{"/npm/internal/@acme/widgets", contentTypeAbbreviated + ";q=0", contentTypeJSON},
	} {
		rr := get(h, tc.path, "Accept", tc.accept)
		if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != tc.contentType {
			t.Errorf("%s with %q: %d %s", tc.path, tc.accept, rr.Code, rr.Header().Get("Content-Type"))
		}
		if tc.contentType == contentTypeJSON && rr.Body.String() != full.Body.String() {
			t.Errorf("%s: another packument", tc.path)
		}
	}
	rr := get(h, "/npm/internal/@acme/widgets", "Accept", acceptNpm, "If-None-Match", full.Header().Get("ETag"))
	expect(t, "If-None-Match", rr.Code, http.StatusNotModified)
	for _, path := range []string{"/npm/internal/@acme/widgets/1.0.0", "/npm/internal/@acme%2fwidgets/latest"} {
		var v struct {
			ID string `json:"_id"`
		}
		rr := get(h, path)
		_ = json.Unmarshal(rr.Body.Bytes(), &v)
		expect(t, path, v.ID, "@acme/widgets@1.0.0")
	}
}

// TestInstallTarballs fetches each tarball at its dist.tarball path and at the path yarn builds,
// with its own bytes though the file names are the same; paths that name nothing are 404 as JSON.
func TestInstallTarballs(t *testing.T) {
	p := newPipeline(t)
	h := p.install("https://registry.example", nil)
	tarballs := publishWidgets(t, h)
	var doc struct {
		Versions map[string]struct{ Dist struct{ Tarball string } }
	}
	must(t, json.Unmarshal(get(h, "/npm/internal/@acme/widgets").Body.Bytes(), &doc))
	tarball := strings.TrimPrefix(doc.Versions["1.0.0"].Dist.Tarball, "https://registry.example")
	expect(t, "dist.tarball", tarball, "/npm/internal/@acme/widgets/-/widgets-1.0.0.tgz")
	for path, want := range map[string][]byte{
		tarball: tarballs["@acme/widgets"], "/npm/internal/@acme%2fwidgets/-/widgets-1.0.0.tgz": tarballs["@acme/widgets"],
		"/npm/internal/widgets/-/widgets-1.0.0.tgz": tarballs["widgets"],
	} {
		rr := get(h, path)
		if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), want) || rr.Header().Get("Content-Type") != "application/octet-stream" ||
			rr.Header().Get("Docker-Content-Digest") == "" || rr.Header().Get("Last-Modified") == "" {
			t.Errorf("%s: %d, %d bytes, %v", path, rr.Code, rr.Body.Len(), rr.Header())
		}
	}
	for _, path := range []string{"/npm/internal/other", "/npm/internal/@acme/widgets/9.9.9", "/npm/internal/widgets/-/other-1.0.0.tgz",
		"/npm/internal/@acme/widgets/-/widgets-2.0.0.tgz", "/npm/internal/-/token", "/npm/internal/a/b/c"} {
		if rr := get(h, path); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s", path, rr.Code, rr.Body)
		}
	}
}

// TestPackumentCache is the acceptance criterion of #306: after the first render the packument
// is served from index_cache, a document of the index, and no packument asks the registry for
// anything; a publish invalidates it, and the next GET has the new version.
func TestPackumentCache(t *testing.T) {
	p := newPipeline(t)
	h := p.install("https://registry.example", nil)
	p.publishVersions(t, "widgets", "1.0.0")
	reads := len(p.fake.OpenBlobs()) + len(p.fake.StatBlobs())
	first := get(h, "/npm/internal/widgets")
	c, err := p.st.GetIndexCache(t.Context(), p.repo.ID, "widgets", kindPackument)
	must(t, err)
	if first.Code != http.StatusOK || !bytes.Equal(c.Content, first.Body.Bytes()) {
		t.Fatalf("the first GET: %d; index_cache %s", first.Code, c.Content)
	}
	if _, err := p.st.GetIndexCache(t.Context(), p.repo.ID, "widgets", kindAbbreviated); err != nil {
		t.Errorf("the abbreviated packument is not in index_cache: %v", err)
	}
	c.Content = []byte(`{"name": "widgets", "from": "the cache"}`)
	gen, err := p.st.IndexCacheGeneration(t.Context(), p.repo.ID)
	must(t, err)
	_, err = p.st.PutIndexCache(t.Context(), c, gen)
	must(t, err)
	if rr := get(h, "/npm/internal/widgets"); !strings.Contains(rr.Body.String(), "the cache") {
		t.Errorf("the second GET did not come from index_cache: %s", rr.Body)
	}
	if n := len(p.fake.OpenBlobs()) + len(p.fake.StatBlobs()); n != reads {
		t.Errorf("the packuments read %d blobs from the registry", n-reads)
	}
	p.publishVersions(t, "widgets", "1.1.0")
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
	}
	if rr := get(h, "/npm/internal/widgets"); json.Unmarshal(rr.Body.Bytes(), &doc) != nil || doc.DistTags["latest"] != "1.1.0" {
		t.Errorf("after a publish: %s", rr.Body)
	}
}

// TestInstallAnonymous: a repo with anonymous read serves its packument and tarballs to
// anonymous, through RBAC; one without answers 401 with the Basic challenge, as JSON. Without an
// external URL the packument has the request's host and is not kept.
func TestInstallAnonymous(t *testing.T) {
	p := newPipeline(t)
	p.publishVersions(t, "widgets", "1.0.0")
	h := p.install("", auth.NewRBAC(auth.RBACOptions{Index: p.st, AnonymousRead: true, Log: slog.New(slog.DiscardHandler)}))
	anonymous := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}
	if rr := anonymous("/npm/internal/widgets"); rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") != `Basic realm="lajstrom"` {
		t.Errorf("a private repo: %d %v", rr.Code, rr.Header())
	}
	p.repo.AnonymousRead = true
	must(t, p.st.UpdateRepo(t.Context(), &p.repo))
	rr := anonymous("/npm/internal/widgets")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"tarball":"http://example.com/npm/internal/widgets/-/widgets-1.0.0.tgz"`) {
		t.Errorf("a public repo: %d %s", rr.Code, rr.Body)
	}
	if rr := anonymous("/npm/internal/widgets/-/widgets-1.0.0.tgz"); rr.Code != http.StatusOK {
		t.Errorf("a tarball of a public repo: %d", rr.Code)
	}
	if _, err := p.st.GetIndexCache(t.Context(), p.repo.ID, "widgets", kindPackument); err == nil {
		t.Error("a packument of the request's host is in index_cache")
	}
}
