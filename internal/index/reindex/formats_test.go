package reindex_test

import (
	"bytes"
	"crypto/md5" //nolint:gosec // twine and maven send md5 checksums; not a security use
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // maven sends sha1 checksums; not a security use
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/deb"
	"github.com/lajstrom/lajstrom/internal/format/maven"
	"github.com/lajstrom/lajstrom/internal/format/pypi"
	"github.com/lajstrom/lajstrom/internal/format/rpm"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// formatCase is one format of the rebuild tests: a representative set of its packages published
// through its frontend and ingest, and what its clients read back.
type formatCase struct {
	plugin format.Plugin
	// publish publishes the set through f and deletes a version of it.
	publish func(t *testing.T, f *frontend)
	// versions is the number of versions the set leaves.
	versions int
	// docs are paths below /<format>/<repo>/ whose bodies the rebuild keeps: index documents and
	// files, which are found through the rows; a space and a media type after the path is the
	// Accept of the GET. signed are the signed documents, whose signature and Date differ at
	// each render: served before and after, not compared.
	docs, signed []string
	// listed returns more paths of docs, read from the documents the frontend serves: the
	// repodata of rpm, named by their checksums. Nil when the format has none.
	listed func(t *testing.T, f *frontend) []string
	// cached are the index_cache rows the GETs of docs and signed fill, as scope and kind.
	cached [][2]string
}

// formatCases are the formats of TestRebuildFormats and TestIntegrationFormats.
var formatCases = map[string]formatCase{
	"apt": {
		plugin: deb.Plugin{}, publish: publishApt, versions: 5,
		docs: []string{"dists/jammy/main/binary-amd64/Packages", "dists/jammy/main/binary-arm64/Packages",
			"dists/jammy/contrib/binary-amd64/Packages.gz", "dists/jammy-updates/main/binary-amd64/Packages.xz",
			"pool/main/h/hello/hello_1.1.0-1_arm64.deb"},
		signed: []string{"dists/jammy/InRelease", "dists/jammy-updates/Release.gpg"},
		cached: [][2]string{{"jammy", "Release"}, {"jammy", "InRelease"}, {"jammy", "main/binary-arm64/Packages"},
			{"jammy-updates", "Release.gpg"}},
	},
	"pypi": {
		plugin: pypi.Plugin{}, publish: publishPypi, versions: 8,
		docs: []string{"simple/", "simple/my-pkg/", "simple/other-lib/", "simple/my-pkg/ application/vnd.pypi.simple.v1+json",
			"files/my-pkg/1.1.0/my_pkg-1.1.0-py3-none-any.whl.metadata", "files/other-lib/0.2/other_lib-0.2-py3-none-any.whl",
			"files/my-pkg/1.1.0/my_pkg-1.1.0-1-py3-none-any.whl", "files/other-lib/0.3/other_lib-0.3-1-py3-none-any.whl.metadata",
			"simple/other-lib/ application/vnd.pypi.simple.v1+json"},
		cached: [][2]string{{"simple", "simple.html"}, {"simple/my-pkg", "simple.json"}, {"simple/other-lib", "simple.html"}},
	},
	"maven": {
		plugin: maven.Plugin{}, publish: publishMaven, versions: 4,
		docs: []string{"com/example/lib-core/maven-metadata.xml", "com/example/lib-core/maven-metadata.xml.sha512",
			"com/example/lib-parent/maven-metadata.xml", "com/example/lib-api/maven-metadata.xml",
			"com/example/lib-core/1.3.0/lib-core-1.3.0.jar.sha256", "com/example/lib-core/1.3.0/lib-core-1.3.0.jar.asc.sha1",
			"com/example/lib-api/2.0.0/lib-api-2.0.0.pom.md5", "com/example/lib-core/1.3.0/lib-core-1.3.0-sources.jar"},
		cached: [][2]string{{"com.example/lib-core", "maven-metadata"}, {"com.example/lib-core", "maven-metadata.sha512"},
			{"com.example/lib-api", "maven-metadata"}},
	},
	"rpm": {
		plugin: rpm.Plugin{}, publish: publishRpm, versions: 4,
		docs: []string{"Packages/hello-1.1.0-1.el9.x86_64.rpm", "Packages/hello-1.1.0-1.el9.src.rpm",
			"Packages/hello-doc-2.0-1.noarch.rpm"},
		listed: rpmRepodata,
		cached: [][2]string{{"repodata", "repomd.xml"}},
	},
}

// TestRebuildFormats is I-01 for apt, pypi, maven and rpm on the fake registry: a representative set
// of each format published through its frontend, the routes over the real ingest pipeline, and
// one version as oras push and the facade do, a version deleted through the write path; then
// the derived rows dropped and rebuilt by a run, equal in every column but the ids and
// indexed_at, and the documents the clients read the same bytes. A second run empties
// index_cache, filled again by the GETs.
func TestRebuildFormats(t *testing.T) {
	for name, c := range formatCases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.useFormat(t, c.plugin, e.providers)
			rebuildFormat(t, e, c)
		})
	}
}

// useFormat replaces the env's repo with an empty repo of p's format, named it-<random hex> so
// that runs do not collide in a registry that keeps what they pushed, and the pipeline and the
// worker with ones over providers and the plugin p.
func (e *env) useFormat(t *testing.T, p format.Plugin, providers provider.Map) {
	t.Helper()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e.repo = e.newRepo(t, p.Name(), "it-"+hex.EncodeToString(suffix))
	log := slog.New(slog.DiscardHandler)
	plugins := func(name string) (format.Plugin, bool) { return p, name == p.Name() }
	e.pipe = ingest.New(ingest.Options{Providers: providers, Index: e.st, Plugins: plugins, Log: log})
	e.w = reindex.New(reindex.Options{Index: e.st, Providers: providers, Inspector: e.pipe, Log: log})
	e.providers = providers
}

// rebuildFormat publishes the set of c in the env's repo, rebuilds its derived rows from the
// registry and compares.
func rebuildFormat(t *testing.T, e *env, c formatCase) {
	t.Helper()
	ctx := t.Context()
	f := e.frontend(t, c.plugin)
	c.publish(t, f)
	docs := f.documents(t, c)
	before := e.derived(t)
	versions := c.versions + f.kept
	if len(before.Versions) != versions {
		t.Fatalf("%d versions published, want %d: %v", len(before.Versions), versions, before.Versions)
	}

	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	r := e.run(t, reindex.Request{Prune: true})
	if r.Added != versions || r.Removed != 0 || r.Degraded != 0 || len(r.Errors) != 0 || len(r.Foreign) != 0 ||
		len(r.Drift) != 0 || len(r.OrphanReferrers) != 0 {
		t.Errorf("the rebuild: %+v", *r)
	}
	after := e.derived(t)
	before.OCIRepos = e.withoutEmptied(before, after)
	same(t, "the rebuilt rows", before, after)
	same(t, "the documents after the rebuild", docs, f.documents(t, c))

	r = e.run(t, reindex.Request{Prune: true})
	if r.Unchanged != versions || r.Added != 0 || r.Removed != 0 || len(r.Errors) != 0 {
		t.Errorf("a second run: %+v", *r)
	}
	for _, k := range c.cached {
		if _, err := e.st.GetIndexCache(ctx, e.repo.ID, k[0], k[1]); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("index_cache %s %s after the run: %v, want ErrNotFound", k[0], k[1], err)
		}
	}
	same(t, "the rows after a second run", before, e.derived(t))
}

// withoutEmptied is before's oci_repos less the repositories that hold no version of before and
// that the rebuild, after, did not find. A repository whose versions were all deleted is found
// again only when the registry's catalog still lists it: the fake's, Zot's and distribution's
// do, Nexus's oci format's and Forgejo's do not, and the row would go after
// reconcile.graceAge anyway (index-schema.md). Every other repository must be found (#800).
func (e *env) withoutEmptied(before, after derived) []string {
	held, found := map[string]bool{}, map[string]bool{}
	for _, p := range before.Packages {
		held[e.repo.Prefix+"/"+p.NameOCI] = true
	}
	for _, r := range after.OCIRepos {
		found[r] = true
	}
	out := []string{}
	for _, r := range before.OCIRepos {
		if held[r] || found[r] {
			out = append(out, r)
		}
	}
	return out
}

// frontend is the routes of one plugin over the env's index and pipeline, the server's
// keys service as the signer, every request made as alice.
type frontend struct {
	e    *env
	p    format.Plugin
	h    http.Handler
	kept int // versions deleteVersion could not delete
}

func (e *env) frontend(t *testing.T, p format.Plugin) *frontend {
	t.Helper()
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	master, err := keys.ParseMaster(hex.EncodeToString(secret))
	must(t, err)
	log := slog.New(slog.DiscardHandler)
	signer, err := keys.New(keys.Options{Index: e.st, Master: master, Log: log})
	must(t, err)
	deps := format.Deps{Index: e.st, Ingest: e.pipe, Sessions: e.pipe, Providers: e.providers, Signer: signer, Log: log}
	mux := http.NewServeMux()
	p.Routes(format.NewRouter(mux, p.Name(), deps), deps)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), alice)))
	})
	return &frontend{e: e, p: p, h: h}
}

// do sends a request to /<format>/<repo>/<p> with the headers hdr, names and values, an empty
// value for none, and fails the test unless the answer is want.
func (f *frontend) do(t *testing.T, method, p string, body []byte, want int, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/"+f.p.Name()+"/"+f.e.repo.Name+"/"+p, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] != "" {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	if rr.Code != want {
		t.Fatalf("%s %s: %d %s, want %d", method, p, rr.Code, rr.Body, want)
	}
	return rr
}

// documents GETs the docs of c, which fills index_cache, and returns their bodies by path; it
// GETs the signed documents too and checks that they are there.
func (f *frontend) documents(t *testing.T, c formatCase) map[string]string {
	t.Helper()
	out := map[string]string{}
	docs := c.docs
	if c.listed != nil {
		docs = append(slices.Clone(docs), c.listed(t, f)...)
	}
	for _, doc := range docs {
		p, accept, _ := strings.Cut(doc, " ")
		out[doc] = f.do(t, http.MethodGet, p, nil, http.StatusOK, "Accept", accept).Body.String()
	}
	for _, p := range c.signed {
		if body := f.do(t, http.MethodGet, p, nil, http.StatusOK).Body.String(); !strings.Contains(body, "-----BEGIN PGP") {
			t.Errorf("%s is not signed:\n%s", p, body)
		}
	}
	for _, k := range c.cached {
		if _, err := f.e.st.GetIndexCache(t.Context(), f.e.repo.ID, k[0], k[1]); err != nil {
			t.Errorf("index_cache %s %s after the GETs: %v", k[0], k[1], err)
		}
	}
	return out
}

// push stores up as oras push and the facade do: the blobs of the plugin's Build straight to the
// registry, then the manifest through ingest.Ingest, the hook of PUT /v2/…/manifests/<tag>.
func (f *frontend) push(t *testing.T, up format.Upload) {
	t.Helper()
	ctx := t.Context()
	e := f.e
	built, err := f.p.Build(ctx, e.repo, up)
	must(t, err)
	prov, _ := e.providers.Provider(e.repo.Registry)
	rr, err := prov.Repository(ctx, built.RepoPath)
	must(t, err)
	for _, b := range built.Blobs {
		if ok, err := rr.Exists(ctx, b.Descriptor); err == nil && ok {
			continue
		}
		rc, err := b.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, b.Descriptor, rc))
		_ = rc.Close()
	}
	raw, _, err := profile.Encode(built.Manifest)
	must(t, err)
	pkgPath := strings.TrimPrefix(built.RepoPath, e.repo.Format+"/"+e.repo.Name+"/")
	_, err = e.pipe.Ingest(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, alice)
	must(t, err)
}

// deleteVersion deletes a version of the package name through the write path, as DELETE
// /api/v1/repos/…/versions does; on a registry that deletes no manifests the version stays,
// and f.kept counts it.
func (f *frontend) deleteVersion(t *testing.T, name, version, variant string) {
	t.Helper()
	ctx := t.Context()
	e := f.e
	pkg, err := e.st.GetPackage(ctx, e.repo.ID, name)
	must(t, err)
	v, err := e.st.GetVersion(ctx, pkg.ID, version, variant)
	must(t, err)
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: pkg.NameOCI, Reference: v.Digest.String()}, alice)
	if errors.Is(err, provider.ErrUnsupported) {
		t.Logf("the registry deletes no manifests: %s %s stays", name, version)
		f.kept++
		return
	}
	must(t, err)
}

// file is a format.UploadedFile of b named name.
func file(name string, b []byte) format.UploadedFile {
	return format.UploadedFile{Name: name, Size: int64(len(b)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

// publishApt uploads to an apt repo what a small distribution has: hello 1.0.0-1 for amd64 in
// jammy and jammy-updates (one .deb in two suites), 1.1.0-1 for amd64 and arm64, hello-data
// 1:2.0-1 for all in the component contrib, goodbye 1.0-1, and bonjour 0.1-1 for arm64 by oras
// push; then hello 1.0.0-1 goes from jammy and stays in jammy-updates, and goodbye, the only
// version of its package, goes.
func publishApt(t *testing.T, f *frontend) {
	hello, _ := testutil.Deb(testutil.DebOptions{})
	hello11, _ := testutil.Deb(testutil.DebOptions{Version: "1.1.0-1"})
	hello11arm, _ := testutil.Deb(testutil.DebOptions{Version: "1.1.0-1", Arch: "arm64"})
	data, _ := testutil.Deb(testutil.DebOptions{Name: "hello-data", Version: "1:2.0-1", Arch: "all", Depends: "hello (>= 1.0.0)",
		Files: map[string]string{"usr/share/hello-data/greetings": "hello\nbonjour\n"}})
	goodbye, _ := testutil.Deb(testutil.DebOptions{Name: "goodbye", Version: "1.0-1"})
	for _, up := range []struct {
		deb              []byte
		suite, component string
	}{{hello, "jammy", "main"}, {hello, "jammy-updates", "main"}, {hello11, "jammy", "main"}, {hello11arm, "jammy", "main"},
		{data, "jammy", "contrib"}, {goodbye, "jammy", "main"}} {
		f.do(t, http.MethodPut, "upload?suite="+up.suite+"&component="+up.component, up.deb, http.StatusCreated)
	}
	bonjour, _ := testutil.Deb(testutil.DebOptions{Name: "bonjour", Version: "0.1-1", Arch: "arm64"})
	f.push(t, format.Upload{Files: []format.UploadedFile{file("bonjour.deb", bonjour)},
		Params: map[string]string{deb.ParamSuite: "jammy", deb.ParamComponent: "main"}})
	f.deleteVersion(t, "hello", "1.0.0-1", "jammy/main/amd64")
	f.deleteVersion(t, "goodbye", "1.0-1", "jammy/main/amd64")
}

// publishRpm uploads to an rpm repo hello 1.0.0 and 1.1.0 for x86_64, the source package of
// 1.1.0 and hello-doc 1:2.0-1 noarch, pushes bonjour as oras push does, then deletes hello
// 1.0.0.
func publishRpm(t *testing.T, f *frontend) {
	for _, o := range []testutil.RPMOptions{
		{}, {Version: "1.1.0"}, {Version: "1.1.0", Source: true},
		{Name: "hello-doc", Epoch: "1", Version: "2.0", Release: "1", Arch: "noarch"},
	} {
		b, _ := testutil.RPM(o)
		f.do(t, http.MethodPut, "upload", b, http.StatusCreated)
	}
	bonjour, info := testutil.RPM(testutil.RPMOptions{Name: "bonjour", Arch: "aarch64"})
	f.push(t, format.Upload{Files: []format.UploadedFile{file(info.Filename, bonjour)}})
	f.deleteVersion(t, "hello", "1.0.0-1.el9", "x86_64")
}

// rpmRepodata returns the repodata/repomd.xml of the repo and the documents it lists.
func rpmRepodata(t *testing.T, f *frontend) []string {
	t.Helper()
	md := f.do(t, http.MethodGet, "repodata/repomd.xml", nil, http.StatusOK).Body.String()
	var out []string
	for _, m := range regexp.MustCompile(`<location href="(repodata/[^"]+)"/>`).FindAllStringSubmatch(md, -1) {
		out = append(out, m[1])
	}
	if len(out) != 3 {
		t.Errorf("repomd.xml lists %d documents:\n%s", len(out), md)
	}
	return out
}

// publishPypi uploads to a pypi repo as twine does, with the sha256 and md5 of each file:
// my-pkg 1.0.0 as a wheel and an sdist, 1.1.0 as a pure wheel with its dependencies, the same
// with the build tag 1 and a wheel for CPython 3.12 on manylinux, 1.2.0.dev.local, a version that
// is not PEP 440, and Other_Lib 0.1 as an sdist; Other_Lib 0.2 by oras push, and 0.3 with the
// build tag 1 too, in the form of its variant before #726, the tag set alone; then the sdist of
// my-pkg 1.0.0 goes. The upload keeps no signature: a gpg_signature part is a form field it
// ignores.
func publishPypi(t *testing.T, f *frontend) {
	type dist struct {
		b        []byte
		filename string
	}
	var dists []dist
	add := func(b []byte, filename string) { dists = append(dists, dist{b, filename}) }
	w, wi := testutil.Wheel(testutil.WheelOptions{})
	add(w, wi.Filename)
	s, si := testutil.Sdist(testutil.SdistOptions{})
	add(s, si.Filename)
	w, wi = testutil.Wheel(testutil.WheelOptions{Version: "1.1.0", RequiresDist: []string{"requests>=2.31", "idna; extra == 'idna'"}})
	add(w, wi.Filename)
	w, wi = testutil.Wheel(testutil.WheelOptions{Version: "1.1.0", Build: "1", RequiresDist: []string{"requests>=2.32"}})
	add(w, wi.Filename)
	w, wi = testutil.Wheel(testutil.WheelOptions{Version: "1.1.0", PythonTag: "cp312", AbiTag: "cp312",
		PlatformTag: "manylinux_2_17_x86_64.manylinux2014_x86_64"})
	add(w, wi.Filename)
	w, wi = testutil.Wheel(testutil.WheelOptions{Version: "1.2.0.dev.local"})
	add(w, wi.Filename)
	s, si = testutil.Sdist(testutil.SdistOptions{Name: "Other_Lib", Version: "0.1"})
	add(s, si.Filename)
	for _, d := range dists {
		body, ctype := twine(t, d.filename, d.b)
		f.do(t, http.MethodPost, "", body, http.StatusOK, "Content-Type", ctype)
	}
	w, wi = testutil.Wheel(testutil.WheelOptions{Name: "Other_Lib", Version: "0.2"})
	f.push(t, format.Upload{Files: []format.UploadedFile{file(wi.Filename, w)}})
	w, wi = testutil.Wheel(testutil.WheelOptions{Name: "Other_Lib", Version: "0.3", Build: "1"})
	f.push(t, format.Upload{Files: []format.UploadedFile{file(wi.Filename, w)}, Params: map[string]string{pypi.ParamVariant: "py3-none-any"}})
	f.deleteVersion(t, "my-pkg", "1.0.0", "sdist")
}

// twine returns the multipart form twine sends for the distribution b and its Content-Type.
func twine(t *testing.T, filename string, b []byte) ([]byte, string) {
	t.Helper()
	s, m := sha256.Sum256(b), md5.Sum(b) //nolint:gosec // see the import
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, kv := range [][2]string{{":action", "file_upload"}, {"protocol_version", "1"}, {"metadata_version", "2.1"},
		{"sha256_digest", hex.EncodeToString(s[:])}, {"md5_digest", hex.EncodeToString(m[:])}} {
		must(t, mw.WriteField(kv[0], kv[1]))
	}
	fw, err := mw.CreateFormFile("content", filename)
	must(t, err)
	_, err = fw.Write(b)
	must(t, err)
	must(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

// publishMaven deploys to a maven repo as mvn and Gradle do, every file followed by its
// checksums and the version committed by maven-metadata.xml: com.example:lib-core 1.2.0 (jar
// and POM), 1.3.0 (the jar, sources, javadoc, a POM with dependencies, OpenPGP signatures of the
// jar and the POM, the four checksums of Gradle) and the pre-release 1.4.0-M1 (jar and POM with
// their signatures), and com.example:lib-parent 1.0.0, a pom-packaging project, the POM alone;
// com.example:lib-api 2.0.0 by oras push; then lib-core 1.2.0 goes.
func publishMaven(t *testing.T, f *frontend) {
	deps := []testutil.DependencyOptions{{GroupID: "com.example", ArtifactID: "lib-api", Version: "2.0.0"},
		{GroupID: "org.junit.jupiter", ArtifactID: "junit-jupiter", Version: "5.11.3", Scope: "test"}}
	sig := []byte("-----BEGIN PGP SIGNATURE-----\n\niQ\n-----END PGP SIGNATURE-----\n")
	var versions []string
	for _, v := range []struct {
		version     string
		classifiers []string
		signed      bool
		checksums   []string
	}{
		{"1.2.0", []string{""}, false, []string{maven.SHA1, maven.MD5}},
		{"1.3.0", []string{"", "sources", "javadoc"}, true, []string{maven.MD5, maven.SHA1, maven.SHA256, maven.SHA512}},
		{"1.4.0-M1", []string{""}, true, []string{maven.SHA1, maven.MD5}},
	} {
		var files [][2][]byte
		for _, c := range v.classifiers {
			b, info := testutil.Jar(testutil.JarOptions{Version: v.version, Classifier: c})
			files = append(files, [2][]byte{[]byte(info.Path), b})
			if c == "" && v.signed {
				files = append(files, [2][]byte{[]byte(info.Path + ".asc"), sig})
			}
		}
		pom, info := testutil.POM(testutil.POMOptions{Version: v.version, Dependencies: deps})
		files = append(files, [2][]byte{[]byte(info.Path), pom})
		if v.signed {
			files = append(files, [2][]byte{[]byte(info.Path + ".asc"), sig})
		}
		versions = append(versions, v.version)
		f.deploy(t, "lib-core", files, versions, v.checksums)
	}
	pom, info := testutil.POM(testutil.POMOptions{ArtifactID: "lib-parent", Version: "1.0.0", Packaging: "pom"})
	f.deploy(t, "lib-parent", [][2][]byte{{[]byte(info.Path), pom}}, []string{"1.0.0"}, []string{maven.SHA1, maven.MD5})

	jar, ji := testutil.Jar(testutil.JarOptions{ArtifactID: "lib-api", Version: "2.0.0"})
	pom, pi := testutil.POM(testutil.POMOptions{ArtifactID: "lib-api", Version: "2.0.0"})
	f.push(t, format.Upload{Files: []format.UploadedFile{file(ji.Path, jar), file(pi.Path, pom)}})
	f.deleteVersion(t, "com.example:lib-core", "1.2.0", "")
}

// deploy PUTs the files of one version of com.example:<artifact>, each followed by its checksum
// files of algs, then the artifact's maven-metadata.xml, which lists versions, the last the one
// deployed, and its checksums.
func (f *frontend) deploy(t *testing.T, artifact string, files [][2][]byte, versions, algs []string) {
	t.Helper()
	for _, file := range files {
		p := string(file[0])
		f.do(t, http.MethodPut, p, file[1], http.StatusCreated)
		for _, a := range algs {
			f.do(t, http.MethodPut, p+"."+a, []byte(checksum(a, file[1])), http.StatusCreated)
		}
	}
	last := versions[len(versions)-1]
	md := fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<metadata modelVersion="1.1.0"><groupId>com.example</groupId>`+
		`<artifactId>%s</artifactId><versioning><latest>%s</latest><release>%s</release><versions><version>%s</version></versions>`+
		`<lastUpdated>%s</lastUpdated></versioning></metadata>`, artifact, last, last, strings.Join(versions, "</version><version>"),
		time.Now().UTC().Format("20060102150405"))
	p := "com/example/" + artifact + "/maven-metadata.xml"
	f.do(t, http.MethodPut, p, md, http.StatusCreated)
	for _, a := range algs {
		f.do(t, http.MethodPut, p+"."+a, []byte(checksum(a, md)), http.StatusCreated)
	}
}

// checksum is the lowercase hex digest of b by the maven algorithm a.
func checksum(a string, b []byte) string {
	switch a {
	case maven.MD5:
		return fmt.Sprintf("%x", md5.Sum(b)) //nolint:gosec // see the import
	case maven.SHA1:
		return fmt.Sprintf("%x", sha1.Sum(b)) //nolint:gosec // see the import
	case maven.SHA256:
		return fmt.Sprintf("%x", sha256.Sum256(b))
	}
	return fmt.Sprintf("%x", sha512.Sum512(b))
}
