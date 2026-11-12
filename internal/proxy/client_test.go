package proxy

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
)

// noProxy is the egress proxy choice of the tests: none, whatever the environment says.
func noProxy(*http.Request) (*url.URL, error) { return nil, nil }

// testUpstream is the client of an upstream at base, with the timeout and the retries given.
func testUpstream(t *testing.T, base string, timeout time.Duration, retries int, o UpstreamOptions) *Upstream {
	t.Helper()
	if o.Proxy == nil {
		o.Proxy = noProxy
	}
	o.Log = slog.New(slog.DiscardHandler)
	u, err := NewUpstream(config.Upstream{Name: "up", Format: "pkg", URL: base, Timeout: timeout, Retries: retries}, o)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// get asks u for path relative to its URL.
func get(t *testing.T, u *Upstream, path string) (*Response, error) {
	t.Helper()
	target, err := u.Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	return u.Get(t.Context(), "document", target, "application/json", "", "")
}

// answers is an upstream below /registry/ that answers each path with its status, and keeps
// the headers of the last request in seen.
func answers(seen *atomic.Value) http.Handler {
	statuses := map[string]int{"/registry/ok": http.StatusOK, "/registry/not-modified": http.StatusNotModified,
		"/registry/gone": http.StatusGone, "/registry/busy": http.StatusTooManyRequests, "/registry/denied": http.StatusForbidden,
		"/registry/teapot": http.StatusTeapot}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Clone())
		status, ok := statuses[r.URL.Path]
		if !ok {
			status = http.StatusNotFound
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, "body")
		}
	})
}

// TestGetAnswers: what Get makes of each answer of an upstream (docs/spec/proxy-and-group.md
// §6), with its own headers only.
func TestGetAnswers(t *testing.T) {
	var seen atomic.Value
	srv := httptest.NewServer(answers(&seen))
	defer srv.Close()
	u := testUpstream(t, srv.URL+"/registry", time.Second, 0, UpstreamOptions{Authorization: "Bearer secret"})

	resp, err := get(t, u, "ok")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	h := seen.Load().(http.Header)
	if resp.Status != 200 || string(body) != "body" || resp.Header.Get("ETag") != `"v1"` {
		t.Errorf("ok: %d %q %v", resp.Status, body, resp.Header)
	}
	if h.Get("Authorization") != "Bearer secret" || h.Get("Accept") != "application/json" || !strings.HasPrefix(h.Get("User-Agent"), "lajstrom/") {
		t.Errorf("request headers %v", h)
	}
	if resp, err := get(t, u, "not-modified"); err != nil || resp.Status != http.StatusNotModified || resp.Body != nil {
		t.Errorf("not-modified: %+v, %v", resp, err)
	}
}

// TestGetErrors: the statuses of §6 that an upstream's failures become, with Retry-After for a
// 429 at most a minute, and no URL in the errors.
func TestGetErrors(t *testing.T) {
	var seen atomic.Value
	srv := httptest.NewServer(answers(&seen))
	defer srv.Close()
	u := testUpstream(t, srv.URL+"/registry", time.Second, 0, UpstreamOptions{})
	for _, tt := range []struct {
		path   string
		status int
		retry  string
	}{
		{"missing", 404, ""}, {"gone", 404, ""}, {"busy", 503, "60"}, {"denied", 502, ""}, {"teapot", 502, ""},
	} {
		_, err := get(t, u, tt.path)
		rr := httptest.NewRecorder()
		if got := format.WriteError(rr, err); got != tt.status || rr.Header().Get("Retry-After") != tt.retry {
			t.Errorf("%s: %v answers %d, Retry-After %q; want %d, %q", tt.path, err, got, rr.Header().Get("Retry-After"), tt.status, tt.retry)
		}
		if err != nil && strings.Contains(err.Error(), srv.URL) {
			t.Errorf("%s: the error %q names the upstream's URL", tt.path, err)
		}
	}
}

// TestGetRetries: a 5xx but 501 and 505 is asked again, up to the retries; a 501 is not.
func TestGetRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch {
		case r.URL.Path == "/flaky" && n < 3:
			w.WriteHeader(http.StatusBadGateway)
		case r.URL.Path == "/flaky":
			_, _ = io.WriteString(w, "ok")
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer srv.Close()
	u := testUpstream(t, srv.URL, time.Second, 2, UpstreamOptions{})
	resp, err := get(t, u, "flaky")
	if err != nil || calls.Load() != 3 {
		t.Fatalf("flaky: %v after %d calls, want an answer after 3", err, calls.Load())
	}
	_ = resp.Body.Close()
	calls.Store(0)
	if _, err := get(t, u, "unimplemented"); !errors.Is(err, format.ErrBadGateway) || calls.Load() != 1 {
		t.Errorf("unimplemented: %v after %d calls, want ErrBadGateway after 1", err, calls.Load())
	}
}

// TestGetTimeouts: the timeout bounds the wait for the headers and every wait for the next
// bytes of a body.
func TestGetTimeouts(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow-headers":
			<-release
			return
		case "/whole":
			_, _ = io.WriteString(w, strings.Repeat("all of it at once ", 1000))
			return
		}
		_, _ = io.WriteString(w, "first bytes")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release) // before Close, which waits for the handlers
	u := testUpstream(t, srv.URL, 100*time.Millisecond, 0, UpstreamOptions{})
	if _, err := get(t, u, "slow-headers"); !errors.Is(err, format.ErrGatewayTimeout) {
		t.Errorf("slow headers: %v, want ErrGatewayTimeout", err)
	}
	resp, err := get(t, u, "stalled-body")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, errStalled) || !errors.Is(u.transportError(t.Context(), err), format.ErrGatewayTimeout) {
		t.Errorf("stalled body: %v, want errStalled, a timeout", err)
	}
	// A reader busy elsewhere between its reads, as a proxy fetch is with its PATCH to the
	// registry, does not stall the body: the bound is on the waits for bytes (#694).
	whole, err := get(t, u, "whole")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = whole.Body.Close() }()
	n := 0
	for buf := make([]byte, 4096); ; {
		k, err := whole.Body.Read(buf)
		n += k
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("a body read with pauses longer than the timeout between the reads: %v after %d bytes", err, n)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if n != len("all of it at once ")*1000 {
		t.Errorf("read %d bytes, want %d", n, len("all of it at once ")*1000)
	}
}

// TestRedirects: the credentials go to the upstream's origin only, a redirect never goes from
// https to http, a host other than the upstream's must be https and public, and five
// redirects are the most (§9.2).
func TestRedirects(t *testing.T) {
	var auth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "elsewhere")
	}))
	defer other.Close()
	_, otherPort, _ := net.SplitHostPort(strings.TrimPrefix(other.URL, "http://"))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-http":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case "/to-localhost":
			http.Redirect(w, r, "https://localhost:"+otherPort+"/x", http.StatusFound)
		case "/to-private":
			http.Redirect(w, r, "https://10.0.0.1/x", http.StatusFound)
		case "/to-plain-other":
			http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer srv.Close()
	u := testUpstream(t, srv.URL, time.Second, 0, UpstreamOptions{Authorization: "Basic c2VjcmV0", TLS: srv.Client().Transport.(*http.Transport).TLSClientConfig})
	for _, path := range []string{"to-http", "to-localhost", "to-private", "to-plain-other", "loop"} {
		if _, err := get(t, u, path); !errors.Is(err, format.ErrBadGateway) {
			t.Errorf("%s: %v, want ErrBadGateway", path, err)
		}
	}
	if auth.Load() != nil {
		t.Errorf("a refused redirect reached the other server")
	}

	// Plain http on the upstream's own host, another port: another origin, no credentials.
	plain := testUpstream(t, strings.Replace(other.URL, otherPort, "1", 1), time.Second, 0, UpstreamOptions{Authorization: "Basic c2VjcmV0"})
	target, _ := url.Parse(other.URL + "/x")
	resp, err := plain.Get(t.Context(), "file", target, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if a := auth.Load(); a != "" {
		t.Errorf("Authorization %q sent to another origin of the upstream's host", a)
	}
}

// TestEgressProxy: a request goes through the proxy the environment chooses, which the dial
// takes as it is, though its address is loopback.
func TestEgressProxy(t *testing.T) {
	var asked atomic.Value
	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Store(r.URL.String())
		_, _ = io.WriteString(w, "through the proxy")
	}))
	defer egress.Close()
	via, _ := url.Parse(egress.URL)
	u := testUpstream(t, "http://registry.invalid/", time.Second, 0,
		UpstreamOptions{Proxy: func(*http.Request) (*url.URL, error) { return via, nil }})
	resp, err := get(t, u, "left-pad")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "through the proxy" || asked.Load() != "http://registry.invalid/left-pad" {
		t.Errorf("body %q, the proxy was asked %v", body, asked.Load())
	}
}

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14": true, "2606:2800:21f:cb07:6820:80da:af6b:8b2c": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "fe80::1": false, "fd00::1": false, "100.64.0.1": false, "0.0.0.0": false,
		"::": false, "224.0.0.1": false, "ff02::1": false, "255.255.255.255": false, "::ffff:10.0.0.1": false,
	} {
		if got := public(net.ParseIP(addr)); got != want {
			t.Errorf("public(%s) = %t, want %t", addr, got, want)
		}
	}
}

// TestLoadUpstreams: the credential files are read at start, Basic or Bearer, and a file that
// is missing or empty stops the start naming the key and the file.
func TestLoadUpstreams(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	token, password := write("token", " tok\n", 0o600), write("password", "pw\n", 0o644)
	var logs bytes.Buffer
	ups, err := LoadUpstreams([]config.Upstream{
		{Name: "a", Format: "npm", URL: "https://a.example", Auth: config.UpstreamAuth{TokenFile: token}},
		{Name: "b", Format: "npm", URL: "http://b.example", Auth: config.UpstreamAuth{Username: "u", PasswordFile: password}},
		{Name: "c", Format: "npm", URL: "https://c.example"},
		{Name: "d", Format: "npm", URL: "https://d.example", Auth: config.UpstreamAuth{Username: "u", PasswordEnv: "P", Password: "pw"}},
	}, UpstreamOptions{Log: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if ups["a"].authorization != "Bearer tok" || ups["b"].authorization != "Basic dTpwdw==" || ups["c"].authorization != "" ||
		ups["d"].authorization != "Basic dTpwdw==" {
		t.Errorf("authorization of a %q, b %q, c %q", ups["a"].authorization, ups["b"].authorization, ups["c"].authorization)
	}
	for _, w := range []string{"key=upstreams[1].auth.passwordFile", "open to the group or others", "upstream=b", "plain http"} {
		if !strings.Contains(logs.String(), w) {
			t.Errorf("log does not contain %q:\n%s", w, logs.String())
		}
	}
	for _, tt := range []struct {
		file, want string
	}{
		{filepath.Join(dir, "nope"), "upstreams[0].auth.tokenFile: the file does not exist"},
		{write("empty", "\n \n", 0o600), "upstreams[0].auth.tokenFile: the file is empty"},
	} {
		_, err := LoadUpstreams([]config.Upstream{{Name: "a", Format: "npm", URL: "https://a.example", Auth: config.UpstreamAuth{TokenFile: tt.file}}},
			UpstreamOptions{Log: slog.New(slog.DiscardHandler)})
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) || strings.Contains(err.Error(), dir) {
			t.Errorf("LoadUpstreams with %s: %v, want %q and no path", tt.file, err, tt.want)
		}
	}
}
