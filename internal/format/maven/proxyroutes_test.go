package maven

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // the fake upstream's .sha1 files
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/proxy"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fakeCentral is an upstream laid out as Maven Central is, below /maven2/: the files it holds,
// each with its .sha1 (the hex in capitals and a file name, as some repositories write it) unless the test
// takes it away, and the maven-metadata.xml documents among them. It counts the requests of each
// kind.
type fakeCentral struct {
	srv               *httptest.Server
	mu                sync.Mutex
	files             map[string][]byte
	sums              map[string]string // path → the sha1 its .sha1 says; absent: no .sha1
	docs, sha1s, gets atomic.Int32
	fail              atomic.Bool // every request is 500
}

func newFakeCentral(t *testing.T) *fakeCentral {
	t.Helper()
	u := &fakeCentral{files: map[string][]byte{}, sums: map[string]string{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeCentral) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fail.Load() {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/maven2/")
	switch target, isSum := strings.CutSuffix(p, ".sha1"); {
	case isSum:
		u.sha1s.Add(1)
		sum, ok := u.sums[target]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(strings.ToUpper(sum) + "  file\n"))
		return
	case strings.HasSuffix(p, MetadataFile):
		u.docs.Add(1)
	default:
		u.gets.Add(1)
	}
	b, ok := u.files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}

// put makes the upstream hold b at path, with its .sha1 unless sum is "-", or with sum.
func (u *fakeCentral) put(path string, b []byte, sum string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.files[path] = b
	switch sum {
	case "-":
	case "":
		s := sha1.Sum(b) //nolint:gosec // see the import
		u.sums[path] = hex.EncodeToString(s[:])
	default:
		u.sums[path] = sum
	}
}

// proxyEnv is a proxy repo maven/central of the fake upstream over SQLite, the fake registry,
// the real ingest pipeline and the core of proxy repos, served by the maven routes.
type proxyEnv struct {
	st   *sqlite.Store
	fake *providertest.Fake
	pipe *ingest.Pipeline
	up   *fakeCentral
	repo index.Repo
	core *proxy.Core
	h    http.Handler
	now  atomic.Int64
}

// proxyDBs numbers the in-memory databases of the proxy tests.
var proxyDBs atomic.Int64

func newProxyEnv(t *testing.T, policy string) *proxyEnv {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:maven-proxy-%d?mode=memory&cache=shared", proxyDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &proxyEnv{st: st, fake: &providertest.Fake{RegistryName: "local"}, up: newFakeCentral(t)}
	e.now.Store(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	e.repo = index.Repo{Format: Name, Name: "central", Registry: "local", Prefix: "maven/central", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "central"}`)}
	if policy != "" {
		e.repo.Policy = json.RawMessage(`{"maven": {"versionPolicy": "` + policy + `"}}`)
	}
	must(t, st.CreateRepo(ctx, &e.repo))
	log := slog.New(slog.DiscardHandler)
	up, err := proxy.NewUpstream(config.Upstream{Name: "central", Format: Name, URL: e.up.srv.URL + "/maven2/", Timeout: 5 * time.Second},
		proxy.UpstreamOptions{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, Log: log})
	must(t, err)
	providers := provider.Map{"local": e.fake}
	plugins := func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }
	e.pipe = ingest.New(ingest.Options{Providers: providers, Index: st, Log: log, Plugins: plugins})
	e.core = proxy.New(proxy.Options{Index: st, Ingest: e.pipe, Providers: providers, Upstreams: map[string]*proxy.Upstream{"central": up},
		Plugins: plugins, Now: func() time.Time { return time.Unix(0, e.now.Load()).UTC() }, Log: log})
	deps := format.Deps{Index: st, Ingest: e.pipe, Sessions: e.pipe, Providers: providers, Proxy: e.core, Log: log}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindUser, Name: "alice"})))
	})
	return e
}

// get sends a GET of /maven/<repo>/<path> to h.
func get(h http.Handler, repo, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/maven/"+repo+"/"+path, nil))
	return rr
}

// served fails the test unless rr is 200 with the bytes want.
func served(t *testing.T, rr *httptest.ResponseRecorder, want []byte, what string) {
	t.Helper()
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), want) {
		t.Errorf("%s: %d with %d bytes %q, want 200 with %d bytes", what, rr.Code, rr.Body.Len(), truncated(rr.Body.String()), len(want))
	}
}

func truncated(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// upstreamDoc is the maven-metadata.xml of com.example:lib-core that lists versions, with
// latest and release as given, the namespace Maven Central writes.
func upstreamDoc(latest, release string, versions ...string) []byte {
	var v strings.Builder
	for _, s := range versions {
		v.WriteString("<version>" + s + "</version>")
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://maven.apache.org/METADATA/1.1.0"><groupId>com.example</groupId><artifactId>lib-core</artifactId>` +
		`<versioning><latest>` + latest + `</latest><release>` + release + `</release><versions>` + v.String() + `</versions>` +
		`<lastUpdated>20261101000000</lastUpdated></versioning></metadata>`)
}

// TestProxyResolve resolves through a maven proxy as mvn and Gradle do: the artifact's
// maven-metadata.xml is the upstream's, without the SNAPSHOT versions a release repo does not
// take and with latest chosen anew when it was one, asked once in its TTL, its checksums those of
// the bytes served; each file is fetched on its first request, alone, checked against the
// upstream's .sha1, and served again without a request; the checksums of a file are those of the
// bytes held; the POM is kept as the upstream has it. Nothing goes through a temporary file.
func TestProxyResolve(t *testing.T) {
	formattest.NoTempDir(t)
	e := newProxyEnv(t, "")
	jar, ji := testutil.Jar(testutil.JarOptions{})
	src, si := testutil.Jar(testutil.JarOptions{Classifier: "sources"})
	pom := project(`<groupId>com.example</groupId><artifactId>lib-core</artifactId><version>${revision}</version>`)
	pomPath := "com/example/lib-core/1.2.0/lib-core-1.2.0.pom"
	e.up.put(ji.Path, jar, "")
	e.up.put(si.Path, src, "")
	e.up.put(pomPath, pom, "")
	e.up.put(metadataPath, upstreamDoc("1.3.0-SNAPSHOT", "1.2.0", "1.1.0", "1.2.0", "1.3.0-SNAPSHOT"), "")

	rr := get(e.h, "central", metadataPath)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", metadataPath, rr.Code, rr.Body)
	}
	doc, err := readRepoMetadata(rr.Body.Bytes())
	must(t, err)
	if v := doc.Versioning; !slices.Equal(v.Versions, []string{"1.1.0", "1.2.0"}) || v.Latest != "1.2.0" || v.Release != "1.2.0" ||
		v.LastUpdated != "20261101000000" || doc.GroupID != "com.example" {
		t.Errorf("the proxied metadata: %s", rr.Body)
	}
	served(t, get(e.h, "central", metadataPath+".sha1"), []byte(sumsOf(rr.Body.Bytes())[SHA1]), "the metadata's sha1")
	expect(t, "metadata requests in the TTL", e.up.docs.Load(), int32(1))

	for range 2 {
		rr := get(e.h, "central", ji.Path)
		served(t, rr, jar, "the jar")
		if rr.Header().Get("Docker-Content-Digest") == "" {
			t.Errorf("the jar without Docker-Content-Digest: %v", rr.Header())
		}
	}
	served(t, get(e.h, "central", si.Path), src, "the sources jar")
	served(t, get(e.h, "central", pomPath), pom, "a POM a deploy would refuse")
	expect(t, "file requests", e.up.gets.Load(), int32(3))
	for _, a := range []string{SHA1, SHA512} {
		served(t, get(e.h, "central", ji.Path+"."+a), []byte(sumsOf(jar)[a]), "the jar's "+a)
	}
	expect(t, "file requests after the checksums", e.up.gets.Load(), int32(3))

	pkg, err := e.st.GetPackage(t.Context(), e.repo.ID, "com.example:lib-core")
	must(t, err)
	vs, err := e.st.ListVersions(t.Context(), pkg.ID)
	must(t, err)
	var got []string
	for _, v := range vs {
		got = append(got, v.Version+" "+v.Variant)
	}
	slices.Sort(got)
	expect(t, "the versions held", got, []string{"1.2.0 jar", "1.2.0 jar:sources", "1.2.0 pom"})
}

// TestProxyRefuses: a file whose bytes are not those of the upstream's .sha1 is 502 and not
// held; a file the upstream does not have, or whose .sha1 it does not have, is 404, the latter
// without a request of the file; a SNAPSHOT version and its files are 404 in a release repo
// without a request; a deploy is 405 with Allow.
func TestProxyRefuses(t *testing.T) {
	e := newProxyEnv(t, "")
	jar, ji := testutil.Jar(testutil.JarOptions{})
	e.up.put(ji.Path, jar, strings.Repeat("ab", 20))
	sources := "com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar"
	e.up.put(sources, []byte("sources"), "-")
	if rr := get(e.h, "central", ji.Path); rr.Code != http.StatusBadGateway {
		t.Errorf("a jar that fails its sha1: %d %s, want 502", rr.Code, rr.Body)
	}
	expect(t, "manifests pushed and upload sessions left open", []int{len(e.fake.PushReferences()), e.fake.OpenUploads()}, []int{0, 0})
	rejects, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: "proxy.reject"})
	must(t, err)
	if len(rejects) != 1 || !strings.Contains(string(rejects[0].Details), `"algorithm":"sha1"`) {
		t.Errorf("proxy.reject rows: %+v", rejects)
	}
	before := e.up.gets.Load()
	for _, p := range []string{sources, "com/example/lib-core/9.9.9/lib-core-9.9.9.jar", "com/example/lib-core/1.0-SNAPSHOT/maven-metadata.xml",
		"com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-20261114.101112-3.jar", "com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-SNAPSHOT.jar"} {
		if rr := get(e.h, "central", p); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d %s, want 404", p, rr.Code, rr.Body)
		}
	}
	expect(t, "file requests for files without a .sha1 and SNAPSHOT versions", e.up.gets.Load()-before, int32(0))
	expect(t, "documents asked for SNAPSHOT versions", e.up.docs.Load(), int32(0))
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/maven/central/"+ji.Path, bytes.NewReader(jar)))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("a deploy to a proxy repo: %d %v %s", rr.Code, rr.Header(), rr.Body)
	}
}

// TestProxyUncheckedSignature covers a signature whose upstream publishes no .sha1 (ADR 0013,
// Amendment, decided on #1152): fetched once and served unchecked, a version of its own whose
// manifest has lajstrom.origin.url and no lajstrom.origin.checksum, its proxy.fetch row the
// checksum "", its checksum files those of the bytes held. A signature with a .sha1 is checked;
// one the upstream does not have is 404; one past the repo's maxFileSize is 403.
func TestProxyUncheckedSignature(t *testing.T) {
	ctx := t.Context()
	e := newProxyEnv(t, "")
	sig := "com/example/lib-core/1.2.0/lib-core-1.2.0.jar.asc"
	body := []byte("-----BEGIN PGP SIGNATURE-----\n\niQEzBAABCAAdFiEE\n-----END PGP SIGNATURE-----\n")
	e.up.put(sig, body, "-")
	for range 2 {
		served(t, get(e.h, "central", sig), body, "a signature without a .sha1")
	}
	expect(t, "file requests", e.up.gets.Load(), int32(1))
	served(t, get(e.h, "central", sig+".sha1"), []byte(sumsOf(body)[SHA1]), "the signature's sha1, of the bytes held")
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, "com.example:lib-core")
	must(t, err)
	v, err := e.st.GetVersion(ctx, pkg.ID, "1.2.0", "jar.asc")
	must(t, err)
	if v.Attrs[profile.AnnotationOriginURL] != e.up.srv.URL+"/maven2/"+sig || v.Attrs[profile.AnnotationOriginChecksum] != "" {
		t.Errorf("the signature's attrs %v, want lajstrom.origin.url and no lajstrom.origin.checksum", v.Attrs)
	}
	fetches, err := e.st.ListAudit(ctx, index.AuditFilter{Action: "proxy.fetch"})
	must(t, err)
	if len(fetches) != 1 || !strings.Contains(string(fetches[0].Details), `"checksum":""`) {
		t.Errorf("proxy.fetch rows: %+v, want one with the checksum \"\"", fetches)
	}

	forged := "com/example/lib-core/1.2.0/lib-core-1.2.0.pom.asc"
	e.up.put(forged, body, strings.Repeat("ab", 20))
	if rr := get(e.h, "central", forged); rr.Code != http.StatusBadGateway {
		t.Errorf("a signature that fails its .sha1: %d %s, want 502", rr.Code, rr.Body)
	}
	if rr := get(e.h, "central", "com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar.asc"); rr.Code != http.StatusNotFound {
		t.Errorf("a signature the upstream does not have: %d %s, want 404", rr.Code, rr.Body)
	}

	small := newProxyEnv(t, "")
	small.repo.Policy = json.RawMessage(`{"maxFileSize": 16}`)
	must(t, small.st.UpdateRepo(ctx, &small.repo))
	small.up.put(sig, body, "-")
	if rr := get(small.h, "central", sig); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "maxFileSize") {
		t.Errorf("a signature without a .sha1 past maxFileSize: %d %s, want 403 naming the bound", rr.Code, rr.Body)
	}
}

// TestProxySnapshot: a snapshot proxy lists the SNAPSHOT versions alone, serves the document of
// a SNAPSHOT version as the upstream has it, and serves the non-unique name of a file as the file
// of the build that document names for its classifier and extension, fetched as that build's.
func TestProxySnapshot(t *testing.T) {
	e := newProxyEnv(t, PolicySnapshot)
	build := "1.0-20261114.101112-3"
	jar, ji := testutil.Jar(testutil.JarOptions{Version: build})
	path := "com/example/lib-core/1.0-SNAPSHOT/" + ji.Filename
	e.up.put(path, jar, "")
	e.up.put(metadataPath, upstreamDoc("1.0-SNAPSHOT", "0.9", "0.9", "1.0-SNAPSHOT"), "")
	snapshotPath := "com/example/lib-core/1.0-SNAPSHOT/maven-metadata.xml"
	e.up.put(snapshotPath, []byte(`<metadata modelVersion="1.1.0"><groupId>com.example</groupId><artifactId>lib-core</artifactId>`+
		`<version>1.0-SNAPSHOT</version><versioning><snapshot><timestamp>20261114.101112</timestamp><buildNumber>3</buildNumber></snapshot>`+
		`<lastUpdated>20261114101112</lastUpdated><snapshotVersions><snapshotVersion><extension>jar</extension><value>`+build+`</value>`+
		`<updated>20261114101112</updated></snapshotVersion></snapshotVersions></versioning></metadata>`), "")

	rr := get(e.h, "central", metadataPath)
	doc, err := readRepoMetadata(rr.Body.Bytes())
	must(t, err)
	if v := doc.Versioning; rr.Code != http.StatusOK || !slices.Equal(v.Versions, []string{"1.0-SNAPSHOT"}) || v.Release != "" {
		t.Errorf("the metadata of a snapshot proxy: %d %s", rr.Code, rr.Body)
	}
	rr = get(e.h, "central", snapshotPath)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "<value>"+build+"</value>") {
		t.Errorf("the metadata of the SNAPSHOT version: %d %s", rr.Code, rr.Body)
	}
	served(t, get(e.h, "central", "com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-SNAPSHOT.jar"), jar, "the non-unique name")
	served(t, get(e.h, "central", path), jar, "the build's file")
	served(t, get(e.h, "central", "com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-SNAPSHOT.jar.sha1"), []byte(sumsOf(jar)[SHA1]),
		"the sha1 of the non-unique name")
	expect(t, "file requests", e.up.gets.Load(), int32(1))
}

// TestProxyUpstreamDown: when the upstream fails, the metadata row is served however old, and
// without a row the document is what the repo holds.
func TestProxyUpstreamDown(t *testing.T) {
	e := newProxyEnv(t, "")
	jar, ji := testutil.Jar(testutil.JarOptions{})
	e.up.put(ji.Path, jar, "")
	served(t, get(e.h, "central", ji.Path), jar, "the jar")
	e.up.fail.Store(true)
	rr := get(e.h, "central", metadataPath)
	doc, err := readRepoMetadata(rr.Body.Bytes())
	if err != nil || rr.Code != http.StatusOK || doc.Versioning == nil || !slices.Equal(doc.Versioning.Versions, []string{"1.2.0"}) {
		t.Errorf("the metadata of the versions held, the upstream down: %d %s", rr.Code, rr.Body)
	}
	served(t, get(e.h, "central", ji.Path), jar, "the jar held, the upstream down")
}

// expect fails the test unless got is want.
func expect[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}
