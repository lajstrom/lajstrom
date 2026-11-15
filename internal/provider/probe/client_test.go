package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
)

func TestRefusal(t *testing.T) {
	cases := []struct {
		name string
		a    answer
		want string
	}{
		{"error code", errAns(405, "UNSUPPORTED"), "405 `UNSUPPORTED`"},
		{"plain text", plain404, "a plain 404"},
		{"html", ans(404, "<html></html>", "Content-Type", "text/html"), "an HTML 404"},
		{"empty", ans(500, ""), "an empty 500"},
		{"other", ans(400, "{}", "Content-Type", "application/json"), "a 400"},
		{"distribution writes the error twice", ans(400,
			`{"errors":[{"code":"NAME_INVALID","message":"m"}]}`+"\n"+`{"errors":[{"code":"NAME_INVALID","message":"m"}]}`),
			"400 `NAME_INVALID`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.refusal(); got != c.want {
				t.Errorf("refusal() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestErrorOfDetail(t *testing.T) {
	a := ans(400, `{"errors":[{"code":"MANIFEST_INVALID","message":"manifest invalid","detail":"unknown media type: x"}]}`)
	code, msg, detail := a.errorOf()
	if code != "MANIFEST_INVALID" || msg != "manifest invalid" || detail != "unknown media type: x" {
		t.Errorf("errorOf() = %q, %q, %q", code, msg, detail)
	}
	if _, _, detail := errAns(400, "X").errorOf(); detail != "" {
		t.Errorf("detail without one = %q", detail)
	}
}

func TestSanitiseURL(t *testing.T) {
	base, _ := url.Parse("http://registry.test")
	cases := []struct{ in, want string }{
		{"http://registry.test/v2/a/tags/list?n=2&last=b", "/v2/a/tags/list?last=b&n=2"},
		{"http://registry.test/v2/a/blobs/uploads/1?_state=secret&digest=sha256:ab", "/v2/a/blobs/uploads/1?_state=…&digest=sha256:ab"},
		{"http://s3.test/b/k?X-Amz-Credential=AKIA&X-Amz-Expires=1200&X-Amz-Signature=f00", "http://s3.test/b/k?X-Amz-Credential=…&X-Amz-Expires=1200&X-Amz-Signature=…"},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.in)
		if got := sanitiseURL(base, u); got != c.want {
			t.Errorf("sanitiseURL(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRepositoryOf(t *testing.T) {
	cases := map[string]string{
		"/v2/a/b/blobs/uploads/":                     "a/b",
		"/v2/a/manifests/sha256:00":                  "a",
		"/v2/p/x/y/tags/list":                        "p/x/y",
		"/v2/a/referrers/sha256:00":                  "a",
		"/v2/_catalog":                               "",
		"/v2/":                                       "",
		"/token":                                     "",
		"/v2/lajstrom-it/s/up/blobs/abc":             "lajstrom-it/s/up",
		"/v2/s/route/x/manifests/y/blobs/uploads/":   "s/route/x/manifests/y",
		"/v2/s/route/x/manifests/y/manifests/v1":     "s/route/x/manifests/y",
		"/v2/s/route/x/tags/list/tags/list":          "s/route/x/tags/list",
		"/v2/s/route/x/tags/list/blobs/uploads/1f":   "s/route/x/tags/list",
		"/v2/s/route/x/referrers/y/referrers/sha256": "s/route/x/referrers/y",
		"/v2/x/blobs/blobs/sha256:00":                "x/blobs",
		"/v2/blobs/uploads/":                         "",
	}
	for path, want := range cases {
		if got := repositoryOf(path); got != want {
			t.Errorf("repositoryOf(%s) = %q, want %q", path, got, want)
		}
	}
}

func TestRealmGuard(t *testing.T) {
	cases := []struct {
		name, registry, challenge string
		refused                   bool
	}{
		{"realm on the registry's host", "http://localhost:5009", `Bearer realm="http://localhost:5009/v2/token",service="container_registry",scope="*"`, false},
		{"realm on another port of it", "http://localhost:5006", `Bearer realm="http://localhost:5005/token"`, false},
		{"realm on another host", "https://registry-1.docker.io", `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`, true},
		{"https downgraded", "https://r.test", `Bearer realm="http://r.test/token"`, true},
		{"not a URL", "https://r.test", `Bearer realm="token"`, true},
		{"basic", "https://r.test", `Basic realm="Registry"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, _ := url.Parse(c.registry)
			g := &realmGuard{base: base, next: roundTrip(func(*http.Request) *http.Response {
				return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": {c.challenge}}, Body: http.NoBody}
			})}
			req := httptest.NewRequest(http.MethodGet, c.registry+"/v2/", nil)
			resp, err := g.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if refused := errors.Is(err, errRealm); refused != c.refused {
				t.Errorf("refused = %v (%v), want %v", refused, err, c.refused)
			}
		})
	}
}

// roundTrip is a RoundTripper of a function.
type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

// TestDoRecords sends requests through a registry with Basic auth and a token-shaped answer:
// every round trip is recorded in the cell, the credentials and the query's state are not, and
// the body of the answer is trimmed into the exchange.
func TestDoRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "probe" || p != "s3cret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Location", "/v2/x/blobs/uploads/1?_state=opaque")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	c := newClient(config.Registry{URL: srv.URL, Auth: config.RegistryAuth{Username: "probe", Password: "s3cret"}}, base, nil)
	tp := &tape{base: base, cell: &Cell{}}
	ctx := context.WithValue(context.Background(), tapeKey{}, tp)
	a, err := c.do(ctx, request{method: http.MethodPost, target: "/v2/x/blobs/uploads/?_state=client", body: []byte("abc")})
	if err != nil {
		t.Fatal(err)
	}
	if !a.ok(http.StatusAccepted) || a.location().String() != srv.URL+"/v2/x/blobs/uploads/1?_state=opaque" {
		t.Errorf("answer = %d %v", a.status, a.location())
	}
	if len(tp.cell.Exchanges) != 2 {
		t.Fatalf("exchanges = %d, want the 401 and the 202", len(tp.cell.Exchanges))
	}
	var all strings.Builder
	for _, e := range tp.cell.Exchanges {
		all.WriteString(e.line() + "\n")
	}
	got := all.String()
	for _, want := range []string{"401", "202", "/v2/x/blobs/uploads/1?_state=…", `{"ok": true}`, "3 bytes"} {
		if !strings.Contains(got, want) {
			t.Errorf("exchanges lack %q:\n%s", want, got)
		}
	}
	for _, leak := range []string{"s3cret", "cHJvYmU6czNjcmV0", "opaque", "_state=client"} {
		if strings.Contains(got, leak) {
			t.Errorf("exchanges show %q:\n%s", leak, got)
		}
	}
}

// TestDoTokenAfterChallenge is #1042 for the probe's own client: against a registry whose bearer
// challenge names a scope of its own (Gitea's and Forgejo's scope="*"), the token got after the
// first challenge is found by every later request with the same scopes, which goes out with it
// and meets no 401. Before, every request did and got a 401, then went again with the cached
// token. A challenge without a scope (Harbor's), or with the request's own (distribution's), is
// unchanged: one 401, then the token from the cache. The ping, which has no scopes of its own,
// still meets the registry's challenge as before: on Gitea each time.
func TestDoTokenAfterChallenge(t *testing.T) {
	const name = "lajstrom-probe-0/a"
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for _, tt := range []struct {
		name  string
		scope func(path string) string // the scope parameter of the challenge to a request of path
		pings int                      // of the two pings after the reads, those without a token
	}{
		{"a scope of its own, as Gitea and Forgejo", func(string) string { return "*" }, 2},
		{"no scope, as Harbor", func(string) string { return "" }, 1},
		{"the request's own scope, as distribution", func(path string) string {
			if n := repositoryOf(path); n != "" {
				return "repository:" + n + ":pull"
			}
			return ""
		}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &tokenRegistry{scope: tt.scope}
			g.srv = httptest.NewServer(g)
			defer g.srv.Close()
			base, _ := url.Parse(g.srv.URL)
			c := newClient(config.Registry{URL: g.srv.URL, Auth: config.RegistryAuth{Username: "probe", Password: "s3cret"}}, base, nil)
			for range 3 {
				sendWant(t, c, http.MethodHead, "/v2/"+name+"/manifests/v1", http.StatusNotFound)
			}
			for range 3 {
				sendWant(t, c, http.MethodHead, "/v2/"+name+"/blobs/"+digest, http.StatusNotFound)
			}
			if got, want := g.take(), []string{"HEAD /v2/" + name + "/manifests/v1"}; !slices.Equal(got, want) {
				t.Errorf("reads without a token: %q, want the first only: %q", got, want)
			}
			for range 2 {
				sendWant(t, c, http.MethodGet, "/v2/", http.StatusOK)
			}
			if got := g.take(); len(got) != tt.pings {
				t.Errorf("pings without a token: %d, want %d", len(got), tt.pings)
			}
		})
	}
}

// tokenRegistry is a registry with a bearer challenge, whose scope parameter scope gives for the
// path of each request, and its token endpoint, /token, which grants the token for the Basic
// credentials probe:s3cret. With the token, /v2/ is 200 and anything else 404. It keeps the
// requests that came without the token.
type tokenRegistry struct {
	srv   *httptest.Server
	scope func(path string) string
	mu    sync.Mutex
	bare  []string // "HEAD /v2/…"
}

func (g *tokenRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		if u, p, ok := r.BasicAuth(); !ok || u != "probe" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"granted"}`)
	case r.Header.Get("Authorization") != "Bearer granted":
		g.mu.Lock()
		g.bare = append(g.bare, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		challenge := fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, g.srv.URL)
		if s := g.scope(r.URL.Path); s != "" {
			challenge += fmt.Sprintf(`,scope="%s"`, s)
		}
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
	case r.URL.Path != "/v2/":
		w.WriteHeader(http.StatusNotFound)
	}
}

// take returns the requests that came without the token since the last take.
func (g *tokenRegistry) take() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := g.bare
	g.bare = nil
	return b
}

// sendWant sends a request of method to target through c and fails unless it is answered want.
func sendWant(t *testing.T, c *client, method, target string, want int) {
	t.Helper()
	a, err := c.do(t.Context(), request{method: method, target: target})
	if err != nil {
		t.Fatal(err)
	}
	if a.status != want {
		t.Fatalf("%s %s = %d, want %d", method, target, a.status, want)
	}
}
