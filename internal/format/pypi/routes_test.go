package pypi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

var update = flag.Bool("update", false, "rewrite the golden files of testdata/")

// golden compares got with testdata/name, or writes it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		must(t, os.MkdirAll("testdata", 0o750))
		must(t, os.WriteFile(p, got, 0o600))
		return
	}
	want, err := os.ReadFile(p) //nolint:gosec // a golden file of this package's testdata
	must(t, err)
	if string(want) != string(got) {
		t.Errorf("%s differs from the golden file (go test -run %s -update to rewrite):\n%s", name, t.Name(), got)
	}
}

// TestRenderGolden renders the project list and a project page from index rows written with
// fixed times: two versions, a wheel per tag set and an sdist, one version without metadata in
// the index (no core-metadata, no requires-python), as HTML and JSON.
func TestRenderGolden(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:pypi-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	r := index.Repo{Format: Name, Name: "internal", Registry: "local", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &r))
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	core := "Metadata-Version: 2.1\nName: My_Pkg\nVersion: 1.0\nRequires-Python: >=3.9\n"
	md, _ := json.Marshal(map[string]any{"name": "My_Pkg", "version": "1.0", "requires_python": ">=3.9", FieldCoreMetadata: core})
	for i, row := range []struct {
		version, variant, filename string
		metadata                   []byte
	}{
		{"1.0", "py3-none-any", "my_pkg-1.0-py3-none-any.whl", md},
		{"1.0", "cp311-cp311-manylinux_2_17_x86_64.manylinux2014_x86_64", "my_pkg-1.0-cp311-cp311-manylinux_2_17_x86_64.manylinux2014_x86_64.whl", md},
		{"1.0", VariantSdist, "my_pkg-1.0.tar.gz", md},
		{"2.0rc1", "py3-none-any", "my_pkg-2.0rc1-py3-none-any.whl", nil},
	} {
		rows := ingest.Rows{
			Package: index.Package{RepoID: r.ID, Name: "my-pkg", NameOCI: "my-pkg"},
			Version: index.Version{Version: row.version, Variant: row.variant, Tag: row.filename, Digest: digest.FromString(row.filename),
				ArtifactType: "application/vnd.lajstrom.pypi.v1", Metadata: row.metadata, CreatedAt: at.Add(time.Duration(i) * time.Hour)},
			Files:   []index.File{{Name: row.filename, MediaType: "x", Digest: digest.FromString("file " + row.filename), Size: int64(100 + i)}},
			OCIRepo: "pypi/internal/my-pkg",
		}
		must(t, ingest.WriteRows(ctx, st, &rows, at))
	}
	other := ingest.Rows{Package: index.Package{RepoID: r.ID, Name: "a-tool", NameOCI: "a-tool"},
		Version: index.Version{Version: "0.1", Variant: VariantSdist, Tag: "0.1", Digest: digest.FromString("t"), ArtifactType: "a", CreatedAt: at},
		OCIRepo: "pypi/internal/a-tool"}
	must(t, ingest.WriteRows(ctx, st, &other, at))

	for scope, prefix := range map[format.IndexScope]string{"simple/my-pkg": "project", "simple": "projects"} {
		docs, err := Plugin{}.RenderIndex(ctx, r, scope, st)
		must(t, err)
		if len(docs) != 2 {
			t.Fatalf("%s: %d documents", scope, len(docs))
		}
		for _, d := range docs {
			golden(t, prefix+map[string]string{kindHTML: ".html", kindJSON: ".json"}[d.Kind], d.Content)
		}
	}
	docs, err := Plugin{}.RenderIndex(ctx, r, "simple/nothing-here", st)
	if err != nil || docs != nil {
		t.Errorf("a project that is not there: %v, %v", docs, err)
	}
}

// env is the pypi frontend over the real pipeline, the store and the fake registry.
type env struct {
	t    *testing.T
	st   *sqlite.Store
	fake *providertest.Fake
	pipe *ingest.Pipeline
	repo index.Repo
	h    http.Handler
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith is newEnv with the registry wrap(fake) when wrap is not nil.
func newEnvWith(t *testing.T, wrap func(*providertest.Fake) provider.Provider) *env {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:pypi-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	e := &env{t: t, st: st, repo: index.Repo{Format: Name, Name: "internal", Registry: "local", Prefix: "pypi/internal", Kind: "hosted"}}
	must(t, st.CreateRepo(ctx, &e.repo))
	e.fake = &providertest.Fake{RegistryName: "local"}
	var prov provider.Provider = e.fake
	if wrap != nil {
		prov = wrap(e.fake)
	}
	log := slog.New(slog.DiscardHandler)
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": prov}, Index: st, Log: log,
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	deps := format.Deps{Index: st, Ingest: e.pipe, Providers: provider.Map{"local": prov}, Log: log}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = mux
	return e
}

// publish uploads a distribution through the pipeline.
func (e *env) publish(filename string, b []byte) {
	e.t.Helper()
	if _, err := e.pipe.Upload(e.t.Context(), e.repo, upload(filename, b), auth.Principal{Kind: auth.KindUser, Name: "alice"}); err != nil {
		e.t.Fatalf("upload %s: %v", filename, err)
	}
}

// do sends a request to /pypi/internal/<path>; hdr are header name and value pairs.
func (e *env) do(method, path string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/pypi/internal/"+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// published is an env with two wheels and an sdist of my-pkg 1.0, and their bytes and core
// metadata by file name.
type published struct {
	*env
	bodies, cores map[string][]byte
	pure          string // the file name of the pure wheel
}

func newPublished(t *testing.T) *published {
	t.Helper()
	e := newEnv(t)
	pure, pi := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.0"})
	native, ni := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.0", PythonTag: "cp311", AbiTag: "cp311",
		PlatformTag: "manylinux_2_17_x86_64"})
	tgz, si := testutil.Sdist(testutil.SdistOptions{Name: "My_Pkg", Version: "1.0"})
	e.publish(pi.Filename, pure)
	e.publish(ni.Filename, native)
	e.publish(si.Filename, tgz)
	return &published{env: e, pure: pi.Filename,
		bodies: map[string][]byte{pi.Filename: pure, ni.Filename: native, si.Filename: tgz},
		cores:  map[string][]byte{pi.Filename: pi.Metadata, ni.Filename: ni.Metadata, si.Filename: si.PKGInfo}}
}

// jsonPage is the JSON page of a project as the tests read it.
type jsonPage struct {
	Meta     map[string]string `json:"meta"`
	Name     string            `json:"name"`
	Versions []string          `json:"versions"`
	Files    []struct {
		Filename       string            `json:"filename"`
		URL            string            `json:"url"`
		Hashes         map[string]string `json:"hashes"`
		RequiresPython string            `json:"requires-python"`
		CoreMetadata   map[string]string `json:"core-metadata"`
		Size           int64             `json:"size"`
	} `json:"files"`
}

// TestRoutesPages checks the project list, the redirects to the normalized page with its final
// slash, and the links of the HTML page.
func TestRoutesPages(t *testing.T) {
	e := newPublished(t)
	rr := e.do(http.MethodGet, "simple/")
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != contentTypeText || !strings.Contains(rr.Body.String(), `<a href="my-pkg/">my-pkg</a>`) {
		t.Errorf("simple/: %d %q %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body)
	}
	for path, location := range map[string]string{
		"simple":         "/pypi/internal/simple/",
		"simple/My_Pkg/": "/pypi/internal/simple/my-pkg/",
		"simple/my-pkg":  "/pypi/internal/simple/my-pkg/",
		"simple/MY.PKG/": "/pypi/internal/simple/my-pkg/",
	} {
		if rr := e.do(http.MethodGet, path); rr.Code != http.StatusMovedPermanently || rr.Header().Get("Location") != location {
			t.Errorf("%s: %d to %q, want 301 to %q", path, rr.Code, rr.Header().Get("Location"), location)
		}
	}
	rr = e.do(http.MethodGet, "simple/my-pkg/")
	want := `<a href="../../files/my-pkg/1.0/` + e.pure + `#sha256=` + digest.FromBytes(e.bodies[e.pure]).Encoded() + `" data-requires-python="&gt;=3.9"`
	if !strings.Contains(rr.Body.String(), want) {
		t.Errorf("the HTML page has no %s:\n%s", want, rr.Body)
	}
}

// TestRoutesFiles reads the JSON page and fetches every file it lists and its core metadata:
// the bytes, the hashes, the sizes, and the sha256 of the core metadata the page announces.
func TestRoutesFiles(t *testing.T) {
	e := newPublished(t)
	rr := e.do(http.MethodGet, "simple/my-pkg/", "Accept", contentTypeJSON)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != contentTypeJSON || rr.Header().Get("Vary") != "Accept" {
		t.Fatalf("simple/my-pkg/ as JSON: %d %v", rr.Code, rr.Header())
	}
	var page jsonPage
	must(t, json.Unmarshal(rr.Body.Bytes(), &page))
	if page.Meta["api-version"] != "1.1" || page.Name != "my-pkg" || len(page.Versions) != 1 || len(page.Files) != 3 {
		t.Fatalf("page: %+v", page)
	}
	for _, f := range page.Files {
		e.checkFile(t, f.Filename, f.URL, f.Hashes["sha256"], f.CoreMetadata["sha256"], f.Size)
	}
}

// checkFile checks one file of the JSON page against what was published: its URL, sha256 and
// size, the bytes of its download, and its core metadata with the sha256 the page announced.
func (e *published) checkFile(t *testing.T, filename, url, sum, coreSum string, size int64) {
	t.Helper()
	want := e.bodies[filename]
	if sum != digest.FromBytes(want).Encoded() || size != int64(len(want)) || url != "../../files/my-pkg/1.0/"+filename {
		t.Errorf("%s: url %q, sha256 %s, size %d", filename, url, sum, size)
	}
	if rr := e.do(http.MethodGet, "files/my-pkg/1.0/"+filename); rr.Code != http.StatusOK || rr.Body.String() != string(want) {
		t.Errorf("GET %s: %d, %d bytes", filename, rr.Code, rr.Body.Len())
	}
	rr := e.do(http.MethodGet, "files/my-pkg/1.0/"+filename+".metadata")
	got := sha256.Sum256(rr.Body.Bytes())
	if rr.Code != http.StatusOK || rr.Body.String() != string(e.cores[filename]) || hex.EncodeToString(got[:]) != coreSum {
		t.Errorf("GET %s.metadata: %d, the announced sha256 %s, the body's %x", filename, rr.Code, coreSum, got)
	}
}

// TestRoutesRefuse checks the 404s and the 406, and that the next GET after another upload has
// the new file: ingest dropped the cached page.
func TestRoutesRefuse(t *testing.T) {
	e := newPublished(t)
	for _, path := range []string{"simple/nothing/", "files/my-pkg/9.9/" + e.pure, "files/other/1.0/" + e.pure, "files/my-pkg/1.0/nothing.whl"} {
		if rr := e.do(http.MethodGet, path); rr.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rr.Code)
		}
	}
	if rr := e.do(http.MethodGet, "simple/my-pkg/", "Accept", "application/xml"); rr.Code != http.StatusNotAcceptable {
		t.Errorf("Accept application/xml: %d, want 406", rr.Code)
	}
	later, li := testutil.Wheel(testutil.WheelOptions{Name: "my-pkg", Version: "1.1"})
	e.publish(li.Filename, later)
	if rr := e.do(http.MethodGet, "simple/my-pkg/"); !strings.Contains(rr.Body.String(), li.Filename) {
		t.Errorf("the page after another upload has no %s", li.Filename)
	}
}

func TestNegotiate(t *testing.T) {
	for _, tt := range []struct {
		accept      []string
		kind, ctype string
		ok          bool
	}{
		{nil, kindHTML, contentTypeText, true},
		{[]string{"*/*"}, kindHTML, contentTypeText, true},
		{[]string{contentTypeJSON}, kindJSON, contentTypeJSON, true},
		{[]string{contentTypeHTML}, kindHTML, contentTypeHTML, true},
		{[]string{"text/html"}, kindHTML, contentTypeText, true},
		{[]string{"text/html;q=0.5, " + contentTypeJSON}, kindJSON, contentTypeJSON, true},
		{[]string{contentTypeJSON + ";q=0.2, text/html;q=0.9"}, kindHTML, contentTypeText, true},
		{[]string{"application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html;q=0.2, text/html;q=0.01"}, kindJSON, contentTypeJSON, true},
		{[]string{"application/xml"}, "", "", false},
		{[]string{contentTypeJSON + ";q=0"}, "", "", false},
	} {
		kind, ctype, ok := negotiate(tt.accept)
		if kind != tt.kind || ctype != tt.ctype || ok != tt.ok {
			t.Errorf("negotiate(%q) = %q %q %v, want %q %q %v", tt.accept, kind, ctype, ok, tt.kind, tt.ctype, tt.ok)
		}
	}
}
