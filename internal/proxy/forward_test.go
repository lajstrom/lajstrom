package proxy_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// seenRequest is what a forwarding upstream was sent.
type seenRequest struct {
	method, path string
	header       http.Header
	body         string
}

// forwardUpstream is an upstream below /registry/ that records each request and answers with
// answer.
type forwardUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

func newForwardUpstream(t *testing.T, answer http.HandlerFunc) *forwardUpstream {
	t.Helper()
	f := &forwardUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(b)})
		f.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *forwardUpstream) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

// forwardCore is a core whose upstream "up" is f below /registry/, with the upstream's token,
// the retries and maxMetadataSize given, over a proxy repo of settings.
func forwardCore(t *testing.T, f *forwardUpstream, settings string, retries int, maxMetadataSize int64) (*proxy.Core, index.Repo) {
	t.Helper()
	e := newEnv(t, settings)
	up, err := proxy.NewUpstream(config.Upstream{Name: "up", Format: "raw", URL: f.srv.URL + "/registry", Timeout: 300 * time.Millisecond,
		Retries: retries, MaxMetadataSize: maxMetadataSize},
		proxy.UpstreamOptions{Authorization: "Bearer upstream-token", Proxy: noProxy, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	return e.core(t, "a", proxy.Options{Upstreams: map[string]*proxy.Upstream{"up": up}}), e.repo
}

// auditRequest is the request of the tests: npm's bulk advisories.
func auditRequest(body string) format.ForwardRequest {
	return format.ForwardRequest{Method: http.MethodPost, Path: "-/npm/v1/security/advisories/bulk", ContentType: "application/json",
		Accept: "application/json", Body: strings.NewReader(body)}
}

// forwardTo forwards an audit request of body to path on core and returns the error, the answer
// closed.
func forwardTo(t *testing.T, core *proxy.Core, repo index.Repo, path, body string) error {
	t.Helper()
	req := auditRequest(body)
	req.Path = path
	resp, err := core.Forward(t.Context(), repo, req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// TestForward: the request is the core's own, below the upstream's URL, with the plugin's parts,
// the upstream's credentials and nothing else; the answer comes back as the upstream sent it,
// and is asked again the next time: nothing is cached.
func TestForward(t *testing.T) {
	const advisories = `{"lodash":[{"id":1106913,"severity":"high"}]}`
	f := newForwardUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("npm-notice", "this endpoint is retiring")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, advisories)
	})
	core, repo := forwardCore(t, f, `{"name": "up"}`, 0, 0)
	for range 2 {
		resp, err := core.Forward(t.Context(), repo, auditRequest(`{"lodash":["4.17.15"]}`))
		must(t, err)
		b, err := io.ReadAll(resp.Body)
		must(t, err)
		must(t, resp.Body.Close())
		if resp.StatusCode != http.StatusOK || string(b) != advisories || resp.Header.Get("npm-notice") == "" {
			t.Errorf("the answer: %d %v %s", resp.StatusCode, resp.Header, b)
		}
	}
	seen := f.requests()
	if len(seen) != 2 {
		t.Fatalf("%d requests to the upstream, want 2: nothing is cached", len(seen))
	}
	r := seen[0]
	if r.method != http.MethodPost || r.path != "/registry/-/npm/v1/security/advisories/bulk" || r.body != `{"lodash":["4.17.15"]}` {
		t.Errorf("the request: %s %s %q", r.method, r.path, r.body)
	}
	for k, want := range map[string]string{"Authorization": "Bearer upstream-token", "Content-Type": "application/json",
		"Accept": "application/json", "Cookie": "", "Content-Encoding": ""} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	if !strings.HasPrefix(r.header.Get("User-Agent"), "lajstrom/") {
		t.Errorf("User-Agent %q", r.header.Get("User-Agent"))
	}
}

// TestForwardErrors: the upstream's statuses become those of §6, with its own error body in
// none of them; a request to an offline repo, one to another kind and one outside the upstream's
// URL send nothing.
func TestForwardErrors(t *testing.T) {
	f := newForwardUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		status, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/registry/"))
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "the upstream's secret body")
	})
	core, repo := forwardCore(t, f, `{"name": "up"}`, 0, 0)
	for _, tt := range []struct{ upstream, want int }{
		{404, 404}, {410, 404}, {400, 400}, {413, 400}, {415, 400}, {422, 400}, {429, 503}, {401, 502}, {403, 502}, {418, 502},
		{500, 502}, {503, 502}, {304, 502},
	} {
		err := forwardTo(t, core, repo, strconv.Itoa(tt.upstream), "{}")
		rr := httptest.NewRecorder()
		if got := format.WriteError(rr, err); got != tt.want {
			t.Errorf("upstream %d: %v answers %d, want %d", tt.upstream, err, got, tt.want)
		}
		if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), f.srv.URL)) {
			t.Errorf("upstream %d: the error %q has the upstream's body or URL", tt.upstream, err)
		}
	}
	calls := len(f.requests())
	offline, offlineRepo := forwardCore(t, f, `{"name": "up", "offline": true}`, 0, 0)
	hosted := repo
	hosted.Kind = index.KindHosted
	if err := forwardTo(t, offline, offlineRepo, "200", "{}"); format.StatusOf(err) != http.StatusNotFound {
		t.Errorf("offline: %v", err)
	}
	if err := forwardTo(t, core, hosted, "200", "{}"); format.StatusOf(err) != http.StatusNotImplemented {
		t.Errorf("a hosted repo: %v", err)
	}
	for _, path := range []string{"https://elsewhere.example/x", "/x", "../x", "//elsewhere.example/x", "-/../../x"} {
		if err := forwardTo(t, core, repo, path, "{}"); err == nil || format.StatusOf(err) != http.StatusInternalServerError {
			t.Errorf("path %q: %v", path, err)
		}
	}
	if got := len(f.requests()); got != calls {
		t.Errorf("%d requests to the upstream for what is refused", got-calls)
	}
}

// TestDenies: the deny list of a proxy repo, matched on the plugin's key of a name, or on the
// name itself when the plugin refuses it, ignoring ASCII case; allow does not count, and another
// kind of repo is an error (#514).
func TestDenies(t *testing.T) {
	f := newForwardUpstream(t, func(http.ResponseWriter, *http.Request) {})
	core, repo := forwardCore(t, f, `{"name": "up", "allow": ["lodash"], "deny": ["@acme/*", "*-internal"]}`, 0, 0)
	for _, tt := range []struct {
		name, pattern string
		denied        bool
	}{
		{"@acme/widgets", "@acme/*", true}, {"@ACME/Gadgets", "@acme/*", true}, {"tools-internal", "*-internal", true},
		{"lodash", "", false}, {"chalk", "", false}, {"@acmeish/x", "", false}, {"", "", false},
	} {
		pattern, denied, err := core.Denies(repo, tt.name)
		if err != nil || denied != tt.denied || pattern != tt.pattern {
			t.Errorf("%q: %q %v %v, want %q %v", tt.name, pattern, denied, err, tt.pattern, tt.denied)
		}
	}
	hosted := repo
	hosted.Kind = index.KindHosted
	if _, _, err := core.Denies(hosted, "@acme/widgets"); format.StatusOf(err) != http.StatusNotImplemented {
		t.Errorf("a hosted repo: %v", err)
	}
	if len(f.requests()) != 0 {
		t.Errorf("Denies asked the upstream")
	}
}

// TestForwardResend: a retry and a 307 send the body again, a retry from the URL first asked; a
// redirect to another host that is not https is refused.
func TestForwardResend(t *testing.T) {
	var mu sync.Mutex
	failed := false
	var f *forwardUpstream
	f = newForwardUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/registry/flaky":
			if !failed {
				failed = true
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		case "/registry/moved":
			http.Redirect(w, r, "/registry/flaky", http.StatusTemporaryRedirect)
			return
		case "/registry/elsewhere":
			http.Redirect(w, r, strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)+"/registry/flaky", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	core, repo := forwardCore(t, f, `{"name": "up"}`, 1, 0)
	must(t, forwardTo(t, core, repo, "moved", `{"a":["1.0.0"]}`))
	seen := f.requests()
	if len(seen) != 4 {
		t.Fatalf("%d requests, want the 307 and the 503, then both again", len(seen))
	}
	for _, r := range seen {
		if r.method != http.MethodPost || r.body != `{"a":["1.0.0"]}` {
			t.Errorf("%s %s: %q", r.method, r.path, r.body)
		}
	}
	if err := forwardTo(t, core, repo, "elsewhere", "{}"); format.StatusOf(err) != http.StatusBadGateway {
		t.Errorf("a redirect to another host over http: %v", err)
	}
	if got := len(f.requests()); got != 5 {
		t.Errorf("%d requests: the refused redirect was followed", got)
	}
}

// TestForwardBounds: a body above 16 MiB is 413 and is not sent; an answer above
// maxMetadataSize is 502, refused before its body when its length says so, else at the read
// past it; a body that stalls for the timeout is 504.
func TestForwardBounds(t *testing.T) {
	release := make(chan struct{})
	f := newForwardUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/registry/sized":
			w.Header().Set("Content-Length", "2048")
			_, _ = w.Write(make([]byte, 2048))
		case "/registry/chunked":
			for range 4 {
				_, _ = w.Write(make([]byte, 512))
				w.(http.Flusher).Flush()
			}
		case "/registry/stalls":
			_, _ = io.WriteString(w, "{")
			w.(http.Flusher).Flush()
			<-release
		}
	})
	t.Cleanup(func() { close(release) })
	core, repo := forwardCore(t, f, `{"name": "up"}`, 0, 1024)
	err := forwardTo(t, core, repo, "sized", strings.Repeat(" ", 16<<20+1))
	if format.StatusOf(err) != http.StatusRequestEntityTooLarge || len(f.requests()) != 0 {
		t.Errorf("a body above 16 MiB: %v, %d requests", err, len(f.requests()))
	}
	if err := forwardTo(t, core, repo, "sized", "{}"); format.StatusOf(err) != http.StatusBadGateway {
		t.Errorf("a Content-Length above maxMetadataSize: %v", err)
	}
	req := auditRequest("{}")
	for path, want := range map[string]error{"chunked": format.ErrBadGateway, "stalls": format.ErrGatewayTimeout} {
		req.Path = path
		resp, err := core.Forward(t.Context(), repo, req)
		must(t, err)
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !errors.Is(err, want) {
			t.Errorf("%s: the read fails with %v, want %v", path, err, want)
		}
	}
}
