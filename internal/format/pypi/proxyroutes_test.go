package pypi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/proxy"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fakePyPI is an upstream simple index as pypi.org's: /simple/<project>/ answers the JSON form
// for PEP 691's Accept, with absolute links to /packages/<file> on its own host, or with htmlOnly
// the HTML form of PEP 503 whatever the Accept, with links relative to the page; /packages/<file>
// answers the bytes. It counts the requests of pages and of files.
type fakePyPI struct {
	srv      *httptest.Server
	mu       sync.Mutex
	htmlOnly bool
	status   int                   // when not 0, the status of every request
	files    map[string][]fakeFile // project → its files, in order
	bodies   map[string][]byte     // file name → bytes
	pages    atomic.Int32
	fetches  atomic.Int32
}

// fakeFile is a file of a project's page on the upstream.
type fakeFile struct {
	filename, sha256, requiresPython, core, uploadTime string
	yanked                                             any // nil, true or the reason
	size                                               int64
}

func newFakePyPI(t *testing.T) *fakePyPI {
	t.Helper()
	u := &fakePyPI{files: map[string][]fakeFile{}, bodies: map[string][]byte{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakePyPI) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.status != 0 {
		w.WriteHeader(u.status)
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, "/packages/"); ok {
		u.fetches.Add(1)
		b, ok := u.bodies[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
		return
	}
	project, ok := strings.CutPrefix(r.URL.Path, "/simple/")
	project, slash := strings.CutSuffix(project, "/")
	u.pages.Add(1)
	files, known := u.files[project]
	if !ok || !slash || !known {
		http.NotFound(w, r)
		return
	}
	if u.htmlOnly || !strings.Contains(r.Header.Get("Accept"), contentTypeJSON) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(u.html(project, files)))
		return
	}
	page := map[string]any{"meta": map[string]any{"_last-serial": 26054547, "api-version": "1.3"}, "name": project}
	var out []map[string]any
	for _, f := range files {
		j := map[string]any{"filename": f.filename, "url": u.srv.URL + "/packages/" + f.filename + "#sha256=" + f.sha256,
			"hashes": map[string]string{"sha256": f.sha256}, "size": f.size, "upload-time": f.uploadTime, "yanked": false,
			"requires-python": nil, "provenance": nil, "data-dist-info-metadata": false}
		if f.requiresPython != "" {
			j["requires-python"] = f.requiresPython
		}
		if f.core != "" {
			j["core-metadata"] = map[string]string{"sha256": f.core}
		}
		if f.yanked != nil {
			j["yanked"] = f.yanked
		}
		out = append(out, j)
	}
	page["files"] = out
	w.Header().Set("Content-Type", contentTypeJSON)
	_ = json.NewEncoder(w).Encode(page)
}

// html is the HTML form of a project's page, its links relative to the page as a static index's,
// with the old name of core-metadata (PEP 714), and a file in a comment that is not one.
func (u *fakePyPI) html(project string, files []fakeFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!DOCTYPE html>\n<html><head><title>Links for %s</title></head><body>\n<!-- <a href=\"../../packages/gone-1.0.tar.gz\">gone</a> -->\n", project)
	for _, f := range files {
		fmt.Fprintf(&b, "<A HREF='../../packages/%s#sha256=%s'", f.filename, f.sha256)
		if f.requiresPython != "" {
			fmt.Fprintf(&b, " data-requires-python=\"%s\"", html.EscapeString(f.requiresPython))
		}
		if f.core != "" {
			fmt.Fprintf(&b, " data-dist-info-metadata=\"sha256=%s\"", f.core)
		}
		switch y := f.yanked.(type) {
		case string:
			fmt.Fprintf(&b, " data-yanked=\"%s\"", html.EscapeString(y))
		case bool:
			b.WriteString(" data-yanked")
		}
		fmt.Fprintf(&b, ">%s</A><br/>\n", f.filename)
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

// publish puts a distribution on the upstream: the page lists it with its sha256, or with other
// the sha256 of other bytes, its size and upload time, and for a wheel its core metadata's sha256.
func (u *fakePyPI) publish(project, filename string, b, core []byte, other bool, set func(*fakeFile)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	sum := sha256.Sum256(b)
	if other {
		sum = sha256.Sum256(append(bytes.Clone(b), 0))
	}
	f := fakeFile{filename: filename, sha256: hex.EncodeToString(sum[:]), size: int64(len(b)), uploadTime: "2026-11-01T09:30:00.123456Z"}
	if core != nil {
		c := sha256.Sum256(core)
		f.core = hex.EncodeToString(c[:])
	}
	if set != nil {
		set(&f)
	}
	u.files[project] = append(u.files[project], f)
	u.bodies[filename] = b
}

// proxyEnv is newEnv with the proxy repo pypi/pypi of the fake upstream, served with a core of
// proxy and group repos whose clock the test moves.
type proxyEnv struct {
	*env
	up    *fakePyPI
	proxy index.Repo
	now   atomic.Int64
}

func newProxyEnv(t *testing.T) *proxyEnv {
	t.Helper()
	p := &proxyEnv{env: newEnv(t), up: newFakePyPI(t)}
	p.now.Store(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	p.proxy = index.Repo{Format: Name, Name: "pypi", Registry: "local", Prefix: "pypi/pypi", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "pypi", "deny": ["acme-*"]}`)}
	must(t, p.st.CreateRepo(t.Context(), &p.proxy))
	up, err := proxy.NewUpstream(config.Upstream{Name: "pypi", Format: Name, URL: p.up.srv.URL + "/simple/", Timeout: 5 * time.Second},
		proxy.UpstreamOptions{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	providers := provider.Map{"local": p.prov}
	core := proxy.New(proxy.Options{Index: p.st, Ingest: p.pipe, Providers: providers, Upstreams: map[string]*proxy.Upstream{"pypi": up},
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }, ExternalURL: "https://pypi.example",
		Now: func() time.Time { return time.Unix(0, p.now.Load()).UTC() }, Log: slog.New(slog.DiscardHandler)})
	deps := format.Deps{Index: p.st, Ingest: p.pipe, Providers: providers, Proxy: core, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	p.h = mux
	return p
}

// get sends a GET of /pypi/<path>; hdr are header name and value pairs.
func (p *proxyEnv) get(path string, hdr ...string) *httptest.ResponseRecorder {
	p.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/pypi/"+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	p.h.ServeHTTP(rr, req)
	return rr
}

// page gets the JSON page of a project of a repo and decodes it; no link names the upstream.
func (p *proxyEnv) page(path string) proxiedPage {
	p.t.Helper()
	rr := p.get(path, "Accept", contentTypeJSON)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != contentTypeJSON {
		p.t.Fatalf("GET %s: %d %v %s", path, rr.Code, rr.Header(), rr.Body)
	}
	if strings.Contains(rr.Body.String(), p.up.srv.URL) {
		p.t.Errorf("GET %s: a link to the upstream:\n%s", path, rr.Body)
	}
	var page proxiedPage
	must(p.t, json.Unmarshal(rr.Body.Bytes(), &page))
	return page
}

// proxiedPage is a JSON page as the proxy tests read it: the files by file name.
type proxiedPage struct {
	Meta     map[string]string `json:"meta"`
	Name     string            `json:"name"`
	Versions []string          `json:"versions"`
	Files    []pageFile        `json:"files"`
}

// pageFile is a file of a proxiedPage.
type pageFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python"`
	CoreMetadata   any               `json:"core-metadata"`
	DistInfo       any               `json:"dist-info-metadata"`
	Yanked         any               `json:"yanked"`
	Size           *int64            `json:"size"`
	UploadTime     string            `json:"upload-time"`
}

// file returns the file of the page named filename.
func (pg proxiedPage) file(t *testing.T, filename string) pageFile {
	t.Helper()
	for _, f := range pg.Files {
		if f.Filename == filename {
			return f
		}
	}
	t.Fatalf("the page of %s has no %s: %+v", pg.Name, filename, pg.Files)
	return pageFile{}
}

// filenames returns the file names of the page, in order.
func (pg proxiedPage) filenames() []string {
	var out []string
	for _, f := range pg.Files {
		out = append(out, f.Filename)
	}
	return out
}

// coreOf is the core-metadata of a page's file as a sha256, "" for false.
func coreOf(f pageFile) string {
	m, _ := f.CoreMetadata.(map[string]any)
	s, _ := m["sha256"].(string)
	return s
}

// sha256Of is the lowercase hex sha256 of b.
func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// upstreamProject puts on the upstream my-pkg 1.0: a pure wheel with Requires-Python, a native
// wheel and an sdist, and returns their bytes and the wheels' core metadata by file name.
func upstreamProject(up *fakePyPI) (bodies, cores map[string][]byte, pure, native, sdist string) {
	pb, pi := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.0", RequiresPython: ">=3.9"})
	nb, ni := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "1.0", PythonTag: "cp311", AbiTag: "cp311",
		PlatformTag: "manylinux_2_17_x86_64"})
	sb, si := testutil.Sdist(testutil.SdistOptions{Name: "My_Pkg", Version: "1.0"})
	up.publish("my-pkg", pi.Filename, pb, pi.Metadata, false, func(f *fakeFile) { f.requiresPython = ">=3.9" })
	up.publish("my-pkg", ni.Filename, nb, ni.Metadata, false, nil)
	up.publish("my-pkg", si.Filename, sb, nil, false, nil)
	return map[string][]byte{pi.Filename: pb, ni.Filename: nb, si.Filename: sb},
		map[string][]byte{pi.Filename: pi.Metadata, ni.Filename: ni.Metadata}, pi.Filename, ni.Filename, si.Filename
}

// TestProxyInstall installs through a proxy repo as pip and uv do: the JSON page lists the
// upstream's files with their links under the repo, their sha256, requires-python and the
// sha256 of a wheel's core metadata, asked of the upstream once in its TTL, and the HTML form is
// the same page; a file and a wheel's core metadata are fetched on the first request, checked,
// held and served without a second request to the upstream, and the page then has the bytes the
// repo holds. A file streams from the upstream into the registry: no temporary file.
func TestProxyInstall(t *testing.T) {
	formattest.NoTempDir(t)
	p := newProxyEnv(t)
	bodies, cores, pure, native, sdist := upstreamProject(p.up)

	page := p.page("pypi/simple/my-pkg/")
	expect(t, "the files", page.filenames(), []string{pure, native, sdist})
	expect(t, "the API version and versions", []any{page.Meta["api-version"], page.Versions}, []any{"1.1", []string{"1.0"}})
	f := page.file(t, pure)
	expect(t, "the pure wheel", []any{f.URL, f.Hashes["sha256"], f.RequiresPython, coreOf(f), *f.Size, f.UploadTime, f.Yanked},
		[]any{"../../files/my-pkg/1.0/" + pure, sha256Of(bodies[pure]), ">=3.9", sha256Of(cores[pure]), int64(len(bodies[pure])),
			"2026-11-01T09:30:00.123456Z", false})
	expect(t, "the sdist's core-metadata", page.file(t, sdist).CoreMetadata, any(false))
	rr := p.get("pypi/simple/my-pkg/")
	want := `<a href="../../files/my-pkg/1.0/` + pure + `#sha256=` + sha256Of(bodies[pure]) + `" data-requires-python="&gt;=3.9"` +
		` data-dist-info-metadata="sha256=` + sha256Of(cores[pure]) + `" data-core-metadata="sha256=` + sha256Of(cores[pure]) + `">`
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != contentTypeText || !strings.Contains(rr.Body.String(), want) {
		t.Errorf("the HTML page: %d %q, want %s in:\n%s", rr.Code, rr.Header().Get("Content-Type"), want, rr.Body)
	}
	expect(t, "page requests to the upstream in the TTL, both forms", p.up.pages.Load(), int32(1))

	for range 2 {
		for _, name := range []string{pure, sdist} {
			if rr := p.get("pypi/files/my-pkg/1.0/" + name); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), bodies[name]) {
				t.Fatalf("GET %s: %d %s", name, rr.Code, rr.Body)
			}
		}
	}
	expect(t, "file requests to the upstream", p.up.fetches.Load(), int32(2))
	rr = p.get("pypi/files/my-pkg/1.0/" + native + ".metadata")
	if rr.Code != http.StatusOK || sha256Of(rr.Body.Bytes()) != coreOf(page.file(t, native)) {
		t.Errorf("the core metadata of a wheel not held: %d, sha256 %s, the page says %s", rr.Code, sha256Of(rr.Body.Bytes()),
			coreOf(page.file(t, native)))
	}
	expect(t, "file requests to the upstream after the metadata of a wheel", p.up.fetches.Load(), int32(3))
	if rr := p.get("pypi/files/my-pkg/1.0/" + sdist + ".metadata"); rr.Code != http.StatusNotFound || p.up.fetches.Load() != 3 {
		t.Errorf("the core metadata of an sdist: %d, %d file requests", rr.Code, p.up.fetches.Load())
	}

	held := p.page("pypi/simple/my-pkg/")
	f = held.file(t, pure)
	expect(t, "the held wheel", []any{f.Hashes["sha256"], coreOf(f), *f.Size}, []any{sha256Of(bodies[pure]), sha256Of(cores[pure]),
		int64(len(bodies[pure]))})
	versions, err := p.st.ListVersions(t.Context(), p.pkg(t, "my-pkg").ID)
	must(t, err)
	expect(t, "the held versions", len(versions), 3)
	for _, v := range versions {
		if !v.CreatedAt.Equal(time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)) {
			t.Errorf("%s %s: created %s, want the upstream's upload-time", v.Version, v.Variant, v.CreatedAt)
		}
	}
}

// pkg returns the package of the proxy repo named name.
func (p *proxyEnv) pkg(t *testing.T, name string) *index.Package {
	t.Helper()
	pkg, err := p.st.GetPackage(t.Context(), p.proxy.ID, name)
	must(t, err)
	return pkg
}

// TestProxyHTMLUpstream reads an upstream that has the HTML form only (PEP 503): links relative
// to its page, the hash in their fragment, data-requires-python escaped, data-yanked, and the
// core metadata under its name before PEP 714; the files a proxy cannot fetch (another kind,
// another project, no sha256, a second sdist of a version) are not listed, and a file is fetched
// from the link resolved against the page. The page is of API version 1.0: the upstream gave no
// size.
func TestProxyHTMLUpstream(t *testing.T) {
	p := newProxyEnv(t)
	p.up.htmlOnly = true
	bodies, cores, pure, _, sdist := upstreamProject(p.up)
	zb, _ := testutil.Wheel(testutil.WheelOptions{Name: "My_Pkg", Version: "0.9"})
	p.up.publish("my-pkg", "my_pkg-0.9-py3-none-any.whl", zb, nil, false, func(f *fakeFile) { f.yanked = "broken < 1" })
	p.up.publish("my-pkg", "my_pkg-0.8-py3-none-any.whl", zb, nil, false, func(f *fakeFile) { f.yanked = true })
	p.up.publish("my-pkg", "my_pkg-1.0.zip", zb, nil, false, nil)
	p.up.publish("my-pkg", "my_pkg-1.0-py3.9.egg", zb, nil, false, nil)
	p.up.publish("my-pkg", "other_pkg-1.0-py3-none-any.whl", zb, nil, false, nil)
	p.up.publish("my-pkg", "my_pkg-0.7-py3-none-any.whl", zb, nil, false, func(f *fakeFile) { f.sha256 = "" })

	page := p.page("pypi/simple/my-pkg/")
	expect(t, "the files", page.filenames(), []string{pure, "my_pkg-1.0-cp311-cp311-manylinux_2_17_x86_64.whl", sdist,
		"my_pkg-0.9-py3-none-any.whl", "my_pkg-0.8-py3-none-any.whl"})
	expect(t, "the API version", page.Meta["api-version"], "1.0")
	f := page.file(t, pure)
	expect(t, "the pure wheel", []any{f.RequiresPython, coreOf(f), f.Size, f.UploadTime}, []any{">=3.9", sha256Of(cores[pure]), (*int64)(nil), ""})
	expect(t, "the yanks", []any{page.file(t, "my_pkg-0.9-py3-none-any.whl").Yanked, page.file(t, "my_pkg-0.8-py3-none-any.whl").Yanked, f.Yanked},
		[]any{"broken < 1", true, false})
	if rr := p.get("pypi/files/my-pkg/1.0/" + pure); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), bodies[pure]) {
		t.Fatalf("GET %s: %d %s", pure, rr.Code, rr.Body)
	}
	rr := p.get("pypi/simple/my-pkg/", "Accept", "text/html")
	if !strings.Contains(rr.Body.String(), `data-yanked="broken &lt; 1"`) || !strings.Contains(rr.Body.String(), `content="1.0"`) {
		t.Errorf("the HTML page:\n%s", rr.Body)
	}
	if held := p.page("pypi/simple/my-pkg/").file(t, pure); held.Size == nil || *held.Size != int64(len(bodies[pure])) {
		t.Errorf("the held wheel's size: %v", held.Size)
	}
}

// TestProxyRefuses: a file whose bytes are not those of its sha256 is 502 and not held; a
// version, a file or a project the upstream does not have is 404, and so is a path whose file
// is not of its project and version; a name of the deny list is 403 and asks the upstream
// nothing; an upload is 405 with Allow.
func TestProxyRefuses(t *testing.T) {
	p := newProxyEnv(t)
	b, info := testutil.Wheel(testutil.WheelOptions{Name: "my-pkg", Version: "2.0"})
	p.up.publish("my-pkg", info.Filename, b, info.Metadata, true, nil)
	p.up.publish("acme-billing", info.Filename, b, nil, false, nil)
	for range 2 {
		if rr := p.get("pypi/files/my-pkg/2.0/" + info.Filename); rr.Code != http.StatusBadGateway {
			t.Errorf("a file that fails its sha256: %d %s", rr.Code, rr.Body)
		}
	}
	expect(t, "manifests pushed and upload sessions left open", []int{len(p.fake.PushReferences()), p.fake.OpenUploads()}, []int{0, 0})
	rejects, err := p.st.ListAudit(t.Context(), index.AuditFilter{Action: "proxy.reject"})
	must(t, err)
	expect(t, "proxy.reject rows", len(rejects), 2)
	pages := p.up.pages.Load()
	for path, code := range map[string]int{
		"pypi/files/my-pkg/9.9/my_pkg-9.9-py3-none-any.whl":             http.StatusNotFound,
		"pypi/files/my-pkg/2.0/my_pkg-2.0-py2-none-any.whl":             http.StatusNotFound,
		"pypi/files/my-pkg/1.0/" + info.Filename:                        http.StatusNotFound,
		"pypi/files/other/2.0/" + info.Filename:                         http.StatusNotFound,
		"pypi/simple/left-pad/":                                         http.StatusNotFound,
		"pypi/simple/acme-billing/":                                     http.StatusForbidden,
		"pypi/files/acme-billing/2.0/acme_billing-2.0-py3-none-any.whl": http.StatusForbidden,
	} {
		if rr := p.get(path); rr.Code != code {
			t.Errorf("GET %s: %d %s, want %d", path, rr.Code, rr.Body, code)
		}
	}
	expect(t, "page requests for those", p.up.pages.Load()-pages, int32(1)) // left-pad's: my-pkg's page is fresh
	req := httptest.NewRequest(http.MethodPost, "/pypi/pypi/", strings.NewReader(""))
	rr := httptest.NewRecorder()
	p.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("an upload to a proxy repo: %d %v %s", rr.Code, rr.Header(), rr.Body)
	}
}

// TestProxyHeldWithoutUpstream: without a cached page, a proxy whose upstream fails serves the
// page of what it holds, as a hosted repo's; and the project list is the projects it holds.
func TestProxyHeldWithoutUpstream(t *testing.T) {
	p := newProxyEnv(t)
	bodies, _, pure, _, _ := upstreamProject(p.up)
	if rr := p.get("pypi/files/my-pkg/1.0/" + pure); rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", pure, rr.Code, rr.Body)
	}
	_, err := p.st.DeleteProxyMeta(t.Context(), p.proxy.ID, "my-pkg")
	must(t, err)
	p.up.mu.Lock()
	p.up.status = http.StatusServiceUnavailable
	p.up.mu.Unlock()
	page := p.page("pypi/simple/my-pkg/")
	expect(t, "the held files", page.filenames(), []string{pure})
	expect(t, "the held file's sha256", page.file(t, pure).Hashes["sha256"], digest.FromBytes(bodies[pure]).Encoded())
	if rr := p.get("pypi/simple/left-pad/"); rr.Code != http.StatusBadGateway {
		t.Errorf("a project neither cached nor held while the upstream fails: %d %s", rr.Code, rr.Body)
	}
	for accept, want := range map[string]string{contentTypeJSON: `{"name":"my-pkg"}`, "text/html": `<a href="my-pkg/">my-pkg</a>`} {
		if rr := p.get("pypi/simple/", "Accept", accept); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
			t.Errorf("the project list as %s: %d %s", accept, rr.Code, rr.Body)
		}
	}
}

// expect compares got with want as JSON, which the tests' values are.
func expect(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("%s: %s, want %s", what, g, w)
	}
}
