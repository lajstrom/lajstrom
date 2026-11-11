package maven

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// deployClock is a settable time for the sessions of the deploy tests.
type deployClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *deployClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *deployClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// deployDBs numbers the in-memory databases of the deploy tests, so that every env gets its own.
var deployDBs atomic.Int64

// deployEnv is the maven frontend over SQLite, the fake registry and the real ingest pipeline,
// whose sessions last a minute.
type deployEnv struct {
	t     *testing.T
	st    *sqlite.Store
	fake  *providertest.Fake
	repo  index.Repo
	clock *deployClock
	h     http.Handler
}

func newDeployEnv(t *testing.T, authz auth.Authorizer) *deployEnv {
	t.Helper()
	return newDeployEnvOver(t, authz, nil)
}

// newDeployEnvOver is newDeployEnv with registry(fake) as the registry of the pipeline, the fake
// when registry is nil.
func newDeployEnvOver(t *testing.T, authz auth.Authorizer, registry func(*providertest.Fake) provider.Provider) *deployEnv {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:maven-deploy-%d?mode=memory&cache=shared", deployDBs.Add(1)))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &deployEnv{t: t, st: st, fake: &providertest.Fake{RegistryName: "local"},
		repo:  index.Repo{Format: Name, Name: "releases", Registry: "local", Kind: "hosted"},
		clock: &deployClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}}
	must(t, st.CreateRepo(ctx, &e.repo))
	log := slog.New(slog.DiscardHandler)
	var reg provider.Provider = e.fake
	if registry != nil {
		reg = registry(e.fake)
	}
	providers := provider.Map{"local": e.fake}
	pipe := ingest.New(ingest.Options{Providers: provider.Map{"local": reg}, Index: st, Log: log, Now: e.clock.now, SessionTimeout: time.Minute,
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	deps := format.Deps{Index: st, Ingest: pipe, Sessions: pipe, Providers: providers, Auth: authz, Log: log}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get("X-Test-User"); user != "" {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindUser, Name: user}))
		}
		mux.ServeHTTP(w, r)
	})
	return e
}

// put PUTs body to /maven/releases/<path> as user ("": anonymous) and returns the answer.
func (e *deployEnv) put(user, path string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/maven/releases/"+path, bytes.NewReader(body))
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// wantPut PUTs as alice and fails the test unless the status is want.
func (e *deployEnv) wantPut(path string, body []byte, want int) {
	e.t.Helper()
	if rr := e.put("alice", path, body); rr.Code != want {
		e.t.Errorf("PUT %s: %d, want %d: %s", path, rr.Code, want, rr.Body)
	}
}

// versions are the versions of com.example:lib-core in the index.
func (e *deployEnv) versions() []index.Version {
	e.t.Helper()
	pkg, err := e.st.GetPackage(e.t.Context(), e.repo.ID, "com.example:lib-core")
	if err != nil {
		return nil
	}
	vs, err := e.st.ListVersions(e.t.Context(), pkg.ID)
	must(e.t, err)
	return vs
}

// deployFile is a file of a deploy: its path below the repo and its bytes.
type deployFile struct {
	path string
	body []byte
}

// release is the files of com.example:lib-core at version: the jar, the sources and javadoc
// jars and the POM; seed changes the bytes of the jars.
func release(version, seed string) []deployFile {
	var files []deployFile
	for _, classifier := range []string{"", "sources", "javadoc"} {
		b, info := testutil.Jar(testutil.JarOptions{Version: version, Classifier: classifier})
		files = append(files, deployFile{info.Path, append(b, seed...)})
	}
	pom, info := testutil.POM(testutil.POMOptions{Version: version})
	return append(files, deployFile{info.Path, pom})
}

// metadataDoc is the maven-metadata.xml a client PUTs after the files of release, listing
// versions with the last as the release.
func metadataDoc(versions ...string) []byte {
	var v strings.Builder
	for _, s := range versions {
		v.WriteString("<version>" + s + "</version>")
	}
	last := versions[len(versions)-1]
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://maven.apache.org/METADATA/1.1.0" modelVersion="1.1.0"><groupId>com.example</groupId><artifactId>lib-core</artifactId>` +
		`<versioning><latest>` + last + `</latest><release>` + last + `</release><versions>` + v.String() + `</versions><lastUpdated>20261109120000</lastUpdated></versioning></metadata>`)
}

const metadataPath = "com/example/lib-core/maven-metadata.xml"

// deploy PUTs the files the way the client does: mvn sends each file and its .sha1 and .md5,
// Gradle each file and its four checksums; then the metadata and its checksums. It returns the
// status of the metadata PUT.
func (e *deployEnv) deploy(client string, files []deployFile, md []byte) int {
	e.t.Helper()
	algs := []string{SHA1, MD5}
	if client == "gradle" {
		algs = []string{MD5, SHA1, SHA256, SHA512}
	}
	for _, f := range files {
		e.wantPut(f.path, f.body, http.StatusCreated)
		for _, a := range algs {
			e.wantPut(f.path+"."+a, []byte(sumsOf(f.body)[a]), http.StatusCreated)
		}
	}
	rr := e.put("alice", metadataPath, md)
	for _, a := range algs {
		e.wantPut(metadataPath+"."+a, []byte(sumsOf(md)[a]), http.StatusCreated)
	}
	return rr.Code
}

// TestDeploy: a deploy of a jar, its sources and javadoc and the POM, as mvn and as Gradle send
// them, is one version with four files when maven-metadata.xml commits it (201), uploaded by
// alice; the same files again are 200, other bytes under the version 409, after which the first
// bytes are 200 again.
func TestDeploy(t *testing.T) {
	for _, client := range []string{"mvn", "gradle"} {
		t.Run(client, func(t *testing.T) {
			e := newDeployEnv(t, nil)
			if got := e.deploy(client, release("1.2.0", ""), metadataDoc("1.2.0")); got != http.StatusCreated {
				t.Fatalf("the metadata PUT: %d, want 201", got)
			}
			vs := e.versions()
			if len(vs) != 1 || vs[0].Version != "1.2.0" {
				t.Fatalf("versions %+v, want 1.2.0", vs)
			}
			fs, err := e.st.ListFiles(t.Context(), vs[0].ID)
			must(t, err)
			if len(fs) != 4 {
				t.Errorf("%d files, want the jar, sources, javadoc and the POM: %+v", len(fs), fs)
			}
			m := e.manifest(vs[0])
			if m.Annotations[profile.AnnotationUploadedBy] != "user:alice" {
				t.Errorf("annotations %v, want uploaded by user:alice", m.Annotations)
			}
			if got := e.deploy(client, release("1.2.0", ""), metadataDoc("1.2.0")); got != http.StatusOK {
				t.Errorf("the same deploy again: %d, want 200", got)
			}
			if got := e.deploy(client, release("1.2.0", "changed"), metadataDoc("1.2.0")); got != http.StatusConflict {
				t.Errorf("other bytes under 1.2.0: %d, want 409", got)
			}
			if got := e.deploy(client, release("1.2.0", ""), metadataDoc("1.2.0")); got != http.StatusOK {
				t.Errorf("the first bytes again after the conflict: %d, want 200", got)
			}
			if got := e.deploy(client, release("1.3.0", ""), metadataDoc("1.2.0", "1.3.0")); got != http.StatusCreated {
				t.Errorf("a second version: %d, want 201", got)
			}
			if n := len(e.versions()); n != 2 {
				t.Errorf("%d versions, want 2", n)
			}
		})
	}
}

// manifest returns the manifest of version v from the fake registry.
func (e *deployEnv) manifest(v index.Version) ocispec.Manifest {
	e.t.Helper()
	ctx := e.t.Context()
	rr, err := e.fake.Repository(ctx, "maven/releases/com.example/lib-core")
	must(e.t, err)
	desc, err := rr.Resolve(ctx, v.Tag)
	must(e.t, err)
	rc, err := rr.Fetch(ctx, desc)
	must(e.t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	must(e.t, err)
	var m ocispec.Manifest
	must(e.t, json.Unmarshal(b, &m))
	return m
}

// TestDeployChecksums: a checksum file is checked against the file it names; a wrong one, one
// that is not hexadecimal, and one before its file are 400; a checksum of maven-metadata.xml is
// taken as it is.
func TestDeployChecksums(t *testing.T) {
	e := newDeployEnv(t, nil)
	jar := release("1.2.0", "")[0]
	e.wantPut(jar.path+".sha1", []byte(sumsOf(jar.body)[SHA1]), http.StatusBadRequest) // before the file
	e.wantPut(jar.path, jar.body, http.StatusCreated)
	e.wantPut(jar.path+".sha1", []byte(sumsOf(jar.body)[SHA1]+"  "+jar.path), http.StatusCreated)
	e.wantPut(jar.path+".md5", []byte(strings.ToUpper(sumsOf(jar.body)[MD5])), http.StatusCreated)
	e.wantPut(jar.path+".sha256", []byte(sumsOf([]byte("other"))[SHA256]), http.StatusBadRequest)
	e.wantPut(jar.path+".sha512", []byte("not a digest"), http.StatusBadRequest)
	e.wantPut(metadataPath+".sha1", []byte(sumsOf([]byte("any"))[SHA1]), http.StatusCreated)
	if rr := e.put("alice", jar.path+".sha256", []byte(sumsOf([]byte("other"))[SHA256])); !strings.Contains(rr.Body.String(), "the sha256 of "+jar.path) {
		t.Errorf("the answer to a wrong checksum does not name it: %s", rr.Body)
	}
}

// TestDeployChecksumRefused: a jar whose bytes differ from what the client sent, its checksums
// 400, as mvn sends it and goes on: the rest of the release and maven-metadata.xml, which is 400
// naming the jar, and nothing is published. The jar uploaded again with its checksums, the same
// maven-metadata.xml publishes the release.
func TestDeployChecksumRefused(t *testing.T) {
	e := newDeployEnv(t, nil)
	files := release("1.2.0", "")
	jar := files[0]
	e.wantPut(jar.path, append(append([]byte{}, jar.body...), "changed on the way"...), http.StatusCreated)
	for _, a := range []string{SHA1, MD5} {
		rr := e.put("alice", jar.path+"."+a, []byte(sumsOf(jar.body)[a]))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "the deploy does not publish until "+jar.path+" is uploaded again") {
			t.Errorf("PUT %s.%s: %d %s, want 400 saying the deploy does not publish", jar.path, a, rr.Code, rr.Body)
		}
	}
	for _, f := range files[1:] {
		e.wantPut(f.path, f.body, http.StatusCreated)
		e.wantPut(f.path+".sha1", []byte(sumsOf(f.body)[SHA1]), http.StatusCreated)
	}
	pushed := len(e.fake.PushReferences())
	rr := e.put("alice", metadataPath, metadataDoc("1.2.0"))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "holds "+jar.path+", which was refused: the md5 of "+jar.path) {
		t.Errorf("maven-metadata.xml: %d %s, want 400 naming the refused jar", rr.Code, rr.Body)
	}
	if vs := e.versions(); len(vs) != 0 || len(e.fake.PushReferences()) != pushed {
		t.Fatalf("versions %+v, %d manifests pushed; want nothing published", vs, len(e.fake.PushReferences())-pushed)
	}
	e.wantPut(jar.path, jar.body, http.StatusCreated)
	e.wantPut(jar.path+".sha1", []byte(sumsOf(jar.body)[SHA1]), http.StatusCreated)
	if got := e.put("alice", metadataPath, metadataDoc("1.2.0")).Code; got != http.StatusCreated {
		t.Fatalf("maven-metadata.xml after the jar again: %d, want 201", got)
	}
	if vs := e.versions(); len(vs) != 1 || len(e.manifest(vs[0]).Layers) != len(files)+1 {
		t.Errorf("versions %+v, want 1.2.0 with %d files", vs, len(files))
	}
}

// TestDeployMetadata: a maven-metadata.xml commits only a version with files in a session of
// its principal; without one it is 200 for a version in the index and 400 otherwise; the
// metadata of a group is taken and not kept; a document that is not one, or of another
// artifact, is 400.
func TestDeployMetadata(t *testing.T) {
	e := newDeployEnv(t, nil)
	e.wantPut(metadataPath, metadataDoc("1.2.0"), http.StatusBadRequest) // no files uploaded
	e.deploy("mvn", release("1.2.0", ""), metadataDoc("1.2.0"))
	e.wantPut(metadataPath, metadataDoc("1.2.0"), http.StatusOK) // the repeat of the commit
	e.wantPut("com/example/maven-metadata.xml", []byte(`<metadata><plugins><plugin><prefix>x</prefix></plugin></plugins></metadata>`), http.StatusCreated)
	e.wantPut(metadataPath, []byte("not xml"), http.StatusBadRequest)
	e.wantPut(metadataPath, bytes.Replace(metadataDoc("1.2.0"), []byte("lib-core"), []byte("other"), 1), http.StatusBadRequest)
	e.wantPut("com/example/lib-core/1.0-SNAPSHOT/lib-core-1.0-20261109.120000-1.jar", []byte("x"), http.StatusBadRequest)
	e.wantPut("com/example/lib-core/1.0-SNAPSHOT/maven-metadata.xml", metadataDoc("1.0-SNAPSHOT"), http.StatusBadRequest)
}

// TestDeployExpiry: a deploy that stops before maven-metadata.xml publishes nothing; after the
// session's minute its files are gone, and the metadata PUT is 400.
func TestDeployExpiry(t *testing.T) {
	e := newDeployEnv(t, nil)
	files := release("1.2.0", "")
	e.wantPut(files[0].path, files[0].body, http.StatusCreated)
	e.wantPut(files[3].path, files[3].body, http.StatusCreated)
	if vs := e.versions(); len(vs) != 0 {
		t.Fatalf("files without the metadata published %+v", vs)
	}
	e.clock.add(2 * time.Minute)
	e.wantPut(metadataPath, metadataDoc("1.2.0"), http.StatusBadRequest)
	if vs := e.versions(); len(vs) != 0 {
		t.Errorf("an expired session published %+v", vs)
	}
}

// TestDeployPrincipals: a version another principal is deploying is 409 to bob, whose
// maven-metadata.xml commits nothing of alice's; anonymous is 401 and a user who may not write
// 404 or 403, by the rule of rbac.md §3.
func TestDeployPrincipals(t *testing.T) {
	e := newDeployEnv(t, nil)
	jar := release("1.2.0", "")[0]
	e.wantPut(jar.path, jar.body, http.StatusCreated)
	if rr := e.put("bob", jar.path, jar.body); rr.Code != http.StatusConflict {
		t.Errorf("bob's PUT into alice's deploy: %d, want 409", rr.Code)
	}
	if rr := e.put("bob", metadataPath, metadataDoc("1.2.0")); rr.Code != http.StatusBadRequest {
		t.Errorf("bob's metadata: %d, want 400, committing nothing", rr.Code)
	}

	z := newDeployEnv(t, readers{"carol": true})
	for _, tt := range []struct {
		user string
		want int
	}{{"", http.StatusUnauthorized}, {"dave", http.StatusNotFound}, {"carol", http.StatusForbidden}} {
		if rr := z.put(tt.user, jar.path, jar.body); rr.Code != tt.want {
			t.Errorf("PUT as %q: %d, want %d", tt.user, rr.Code, tt.want)
		}
	}
}

// TestDeploySessionFiles: a publication session stores at most 256 files by default
// (http-routing.md §3, #246); the next file of the version is 400 naming the bound and the key
// that raises it, before it is stored, and a file the session holds may come again.
func TestDeploySessionFiles(t *testing.T) {
	e := newDeployEnv(t, nil)
	jar := release("1.2.0", "")[0]
	dir := jar.path[:strings.LastIndex(jar.path, "/")+1]
	for i := range 256 {
		e.wantPut(fmt.Sprintf("%slib-core-1.2.0-part%03d.bin", dir, i), []byte{byte(i)}, http.StatusCreated)
	}
	rr := e.put("alice", jar.path, jar.body)
	if body := rr.Body.String(); rr.Code != http.StatusBadRequest || !strings.Contains(body, "holds 256 files, the bound policy.maven.maxSessionFiles of maven/releases") {
		t.Errorf("the 257th file: %d %s, want 400 naming the bound and its key", rr.Code, body)
	}
	e.wantPut(dir+"lib-core-1.2.0-part000.bin", []byte("again"), http.StatusCreated)
}

// TestDeploySessionFilesCounted: with maven.maxSessionFiles 8, a Gradle deploy of four files, a
// .module, a signature, a signature of the signature and a .sigstore.json fills the session,
// although every one of them has four checksum files: checksums, of a signature or a .module
// too, are not stored and do not count, and neither does maven-metadata.xml, which commits the
// eight. The bound is a session's, not the version's: the same deploy again is 200.
func TestDeploySessionFilesCounted(t *testing.T) {
	e := newDeployEnv(t, nil)
	e.repo.Policy = json.RawMessage(`{"maven":{"maxSessionFiles":8}}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.repo))
	files := release("1.2.0", "")
	dir := files[0].path[:strings.LastIndex(files[0].path, "/")+1]
	files = append(files,
		deployFile{dir + "lib-core-1.2.0.module", []byte(`{"formatVersion":"1.1"}`)},
		deployFile{dir + "lib-core-1.2.0.jar.asc", []byte("-----BEGIN PGP SIGNATURE-----")},
		deployFile{dir + "lib-core-1.2.0.jar.asc.asc", []byte("-----BEGIN PGP SIGNATURE----- again")},
		deployFile{dir + "lib-core-1.2.0.jar.sigstore.json", []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)})
	putAll := func() {
		t.Helper()
		for _, f := range files {
			e.wantPut(f.path, f.body, http.StatusCreated)
			for _, a := range []string{MD5, SHA1, SHA256, SHA512} {
				e.wantPut(f.path+"."+a, []byte(sumsOf(f.body)[a]), http.StatusCreated)
			}
		}
	}
	putAll()
	rr := e.put("alice", dir+"lib-core-1.2.0-extra.jar", []byte("one too many"))
	if body := rr.Body.String(); rr.Code != http.StatusBadRequest || !strings.Contains(body, "holds 8 files, the bound policy.maven.maxSessionFiles") {
		t.Errorf("the ninth file: %d %s, want 400 naming the bound", rr.Code, body)
	}
	e.wantPut(files[0].path, files[0].body, http.StatusCreated)
	if got := e.put("alice", metadataPath, metadataDoc("1.2.0")).Code; got != http.StatusCreated {
		t.Fatalf("maven-metadata.xml of a full session: %d, want 201", got)
	}
	if vs := e.versions(); len(vs) != 1 || len(e.manifest(vs[0]).Layers) != len(files)+1 {
		t.Errorf("versions %+v, want one of %d files and its metadata", vs, len(files))
	}
	putAll()
	if got := e.put("alice", metadataPath, metadataDoc("1.2.0")).Code; got != http.StatusOK {
		t.Errorf("the same deploy again: %d, want 200", got)
	}
}

// TestDeploySpelling: after com.example:lib-core 1.2.0, a file of Com.Example:lib-core, whose
// package path is the same, is 409 naming com.example:lib-core, under the same version and
// under another; it is not stored and opens no session (#244).
func TestDeploySpelling(t *testing.T) {
	e := newDeployEnv(t, nil)
	if got := e.deploy("mvn", release("1.2.0", ""), metadataDoc("1.2.0")); got != http.StatusCreated {
		t.Fatalf("the deploy of com.example:lib-core: %d, want 201", got)
	}
	pushes := len(e.fake.Pushes())
	for _, version := range []string{"1.2.0", "1.3.0"} {
		b, info := testutil.Jar(testutil.JarOptions{GroupID: "Com.Example", Version: version})
		rr := e.put("alice", info.Path, b)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "Com.Example:lib-core exists as com.example:lib-core") {
			t.Errorf("PUT %s: %d %s, want 409 naming com.example:lib-core", info.Path, rr.Code, rr.Body)
		}
		if _, err := e.st.GetSession(t.Context(), e.repo.ID, "Com.Example:lib-core:"+version); err == nil {
			t.Errorf("a session of Com.Example:lib-core %s was opened", version)
		}
	}
	if n := len(e.fake.Pushes()) - pushes; n != 0 {
		t.Errorf("%d blobs of the refused spelling pushed", n)
	}
}

// readers lets the users it lists read every repo, and nobody write.
type readers map[string]bool

func (z readers) Allowed(_ context.Context, p auth.Principal, a auth.Action, _, _ string) (bool, error) {
	return a == auth.ActionRead && z[p.Name], nil
}

// TestDeployInterrupted: the jar of a deploy of 1.2.0 that stopped is not published by the
// maven-metadata.xml of the deploy of 1.3.0, which lists 1.2.0 too: only the version the
// document is for commits.
func TestDeployInterrupted(t *testing.T) {
	e := newDeployEnv(t, nil)
	jar := release("1.2.0", "")[0]
	e.wantPut(jar.path, jar.body, http.StatusCreated)
	if got := e.deploy("mvn", release("1.3.0", ""), metadataDoc("1.2.0", "1.3.0")); got != http.StatusCreated {
		t.Fatalf("the deploy of 1.3.0: %d, want 201", got)
	}
	vs := e.versions()
	if len(vs) != 1 || vs[0].Version != "1.3.0" {
		t.Errorf("versions %+v, want 1.3.0 alone", vs)
	}
}
