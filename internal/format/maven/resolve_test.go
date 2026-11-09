package maven

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// countingStore is the store the routes read, counting the reads of files and versions: those
// of the store and those of the transactions of format.CachedDocument, in which RenderIndex
// reads.
type countingStore struct {
	index.Store
	reads *atomic.Int64
}

func (s countingStore) FindFiles(ctx context.Context, repoID int64, name string) ([]index.FileMatch, error) {
	s.reads.Add(1)
	return s.Store.FindFiles(ctx, repoID, name)
}

func (s countingStore) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(countingWriter{Writer: w, reads: s.reads}) })
}

// countingWriter is a transaction of countingStore.
type countingWriter struct {
	index.Writer
	reads *atomic.Int64
}

func (w countingWriter) ListVersions(ctx context.Context, packageID int64) ([]index.Version, error) {
	w.reads.Add(1)
	return w.Writer.ListVersions(ctx, packageID)
}

// authorizer lets alice read maven/releases and nobody else anything.
type authorizer struct{}

func (authorizer) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	return p.Name == "alice" && a == auth.ActionRead && f == Name && r == "releases", nil
}

// mavenEnv is the maven routes on a fresh ServeMux over the real pipeline, SQLite and the fake
// registry, with the repo maven/releases.
type mavenEnv struct {
	t     *testing.T
	st    *sqlite.Store
	fake  *providertest.Fake
	pipe  *ingest.Pipeline
	repo  index.Repo
	h     http.Handler
	reads *atomic.Int64
}

// newMavenEnv builds a mavenEnv whose routes ask authz, nil for none, and whose repo has policy.
func newMavenEnv(t *testing.T, authz auth.Authorizer, policy string) *mavenEnv {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:maven-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &mavenEnv{t: t, st: st, fake: &providertest.Fake{RegistryName: "local"}, reads: &atomic.Int64{},
		repo: index.Repo{Format: Name, Name: "releases", Registry: "local", Prefix: "maven/releases", Kind: "hosted"}}
	if policy != "" {
		e.repo.Policy = []byte(policy)
	}
	must(t, st.CreateRepo(ctx, &e.repo))
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.fake}, Index: st, Log: log,
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	deps := format.Deps{Index: countingStore{Store: st, reads: e.reads}, Ingest: e.pipe, Providers: provider.Map{"local": e.fake},
		Auth: authz, Log: log}
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

// publish deploys a version through the pipeline: its files by their paths below the repo.
func (e *mavenEnv) publish(files map[string][]byte) {
	e.t.Helper()
	var up format.Upload
	for p, b := range files {
		up.Files = append(up.Files, upFile(p, b))
	}
	if _, err := e.pipe.Upload(e.t.Context(), e.repo, up, auth.Principal{Kind: auth.KindUser, Name: "alice"}); err != nil {
		e.t.Fatalf("publish: %v", err)
	}
}

// publishVersion deploys version of com.example:lib-core with its jar and its POM.
func (e *mavenEnv) publishVersion(version string) {
	e.t.Helper()
	jar, ji := testutil.Jar(testutil.JarOptions{Version: version})
	pom, pi := testutil.POM(testutil.POMOptions{Version: version})
	e.publish(map[string][]byte{ji.Path: jar, pi.Path: pom})
}

// do sends a request to /maven/<repo>/<path>; hdr are header name and value pairs.
func (e *mavenEnv) do(method, repo, path string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/maven/"+repo+"/"+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// files are the files of com.example:lib-core:1.2.0 that newPublished deploys, by path: a jar,
// its sources (a classifier), the POM, and the signature of the jar.
func files() map[string][]byte {
	jar, ji := testutil.Jar(testutil.JarOptions{})
	sources, si := testutil.Jar(testutil.JarOptions{Classifier: "sources"})
	pom, pi := testutil.POM(testutil.POMOptions{})
	return map[string][]byte{
		ji.Path: jar, si.Path: sources, pi.Path: pom,
		ji.Path + ".asc": []byte("-----BEGIN PGP SIGNATURE-----\n\nnot a signature\n-----END PGP SIGNATURE-----\n"),
	}
}

// newPublished is a mavenEnv with the files of files() deployed.
func newPublished(t *testing.T, authz auth.Authorizer, policy string) *mavenEnv {
	t.Helper()
	e := newMavenEnv(t, authz, policy)
	e.publish(files())
	return e
}

// header checks that the response has the header name with the value want.
func header(t *testing.T, what string, rr *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rr.Header().Get(name); got != want {
		t.Errorf("%s: %s %q, want %q", what, name, got, want)
	}
}

// TestResolveFiles fetches every file of a version, a jar, a classifier, the POM and a
// signature, and its four checksum files, with GET and HEAD: the bytes, the Content-Type of the
// extension, the size, the blob digest as the ETag and the manifest digest; a checksum is the
// lowercase hex alone, as text/plain.
func TestResolveFiles(t *testing.T) {
	e := newPublished(t, nil, "")
	pkg, err := e.st.GetPackage(t.Context(), e.repo.ID, "com.example:lib-core")
	must(t, err)
	v, err := e.st.GetVersion(t.Context(), pkg.ID, "1.2.0", "")
	must(t, err)
	for path, contentType := range map[string]string{
		"com/example/lib-core/1.2.0/lib-core-1.2.0.jar":         "application/java-archive",
		"com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar": "application/java-archive",
		"com/example/lib-core/1.2.0/lib-core-1.2.0.pom":         "application/xml",
		"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.asc":     "application/pgp-signature",
	} {
		body := files()[path]
		rr := e.do(http.MethodGet, "releases", path)
		if rr.Code != http.StatusOK || rr.Body.String() != string(body) {
			t.Errorf("GET %s: %d, %d bytes, want 200 and %d bytes", path, rr.Code, rr.Body.Len(), len(body))
		}
		header(t, "GET "+path, rr, "Content-Type", contentType)
		header(t, "GET "+path, rr, "Content-Length", strconv.Itoa(len(body)))
		header(t, "GET "+path, rr, "ETag", `"`+digest.FromBytes(body).String()+`"`)
		header(t, "GET "+path, rr, "Docker-Content-Digest", v.Digest.String())
		rr = e.do(http.MethodHead, "releases", path)
		if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d with %d bytes", path, rr.Code, rr.Body.Len())
		}
		header(t, "HEAD "+path, rr, "Content-Type", contentType)
		header(t, "HEAD "+path, rr, "Content-Length", strconv.Itoa(len(body)))
		for algorithm, sum := range sumsOf(body) {
			rr := e.do(http.MethodGet, "releases", path+"."+algorithm)
			if rr.Code != http.StatusOK || rr.Body.String() != sum {
				t.Errorf("GET %s.%s: %d %q, want 200 %q", path, algorithm, rr.Code, rr.Body, sum)
			}
			header(t, "GET "+path+"."+algorithm, rr, "Content-Type", "text/plain")
			rr = e.do(http.MethodHead, "releases", path+"."+algorithm)
			if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
				t.Errorf("HEAD %s.%s: %d with %d bytes", path, algorithm, rr.Code, rr.Body.Len())
			}
			header(t, "HEAD "+path+"."+algorithm, rr, "Content-Length", strconv.Itoa(len(sum)))
		}
	}
}

// TestResolveNotFound checks the paths that name nothing the repo has: a plain 404 for GET and
// HEAD, never a 400 for a path that does not parse. SNAPSHOT versions, the maven-metadata.xml
// of a version and of a group are 404 too.
func TestResolveNotFound(t *testing.T) {
	e := newPublished(t, nil, "")
	for _, path := range []string{
		"com/example/lib-core/9.9/lib-core-9.9.jar",                       // a version the repo does not have
		"com/example/lib-core/9.9/lib-core-9.9.jar.sha1",                  // its checksum
		"com/example/lib-core/1.2.0/lib-core-1.2.0-javadoc.jar",           // a file the version does not have
		"com/example/lib-core/1.2.0/lib-core-1.2.0-javadoc.jar.sha512",    // its checksum
		"org/other/lib-core/1.2.0/lib-core-1.2.0.jar",                     // the file name in another group
		"Com/Example/lib-core/1.2.0/lib-core-1.2.0.jar",                   // another spelling of the group
		"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha1.md5",          // a checksum of a checksum
		"com/example/lib-core/1.2.0/other-1.2.0.jar",                      // not a file of the version
		"com/example/lib-core/1.2.0-SNAPSHOT/lib-core-1.2.0-SNAPSHOT.jar", // SNAPSHOT versions are after the MVP
		"com/example/lib-core/1.2.0-SNAPSHOT/maven-metadata.xml",
		"com/example/lib-core/1.2.0-SNAPSHOT/maven-metadata.xml.sha1",
		"com/example/lib-core/1.2.0/maven-metadata.xml", // the metadata of a version
		"com/example/lib-core/1.2.0/maven-metadata.xml.md5",
		"com/example/maven-metadata.xml", // the metadata of a group
		"com/example/nothing/maven-metadata.xml",
		"com/example/nothing/maven-metadata.xml.sha256",
		"maven-metadata.xml",
		"com/example/lib-core/",
		"",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if rr := e.do(method, "releases", path); rr.Code != http.StatusNotFound {
				t.Errorf("%s %q: %d %s, want 404", method, path, rr.Code, rr.Body)
			}
		}
	}
}

// metadataGolden is the maven-metadata.xml of TestResolveMetadata, with latest, the versions after
// 1.10 and lastUpdated as %s.
const metadataGolden = `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>com.example</groupId>
  <artifactId>lib-core</artifactId>
  <versioning>
    <latest>%s</latest>
    <release>1.10</release>
    <versions>
      <version>1.0-rc1</version>
      <version>1.0</version>
      <version>1.2.0</version>
      <version>1.10</version>%s
    </versions>
    <lastUpdated>%s</lastUpdated>
  </versioning>
</metadata>
`

// TestResolveMetadata deploys versions out of Maven's order and reads the maven-metadata.xml of
// the artifact: the versions in that order, latest and release the greatest, lastUpdated the
// newest created in the index, and its four checksum files those of the bytes served; GET and
// HEAD, and If-None-Match is 304. The document is in index_cache after the first GET, and the
// second reads no version; another deploy, of a pre-release, drops it, and the next GET has the
// new version as latest, and the release as it was.
func TestResolveMetadata(t *testing.T) {
	e := newPublished(t, nil, "")
	for _, v := range []string{"1.10", "1.0", "1.0-rc1"} {
		e.publishVersion(v)
	}
	const path = "com/example/lib-core/maven-metadata.xml"
	e.checkMetadata(path, fmt.Sprintf(metadataGolden, "1.10", "", e.lastUpdated()))
	if n := e.reads.Load(); n != 1 { // the render of the first GET; its checksums are from the same render
		t.Errorf("the document and its checksum files read the versions %d times, want once", n)
	}
	cached, err := e.st.GetIndexCache(t.Context(), e.repo.ID, "com.example/lib-core", "maven-metadata")
	must(t, err)
	rr := e.do(http.MethodGet, "releases", path)
	if rr.Body.String() != string(cached.Content) || e.reads.Load() != 1 {
		t.Errorf("the second GET read the versions again, or is not the document of index_cache")
	}
	header(t, "GET "+path, rr, "ETag", `"`+cached.SHA256+`"`)
	if rr := e.do(http.MethodGet, "releases", path, "If-None-Match", `"`+cached.SHA256+`"`); rr.Code != http.StatusNotModified {
		t.Errorf("GET with If-None-Match: %d, want 304", rr.Code)
	}
	if rr := e.do(http.MethodHead, "releases", path); rr.Code != http.StatusOK || rr.Body.Len() != 0 ||
		rr.Header().Get("Content-Length") != strconv.Itoa(len(cached.Content)) {
		t.Errorf("HEAD %s: %d with %d bytes, Content-Length %s", path, rr.Code, rr.Body.Len(), rr.Header().Get("Content-Length"))
	}

	e.publishVersion("2.0-beta-1")
	if _, err := e.st.GetIndexCache(t.Context(), e.repo.ID, "com.example/lib-core", "maven-metadata"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("index_cache after a deploy: %v, want index.ErrNotFound", err)
	}
	e.checkMetadata(path, fmt.Sprintf(metadataGolden, "2.0-beta-1", "\n      <version>2.0-beta-1</version>", e.lastUpdated()))
}

// lastUpdated is the newest created of the versions of com.example:lib-core in the index, as
// maven-metadata.xml writes it.
func (e *mavenEnv) lastUpdated() string {
	e.t.Helper()
	pkg, err := e.st.GetPackage(e.t.Context(), e.repo.ID, "com.example:lib-core")
	must(e.t, err)
	vs, err := e.st.ListVersions(e.t.Context(), pkg.ID)
	must(e.t, err)
	var newest time.Time
	for _, v := range vs {
		if v.CreatedAt.After(newest) {
			newest = v.CreatedAt
		}
	}
	return newest.UTC().Format("20060102150405")
}

// checkMetadata GETs the maven-metadata.xml at path, which must be want, and its four checksum
// files, which must be those of its bytes.
func (e *mavenEnv) checkMetadata(path, want string) {
	e.t.Helper()
	rr := e.do(http.MethodGet, "releases", path)
	if rr.Code != http.StatusOK || rr.Body.String() != want {
		e.t.Fatalf("GET %s: %d\n%s\nwant\n%s", path, rr.Code, rr.Body, want)
	}
	header(e.t, "GET "+path, rr, "Content-Type", "application/xml")
	for algorithm, sum := range sumsOf(rr.Body.Bytes()) {
		rr := e.do(http.MethodGet, "releases", path+"."+algorithm)
		if rr.Code != http.StatusOK || rr.Body.String() != sum {
			e.t.Errorf("GET %s.%s: %d %q, want %q", path, algorithm, rr.Code, rr.Body, sum)
		}
		header(e.t, "GET "+path+"."+algorithm, rr, "Content-Type", "text/plain")
	}
}

// TestResolveAuthorize checks that every route needs read: the anonymous principal gets 401 with
// a Basic challenge, a user without read 404 on the repo and on a repo that does not exist
// alike, without a read of the index, and a reader the files.
func TestResolveAuthorize(t *testing.T) {
	e := newPublished(t, authorizer{}, "")
	paths := map[string]int{ // the status alice gets
		"com/example/lib-core/1.2.0/lib-core-1.2.0.jar":      http.StatusOK,
		"com/example/lib-core/1.2.0/lib-core-1.2.0.jar.sha1": http.StatusOK,
		"com/example/lib-core/maven-metadata.xml":            http.StatusOK,
		"com/example/lib-core/maven-metadata.xml.sha512":     http.StatusOK,
		"com/example/lib-core/9.9/lib-core-9.9.jar":          http.StatusNotFound,
	}
	for path, want := range paths {
		for _, repo := range []string{"releases", "nothing"} {
			if rr := e.do(http.MethodGet, repo, path); rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("anonymous GET %s/%s: %d, want 401 with a challenge", repo, path, rr.Code)
			}
			if rr := e.do(http.MethodGet, repo, path, "X-Test-User", "bob"); rr.Code != http.StatusNotFound {
				t.Errorf("bob's GET %s/%s: %d, want 404", repo, path, rr.Code)
			}
		}
		if rr := e.do(http.MethodGet, "nothing", path, "X-Test-User", "alice"); rr.Code != http.StatusNotFound {
			t.Errorf("alice's GET nothing/%s: %d, want 404", path, rr.Code)
		}
		if e.reads.Load() != 0 {
			t.Fatalf("a denied request read the index")
		}
		if rr := e.do(http.MethodGet, "releases", path, "X-Test-User", "alice"); rr.Code != want {
			t.Errorf("alice's GET %s: %d, want %d", path, rr.Code, want)
		}
		e.reads.Store(0)
	}
}

// TestResolveRedirect checks that a file is a 307 to the backend's storage when the provider
// gives a URL, and streams when the repo's policy.blobRedirect is never.
func TestResolveRedirect(t *testing.T) {
	const path = "com/example/lib-core/1.2.0/lib-core-1.2.0.jar"
	for _, tt := range []struct {
		name, policy string
		want         int
	}{{"auto", "", http.StatusTemporaryRedirect}, {"never", `{"blobRedirect": "never"}`, http.StatusOK}} {
		t.Run(tt.name, func(t *testing.T) {
			e := newPublished(t, nil, tt.policy)
			e.fake.RedirectURL = func(_ string, d digest.Digest) (string, bool) { return "https://cdn.test/" + d.Encoded(), true }
			rr := e.do(http.MethodGet, "releases", path)
			if rr.Code != tt.want || (tt.want == http.StatusTemporaryRedirect) != strings.HasPrefix(rr.Header().Get("Location"), "https://cdn.test/") {
				t.Errorf("%d to %q, want %d", rr.Code, rr.Header().Get("Location"), tt.want)
			}
		})
	}
}

// TestResolveWithoutRepo checks the handler on a request that the repo middleware did not
// resolve: 500, a programming error.
func TestResolveWithoutRepo(t *testing.T) {
	h := &resolver{deps: format.Deps{Log: slog.New(slog.DiscardHandler)}}
	rr := httptest.NewRecorder()
	h.get(rr, httptest.NewRequest(http.MethodGet, "/maven/releases/com/example/lib-core/maven-metadata.xml", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("a request without a repo: %d, want 500", rr.Code)
	}
}
