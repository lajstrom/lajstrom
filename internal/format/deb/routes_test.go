package deb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// countingStore is the store the routes read, counting the reads of package and version rows:
// those of the store and those of the transactions of format.CachedDocument, in which
// RenderIndex reads.
type countingStore struct {
	index.Store
	reads *atomic.Int64
}

func (s countingStore) ListPackages(ctx context.Context, repoID int64, prefix string) ([]index.Package, error) {
	s.reads.Add(1)
	return s.Store.ListPackages(ctx, repoID, prefix)
}

func (s countingStore) ListVersions(ctx context.Context, packageID int64) ([]index.Version, error) {
	s.reads.Add(1)
	return s.Store.ListVersions(ctx, packageID)
}

func (s countingStore) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(countingWriter{Writer: w, reads: s.reads}) })
}

// countingWriter is a transaction of countingStore.
type countingWriter struct {
	index.Writer
	reads *atomic.Int64
}

func (w countingWriter) ListPackages(ctx context.Context, repoID int64, prefix string) ([]index.Package, error) {
	w.reads.Add(1)
	return w.Writer.ListPackages(ctx, repoID, prefix)
}

func (w countingWriter) ListVersions(ctx context.Context, packageID int64) ([]index.Version, error) {
	w.reads.Add(1)
	return w.Writer.ListVersions(ctx, packageID)
}

// aptEnv is the apt routes on a fresh ServeMux over the pipeline of newPipeline, reading the
// store through countingStore.
type aptEnv struct {
	*pipeline
	t     *testing.T
	h     http.Handler
	reads *atomic.Int64
}

// envOptions change what newAptEnv builds.
type envOptions struct {
	authz     auth.Authorizer
	providers provider.Set // nil means the fake of the pipeline as "local"
}

func newAptEnv(t *testing.T, o envOptions) *aptEnv {
	t.Helper()
	p := newPipeline(t)
	e := &aptEnv{pipeline: p, t: t, reads: &atomic.Int64{}}
	if o.providers == nil {
		o.providers = provider.Map{"local": p.prov}
	}
	deps := format.Deps{Index: countingStore{Store: p.st, reads: e.reads}, Ingest: p.pipe, Providers: o.providers, Auth: o.authz,
		Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := r.Header.Get("X-Test-User"); name != "" {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindUser, Name: name}))
		}
		mux.ServeHTTP(w, r)
	})
	return e
}

// do sends a request to /apt/ubuntu/<path>; hdr are header name and value pairs.
func (e *aptEnv) do(method, path string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/apt/ubuntu/"+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// get sends a GET of path, which must answer 200, and returns the response.
func (e *aptEnv) get(path string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	rr := e.do(http.MethodGet, path, hdr...)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d %s", path, rr.Code, rr.Body)
	}
	return rr
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestRoutes fetches every document of the suites of fixtureSet as apt does: the Release, then
// each index it lists by name and by hash. Each has the content type of its kind, an ETag of
// its sha256, the size and the hashes the Release lists, and the .gz and .xz decompress to the
// plain Packages; HEAD has the headers without the body, and If-None-Match answers 304.
func TestRoutes(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	for _, suite := range []string{"jammy", "jammy-updates"} {
		rel := e.get("dists/" + suite + "/Release")
		checkHeaders(t, rel, "text/plain", rel.Body.Bytes())
		if !strings.HasPrefix(rel.Body.String(), "Origin: Lajstrom\nLabel: ubuntu\nSuite: "+suite+"\n") {
			t.Errorf("Release of %s:\n%s", suite, rel.Body)
		}
		sums := listed(rel.Body.Bytes())
		if len(sums) == 0 {
			t.Fatalf("the Release of %s lists nothing", suite)
		}
		plain := map[string][]byte{}
		for _, kind := range slices.Sorted(maps.Keys(sums)) { // Packages before its .gz and .xz
			body := e.fetch(suite, kind, sums[kind])
			switch {
			case strings.HasSuffix(kind, ".gz"):
				body = gunzip(t, body)
			case strings.HasSuffix(kind, ".xz"):
				body = unxz(t, body)
			default:
				plain[kind] = body
				continue
			}
			if !bytes.Equal(body, plain[kind[:strings.LastIndexByte(kind, '.')]]) {
				t.Errorf("%s/%s does not decompress to its Packages", suite, kind)
			}
		}
	}
	rr := e.do(http.MethodHead, "dists/jammy/main/binary-amd64/Packages")
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 || rr.Header().Get("Content-Length") == "" || rr.Header().Get("ETag") == "" {
		t.Errorf("HEAD = %d, %d bytes, headers %v", rr.Code, rr.Body.Len(), rr.Header())
	}
	etag := e.get("dists/jammy/Release").Header().Get("ETag")
	if rr := e.do(http.MethodGet, "dists/jammy/Release", "If-None-Match", etag); rr.Code != http.StatusNotModified || rr.Body.Len() != 0 {
		t.Errorf("GET with If-None-Match %s = %d with %d bytes, want 304", etag, rr.Code, rr.Body.Len())
	}
}

// fetch GETs the document kind of suite by its name and by its hash sum, checks that both are
// the same bytes with the headers of the kind and that sum is their sha256, and returns them.
func (e *aptEnv) fetch(suite, kind, sum string) []byte {
	e.t.Helper()
	byName := e.get("dists/" + suite + "/" + kind)
	body := byName.Body.Bytes()
	checkHeaders(e.t, byName, contentType(kind), body)
	if got := sha256Hex(body); got != sum {
		e.t.Errorf("%s/%s: sha256 %s, the Release lists %s", suite, kind, got, sum)
	}
	byHash := e.get("dists/" + suite + "/" + kind[:strings.LastIndexByte(kind, '/')] + "/by-hash/SHA256/" + sum)
	if !bytes.Equal(byHash.Body.Bytes(), body) {
		e.t.Errorf("%s/%s by hash is not the document", suite, kind)
	}
	checkHeaders(e.t, byHash, contentType(kind), body)
	return body
}

// checkHeaders checks Content-Type, ETag and Last-Modified of a document with body.
func checkHeaders(t *testing.T, rr *httptest.ResponseRecorder, contentType string, body []byte) {
	t.Helper()
	h := rr.Header()
	if h.Get("Content-Type") != contentType || h.Get("ETag") != `"`+sha256Hex(body)+`"` || h.Get("Last-Modified") == "" {
		t.Errorf("headers %v, want Content-Type %s and the ETag of the body", h, contentType)
	}
}

// TestRoutesNotFound checks the 404s: unknown suites, components, architectures and files, the
// signed forms of the Release until #41, a hash the Release does not list in that directory,
// the routes of #41 and #42, and a repo that does not exist.
func TestRoutesNotFound(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	universe := listed(e.get("dists/jammy/Release").Body.Bytes())["universe/binary-amd64/Packages"]
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "dists/focal/Release", "no package in the suite focal"},
		{http.MethodGet, "dists/Jammy/Release", `"Jammy" is not a suite`},
		{http.MethodGet, "dists/jammy/InRelease", "not signed until #41"},
		{http.MethodGet, "dists/jammy/Release.gpg", "not signed until #41"},
		{http.MethodGet, "dists/jammy/Contents-amd64", "no dists/jammy/Contents-amd64"},
		{http.MethodGet, "dists/jammy/multiverse/binary-amd64/Packages", "no dists/jammy/multiverse/binary-amd64/Packages"},
		{http.MethodGet, "dists/jammy/main/binary-i386/Packages.xz", "no dists/jammy/main/binary-i386/Packages.xz"},
		{http.MethodGet, "dists/jammy/main/binary-all/Packages", "no dists/jammy/main/binary-all/Packages"},
		{http.MethodGet, "dists/jammy/main/binary-amd64/Packages.bz2", "no dists/jammy/main/binary-amd64/Packages.bz2"},
		{http.MethodGet, "dists/jammy/main/binary-amd64/Release", "no dists/jammy/main/binary-amd64/Release"},
		{http.MethodGet, "dists/jammy/main/i18n/Translation-en", "no dists/jammy/main/i18n/Translation-en"},
		{http.MethodGet, "dists/focal/main/binary-amd64/Packages", "no package in the suite focal"},
		{http.MethodGet, "dists/jammy/main/binary-amd64/by-hash/SHA256/" + strings.Repeat("0", 64), "no index with the SHA256"},
		{http.MethodGet, "dists/jammy/main/binary-amd64/by-hash/SHA256/" + universe, "no index with the SHA256"},
		{http.MethodGet, "dists/jammy/universe/by-hash/SHA256/" + universe, ""},
		{http.MethodGet, "dists/focal/main/binary-amd64/by-hash/SHA256/" + universe, "no package in the suite focal"},
		{http.MethodGet, "pool/main/h/hello/hello_1.0.0-1_amd64.deb", ""},
		{http.MethodGet, "key.asc", ""},
		{http.MethodPut, "upload?suite=jammy&component=main", ""},
	} {
		rr := e.do(c.method, c.path)
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), c.body) {
			t.Errorf("%s %s = %d %q, want 404 containing %q", c.method, c.path, rr.Code, rr.Body, c.body)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/apt/nope/dists/jammy/Release", nil)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET of a repo that does not exist = %d", rr.Code)
	}
}

// TestRoutesNewRender checks what a publish does to the documents a client holds: the indexes
// by name are the new ones, a hash of the old render is 404 (apt falls back to the name), and
// the hash of the new render is served.
func TestRoutesNewRender(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	const kind = "main/binary-amd64/Packages.xz"
	old := listed(e.get("dists/jammy/Release").Body.Bytes())[kind]
	e.get("dists/jammy/main/binary-amd64/by-hash/SHA256/" + old)

	newer, _ := testutil.Deb(testutil.DebOptions{Version: "1.2.0-1"})
	e.uploadTo(t, newer, "jammy", "main")
	if rr := e.do(http.MethodGet, "dists/jammy/main/binary-amd64/by-hash/SHA256/"+old); rr.Code != http.StatusNotFound {
		t.Errorf("GET of the hash of the old render = %d, want 404", rr.Code)
	}
	current := listed(e.get("dists/jammy/Release").Body.Bytes())[kind]
	if current == old {
		t.Fatalf("the Release lists the same %s after a publish", kind)
	}
	body := e.get("dists/jammy/main/binary-amd64/by-hash/SHA256/" + current).Body.Bytes()
	if !strings.Contains(string(unxz(t, body)), "Version: 1.2.0-1\n") {
		t.Errorf("the new %s has no hello 1.2.0-1", kind)
	}
}

// TestRoutesCache checks index_cache: after the first GET, every document of the suite is
// served without reading a package or a version row, by name and by hash, with HEAD and with
// If-None-Match; an invalidation, which ingest and delete do, makes the next GET render again.
func TestRoutesCache(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	fixtureSet(t, e.pipeline)
	rel := e.get("dists/jammy/Release")
	if n := e.reads.Load(); n == 0 {
		t.Fatal("the first GET read no package or version: nothing was rendered")
	}
	e.reads.Store(0)
	sums := listed(rel.Body.Bytes())
	for kind, sum := range sums {
		dir := kind[:strings.LastIndexByte(kind, '/')]
		e.get("dists/jammy/" + kind)
		e.get("dists/jammy/" + dir + "/by-hash/SHA256/" + sum)
		e.do(http.MethodHead, "dists/jammy/"+kind)
		e.do(http.MethodGet, "dists/jammy/"+kind, "If-None-Match", `"`+sum+`"`)
	}
	e.get("dists/jammy/Release")
	e.do(http.MethodGet, "dists/jammy/multiverse/binary-amd64/Packages")
	if n := e.reads.Load(); n != 0 {
		t.Errorf("%d reads of packages and versions after the first GET, want 0", n)
	}
	if got := e.get("dists/jammy/Release").Body.String(); got != rel.Body.String() {
		t.Errorf("the cached Release changed:\n%s", got)
	}

	must(t, e.st.InvalidateIndexCache(t.Context(), e.repo.ID, ""))
	e.get("dists/jammy/main/binary-amd64/Packages")
	if n := e.reads.Load(); n == 0 {
		t.Error("the GET after the invalidation read no package or version: it did not render")
	}
}

// authorizer lets alice read apt/ubuntu and nobody else anything.
type authorizer struct{}

func (authorizer) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	return p.Name == "alice" && a == auth.ActionRead && f == Name && r == "ubuntu", nil
}

// TestRoutesAuthorize checks that the routes need read: the anonymous principal gets 401 with
// a Basic challenge, another user 404, and alice the documents.
func TestRoutesAuthorize(t *testing.T) {
	e := newAptEnv(t, envOptions{authz: authorizer{}})
	fixtureSet(t, e.pipeline)
	for _, path := range []string{"dists/jammy/Release", "dists/jammy/main/binary-amd64/Packages",
		"dists/jammy/main/binary-amd64/by-hash/SHA256/" + strings.Repeat("0", 64)} {
		if rr := e.do(http.MethodGet, path); rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("anonymous GET %s = %d", path, rr.Code)
		}
		if rr := e.do(http.MethodGet, path, "X-Test-User", "bob"); rr.Code != http.StatusNotFound {
			t.Errorf("bob's GET %s = %d, want 404", path, rr.Code)
		}
	}
	if e.reads.Load() != 0 {
		t.Error("a denied request read the index")
	}
	e.get("dists/jammy/Release", "X-Test-User", "alice")
}

// TestRoutesRenderFails checks that a render that fails is 500 with the cause in the log, not
// in the body: a version without metadata in the index of a repo whose registry the routes do
// not have.
func TestRoutesRenderFails(t *testing.T) {
	e := newAptEnv(t, envOptions{providers: provider.Map{}})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	e.upload(t, hello, "jammy")
	dropMetadata(t, e.pipeline, "jammy")
	for _, path := range []string{"dists/jammy/Release", "dists/jammy/main/binary-amd64/Packages",
		"dists/jammy/main/binary-amd64/by-hash/SHA256/" + strings.Repeat("0", 64)} {
		if rr := e.do(http.MethodGet, path); rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "registry") {
			t.Errorf("GET %s = %d %q, want 500 without the cause", path, rr.Code, rr.Body)
		}
	}
}

// TestHandlersWithoutRepo checks the handlers on a request that the repo middleware did not
// resolve: 500, a programming error.
func TestHandlersWithoutRepo(t *testing.T) {
	h := &handlers{deps: format.Deps{Log: slog.New(slog.DiscardHandler)}}
	for _, serve := range []http.HandlerFunc{h.release, h.packages, h.byHash} {
		rr := httptest.NewRecorder()
		serve(rr, httptest.NewRequest(http.MethodGet, "/apt/ubuntu/dists/jammy/Release", nil))
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("a handler without a repo = %d, want 500", rr.Code)
		}
	}
}
