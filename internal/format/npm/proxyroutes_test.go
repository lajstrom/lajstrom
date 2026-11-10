package npm

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/proxy"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fakeNpmjs is an upstream that answers as registry.npmjs.org does, from the captured
// packuments of is-number and @isaacs/string-locale-compare (full, or abbreviated for pnpm's
// Accept) with their tarball links on its own host; the versions it publishes are tarballs
// built by the test, whose checksums its packuments give. It counts the requests of each kind.
type fakeNpmjs struct {
	srv        *httptest.Server
	mu         sync.Mutex
	docs       map[string]map[bool]map[string]any // name, abbreviated → the packument
	tarballs   map[string][]byte                  // path → bytes
	packuments atomic.Int32
	files      atomic.Int32
	audits     []seenAudit  // the audit requests, in order (serveAudit)
	auditCode  atomic.Int32 // the status of an audit answer; 0 is 200 with advisories
}

func newFakeNpmjs(t *testing.T) *fakeNpmjs {
	t.Helper()
	u := &fakeNpmjs{docs: map[string]map[bool]map[string]any{}, tarballs: map[string][]byte{}}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	for name, path := range map[string]string{"is-number": capturedIsNumber, scopedName: capturedScoped} {
		u.docs[name] = map[bool]map[string]any{}
		for _, abbreviated := range []bool{false, true} {
			raw := strings.ReplaceAll(string(captured(t, path, abbreviated)), "https://registry.npmjs.org/", u.srv.URL+"/")
			var doc map[string]any
			must(t, json.Unmarshal([]byte(raw), &doc))
			u.docs[name][abbreviated] = doc
		}
	}
	return u
}

func (u *fakeNpmjs) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if r.Method == http.MethodPost {
		u.serveAudit(w, r)
		return
	}
	if b, ok := u.tarballs[r.URL.Path]; ok || strings.Contains(r.URL.Path, "/-/") {
		u.files.Add(1)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
		return
	}
	u.packuments.Add(1)
	doc, ok := u.docs[strings.TrimPrefix(r.URL.Path, "/")][strings.Contains(r.Header.Get("Accept"), contentTypeAbbreviated)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, _ := json.Marshal(doc)
	_, _ = w.Write(b)
}

// publish builds the tarball of name@version, makes both packuments give its checksums and
// serves it, or with other set other bytes than the checksums say.
func (u *fakeNpmjs) publish(t *testing.T, name, version string, other bool) []byte {
	t.Helper()
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: name, Version: version})
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, doc := range u.docs[name] {
		dist := doc["versions"].(map[string]any)[version].(map[string]any)["dist"].(map[string]any)
		dist["integrity"], dist["shasum"] = info.Integrity, info.Shasum
		link, err := url.Parse(dist["tarball"].(string))
		must(t, err)
		u.tarballs[link.Path] = b
		if other {
			u.tarballs[link.Path] = append(bytes.Clone(b), 0)
		}
	}
	return b
}

// drop removes a version from the upstream's packuments and names latest anew.
func (u *fakeNpmjs) drop(name, version, latest string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, doc := range u.docs[name] {
		delete(doc["versions"].(map[string]any), version)
		doc["dist-tags"] = map[string]any{"latest": latest}
	}
}

// proxyRepo is a proxy repo npm/npmjs of the fake upstream on the pipeline's registry and index,
// served by the npm routes with a proxy core whose clock the test moves.
type proxyRepo struct {
	*pipeline
	up   *fakeNpmjs
	repo index.Repo
	h    http.Handler
	now  atomic.Int64
}

func newProxyRepo(t *testing.T, externalURL string) *proxyRepo {
	t.Helper()
	p := &proxyRepo{pipeline: newPipeline(t), up: newFakeNpmjs(t)}
	p.now.Store(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	p.repo = index.Repo{Format: Name, Name: "npmjs", Registry: "local", Prefix: "npm/npmjs", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "npmjs"}`)}
	must(t, p.st.CreateRepo(t.Context(), &p.repo))
	up, err := proxy.NewUpstream(config.Upstream{Name: "npmjs", Format: Name, URL: p.up.srv.URL, Timeout: 5 * time.Second},
		proxy.UpstreamOptions{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	providers := provider.Map{"local": p.fake}
	core := proxy.New(proxy.Options{Index: p.st, Ingest: p.pipe, Providers: providers, Upstreams: map[string]*proxy.Upstream{"npmjs": up},
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{ExternalURL: externalURL}, name == Name }, ExternalURL: externalURL,
		Now: func() time.Time { return time.Unix(0, p.now.Load()).UTC() }, Log: slog.New(slog.DiscardHandler)})
	deps := format.Deps{Index: p.st, Ingest: p.pipe, Providers: providers, Proxy: core, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{ExternalURL: externalURL}.Routes(format.NewRouter(mux, Name, deps), deps)
	p.h = mux
	return p
}

// packument gets a packument of the proxy repo and decodes it.
func (p *proxyRepo) packument(t *testing.T, path, accept string) proxied {
	t.Helper()
	rr := get(p.h, "/npm/npmjs/"+path, "Accept", accept)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), p.up.srv.URL) {
		t.Errorf("GET %s: a link to the upstream", path)
	}
	return decodeProxied(t, rr.Body.Bytes())
}

// TestProxyInstall installs through a proxy repo as npm, pnpm and yarn do: the packuments with
// the upstream's versions and dist-tags and the tarballs under the repo's URL, asked of the
// upstream once in their TTL; each tarball fetched on its first request, checked, held and
// served, and served again without a request to the upstream; a scoped package at both
// spellings, through the abbreviated packument.
func TestProxyInstall(t *testing.T) {
	p := newProxyRepo(t, "https://registry.example")
	want := p.up.publish(t, "is-number", "7.0.0", false)
	doc := p.packument(t, "is-number", acceptNpm)
	expect(t, "dist-tags", doc.distTags, map[string]string{"latest": "7.0.0"})
	expect(t, "versions", len(doc.dists), 15)
	link := doc.dists["7.0.0"]["tarball"].(string)
	expect(t, "dist.tarball", link, "https://registry.example/npm/npmjs/is-number/-/is-number-7.0.0.tgz")
	p.packument(t, "is-number", acceptNpm)
	expect(t, "packument requests in the TTL", p.up.packuments.Load(), int32(1))
	for range 2 {
		rr := get(p.h, strings.TrimPrefix(link, "https://registry.example"))
		if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), want) || rr.Header().Get("Docker-Content-Digest") == "" {
			t.Fatalf("GET %s: %d, %d bytes", link, rr.Code, rr.Body.Len())
		}
	}
	expect(t, "tarball requests", p.up.files.Load(), int32(1))

	scoped := p.up.publish(t, scopedName, "1.1.0", false)
	short := p.packument(t, "@isaacs%2Fstring-locale-compare", acceptPnpm)
	expect(t, "abbreviated members", short.members, []string{"dist-tags", "modified", "name", "versions"})
	expect(t, "scoped dist.tarball", short.dists["1.1.0"]["tarball"],
		any("https://registry.example/npm/npmjs/@isaacs/string-locale-compare/-/string-locale-compare-1.1.0.tgz"))
	for _, path := range []string{"@isaacs%2fstring-locale-compare/-/string-locale-compare-1.1.0.tgz",
		"@isaacs/string-locale-compare/-/string-locale-compare-1.1.0.tgz"} {
		if rr := get(p.h, "/npm/npmjs/"+path); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), scoped) {
			t.Errorf("GET %s: %d", path, rr.Code)
		}
	}
	expect(t, "tarball requests", p.up.files.Load(), int32(2))
}

// TestProxyUpstreamListing: once its TTL is over the packument is the upstream's again, so a
// version the upstream dropped is not listed, and its held tarball is still served without a
// request to the upstream.
func TestProxyUpstreamListing(t *testing.T) {
	p := newProxyRepo(t, "https://registry.example")
	want := p.up.publish(t, "is-number", "7.0.0", false)
	p.packument(t, "is-number", acceptNpm)
	if rr := get(p.h, "/npm/npmjs/is-number/-/is-number-7.0.0.tgz"); rr.Code != http.StatusOK {
		t.Fatalf("the first GET: %d %s", rr.Code, rr.Body)
	}
	p.up.drop("is-number", "7.0.0", "6.0.0")
	p.now.Add(int64(11 * time.Minute))
	doc := p.packument(t, "is-number", acceptNpm)
	if _, ok := doc.dists["7.0.0"]; ok || doc.distTags["latest"] != "6.0.0" {
		t.Errorf("after the upstream dropped 7.0.0: latest %s, 7.0.0 listed %v", doc.distTags["latest"], ok)
	}
	rr := get(p.h, "/npm/npmjs/is-number/-/is-number-7.0.0.tgz")
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), want) || p.up.files.Load() != 1 {
		t.Errorf("the dropped version's tarball: %d, %d upstream requests", rr.Code, p.up.files.Load())
	}
}

// TestProxyRefuses: a tarball whose bytes are not those of its integrity is 502 and not held; a
// version or a name the upstream does not have is 404; a publish is 405 with Allow.
func TestProxyRefuses(t *testing.T) {
	p := newProxyRepo(t, "https://registry.example")
	p.up.publish(t, "is-number", "6.0.0", true)
	for range 2 {
		rr := get(p.h, "/npm/npmjs/is-number/-/is-number-6.0.0.tgz")
		if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), `"error"`) {
			t.Errorf("a tarball that fails its integrity: %d %s", rr.Code, rr.Body)
		}
	}
	expect(t, "tarball requests", p.up.files.Load(), int32(2))
	for path, code := range map[string]int{"is-number/-/is-number-99.0.0.tgz": http.StatusNotFound, "left-pad": http.StatusNotFound,
		"left-pad/-/left-pad-1.3.0.tgz": http.StatusNotFound} {
		if rr := get(p.h, "/npm/npmjs/"+path); rr.Code != code {
			t.Errorf("GET %s: %d %s, want %d", path, rr.Code, rr.Body, code)
		}
	}
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: "is-number", Version: "8.0.0"})
	rr := do(p.h, http.MethodPut, "/npm/npmjs/is-number", npmBody(t, b, info, nil), false)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("a publish to a proxy repo: %d %v %s", rr.Code, rr.Header(), rr.Body)
	}
}

// TestProxyRequestHost: without server.externalURL, the links are under the request's host.
func TestProxyRequestHost(t *testing.T) {
	p := newProxyRepo(t, "")
	doc := p.packument(t, "is-number", acceptNpm)
	expect(t, "dist.tarball", doc.dists["7.0.0"]["tarball"], any("http://example.com/npm/npmjs/is-number/-/is-number-7.0.0.tgz"))
}
