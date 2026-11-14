package generic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// newUpstream returns NewUpstream of the fake with cred.
func newUpstream(t *testing.T, f *fakeRegistry, name string, cred Credential, edit func(*config.Upstream)) *Provider {
	t.Helper()
	cfg := config.Upstream{Name: name, Format: config.FormatOCI, URL: f.srv.URL}
	if edit != nil {
		edit(&cfg)
	}
	log, _ := testLogger()
	p, err := NewUpstream(cfg, cred, log)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	return p
}

// resolve resolves ref in the repository name of p.
func resolve(p *Provider, name, ref string) (ocispec.Descriptor, error) {
	rr, err := p.Repository(context.Background(), name)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	return rr.Resolve(context.Background(), ref)
}

// TestUpstreamNotFound checks the error rule of an upstream (proxy-and-group.md §11.7): Docker
// Hub grants a token and answers the read of a repository that does not exist 401, which is
// provider.ErrNotFound; a refused token request with credentials stays ErrUnauthorized, and
// without credentials any 401 or 403 is ErrNotFound. A read refused for a quota is
// ErrNotFound too, where a backend's provider has ErrLimit (#357).
func TestUpstreamNotFound(t *testing.T) {
	f := newFakeRegistry(t)
	f.authMode = "bearer"
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/library/missing/"):
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+f.srv.URL+`/token",error="insufficient_scope"`)
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return true
		case strings.HasPrefix(r.URL.Path, "/v2/library/quota/"):
			writeErr(w, http.StatusForbidden, "DENIED", "pull quota exceeded for this namespace")
			return true
		case r.URL.Path == "/token" && r.Header.Get("Authorization") == "":
			writeErr(w, http.StatusForbidden, "DENIED", "requested access to the resource is denied")
			return true
		}
		return false
	}
	user := Credential{Username: f.user, Password: f.pass}
	for _, tc := range []struct {
		name string
		cred Credential
		repo string
		want error
	}{
		{"a repository that does not exist", user, "library/missing", provider.ErrNotFound},
		{"a refused token request", Credential{Username: f.user, Password: "wrong"}, "library/nginx", provider.ErrUnauthorized},
		{"a refused anonymous token request", Credential{}, "library/nginx", provider.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolve(newUpstream(t, f, "hub", tc.cred, nil), tc.repo, "1.27")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Resolve: %v, want %v", err, tc.want)
			}
		})
	}
	// A backend's provider keeps ErrUnauthorized for the same answer.
	p := f.provider(t, nil, func(c *config.Registry) {
		c.Prefix, c.Auth = "", config.RegistryAuth{Username: f.user, Password: f.pass}
	})
	if _, err := resolve(p, "library/missing", "1.27"); !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("a backend's Resolve: %v, want ErrUnauthorized", err)
	}
	// A read refused for a quota, with a body (tags/list, a GET): an upstream's is ErrNotFound,
	// a refused read like any other, and a backend's ErrLimit (#357).
	if err := tagsOf(newUpstream(t, f, "hub", user, nil), "library/quota"); !errors.Is(err, provider.ErrNotFound) ||
		errors.Is(err, provider.ErrLimit) {
		t.Fatalf("an upstream's tags refused for a quota: %v, want ErrNotFound", err)
	}
	if err := tagsOf(p, "library/quota"); !errors.Is(err, provider.ErrLimit) {
		t.Fatalf("a backend's tags refused for a quota: %v, want ErrLimit", err)
	}
}

// tagsOf lists the tags of the repository name of p.
func tagsOf(p *Provider, name string) error {
	rr, err := p.Repository(context.Background(), name)
	if err != nil {
		return err
	}
	_, err = listTags(context.Background(), rr)
	return err
}

// TestUpstreamToken checks a static bearer token: sent at the challenge, with no token request.
func TestUpstreamToken(t *testing.T) {
	f := newFakeRegistry(t)
	f.authMode = "bearer"
	f.token = "static-t0ken"
	data := []byte(`{"schemaVersion":2}`)
	d := digest.FromBytes(data)
	f.manifests["library/nginx"] = map[string]*fakeManifest{"1.27": {desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(data))}, content: data}}
	desc, err := resolve(newUpstream(t, f, "token", Credential{Token: "static-t0ken"}, nil), "library/nginx", "1.27")
	if err != nil || desc.Digest != d {
		t.Fatalf("Resolve: %v, %v", desc, err)
	}
	if n := f.count("GET /token"); n != 0 {
		t.Errorf("%d token requests, want none", n)
	}
}

// rotating is a credential that expires and is replaced, as a cloud's: Basic hands out the
// current password, or err.
type rotating struct {
	mu    sync.Mutex
	pass  string
	err   error
	calls int
}

func (r *rotating) set(pass string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pass, r.err = pass, err
}

func (r *rotating) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *rotating) basic(context.Context) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return "AWS", r.pass, r.err
}

// TestUpstreamBasic checks a rotating credential (Credential.Basic): every request answers a
// Basic challenge with the current password, so a replaced one is sent from the next request on
// without a 401 first; a token realm gets it too; a credential that cannot be had is
// ErrUnauthorized, not the ErrNotFound of an anonymous upstream, and its error shows no secret.
func TestUpstreamBasic(t *testing.T) {
	data := []byte(`{"schemaVersion":2}`)
	d := digest.FromBytes(data)
	manifests := map[string]*fakeManifest{"1.27": {desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(data))}, content: data}}
	cred := &rotating{pass: "token-1"}

	f := newFakeRegistry(t)
	f.manifests["library/nginx"] = manifests
	var mu sync.Mutex
	want, refused := "token-1", 0
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if u, p, ok := r.BasicAuth(); ok && u == "AWS" && p == want {
			return false
		}
		refused++
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return true
	}
	p := newUpstream(t, f, "ecr", Credential{Basic: cred.basic}, nil)
	if desc, err := resolve(p, "library/nginx", "1.27"); err != nil || desc.Digest != d {
		t.Fatalf("Resolve: %v, %v", desc, err)
	}
	mu.Lock()
	want, refused = "token-2", 0
	mu.Unlock()
	cred.set("token-2", nil)
	if desc, err := resolve(p, "library/nginx", "1.27"); err != nil || desc.Digest != d {
		t.Fatalf("Resolve after the rotation: %v, %v", desc, err)
	}
	mu.Lock()
	if refused != 0 {
		t.Errorf("%d requests refused after the rotation, want none", refused)
	}
	mu.Unlock()

	cred.set("", errors.New("ecr: get authorization token: not allowed"))
	_, err := resolve(p, "library/nginx", "1.27")
	if !errors.Is(err, provider.ErrUnauthorized) || strings.Contains(err.Error(), "token-2") {
		t.Errorf("Resolve without a credential: %v, want ErrUnauthorized without the secret", err)
	}

	// A token realm: the token request carries the current credential.
	b := newFakeRegistry(t)
	b.authMode, b.user, b.pass, b.token = "bearer", "AWS", "token-3", "registry-t0ken"
	b.manifests["library/nginx"] = manifests
	cred.set("token-3", nil)
	before := cred.count()
	if desc, err := resolve(newUpstream(t, b, "gar", Credential{Basic: cred.basic}, nil), "library/nginx", "1.27"); err != nil || desc.Digest != d {
		t.Fatalf("Resolve through the realm: %v, %v", desc, err)
	}
	if b.count("GET /token") == 0 || cred.count() == before {
		t.Errorf("token requests %d, Basic asked %d times: want the realm asked with the credential", b.count("GET /token"), cred.count()-before)
	}
}

// TestUpstreamRealmHosts checks that tokenRealmHosts reach the realm rule: a realm on a listed
// host must be https, one on another host is refused, naming it.
func TestUpstreamRealmHosts(t *testing.T) {
	f := newFakeRegistry(t)
	f.authMode = "bearer"
	_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	f.realm = "http://localhost:" + port + "/token"
	for _, tc := range []struct {
		hosts []string
		want  string
	}{
		{[]string{"localhost"}, `"localhost" of its tokenRealmHosts but not https`},
		{nil, `the host "localhost", neither the registry's nor one of its tokenRealmHosts`},
	} {
		p := newUpstream(t, f, "realm", Credential{}, func(c *config.Upstream) { c.TokenRealmHosts = tc.hosts })
		if _, err := resolve(p, "library/nginx", "1.27"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("tokenRealmHosts %q: %v, want %q", tc.hosts, err, tc.want)
		}
	}
}

// TestUpstreamRateLimit checks the gauges of Docker Hub's rate-limit headers, the window read
// from w=, and the warning below 10 %.
func TestUpstreamRateLimit(t *testing.T) {
	f := newFakeRegistry(t)
	remaining := "76;w=21600"
	f.hook = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("ratelimit-limit", "100;w=21600")
		w.Header().Set("ratelimit-remaining", remaining)
		return false
	}
	log, logs := testLogger()
	p, err := NewUpstream(config.Upstream{Name: "ratelimited", Format: config.FormatOCI, URL: f.srv.URL}, Credential{}, log)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = resolve(p, "library/nginx", "1.27")
	for _, g := range []struct {
		name      string
		got, want float64
	}{
		{"limit", testutil.ToFloat64(metrics.ProxyRateLimit.WithLabelValues("ratelimited")), 100},
		{"remaining", testutil.ToFloat64(metrics.ProxyRateRemaining.WithLabelValues("ratelimited")), 76},
		{"window", testutil.ToFloat64(metrics.ProxyRateWindow.WithLabelValues("ratelimited")), 21600},
	} {
		if g.got != g.want {
			t.Errorf("%s = %v, want %v", g.name, g.got, g.want)
		}
	}
	if strings.Contains(logs.String(), "nearly spent") {
		t.Errorf("a warning at 76 of 100:\n%s", logs)
	}
	remaining = "9;w=21600"
	_, _ = resolve(p, "library/nginx", "1.27")
	_, _ = resolve(p, "library/nginx", "1.27")
	if n := strings.Count(logs.String(), "nearly spent"); n != 1 {
		t.Errorf("%d warnings at 9 of 100, want one:\n%s", n, logs)
	}
}

func TestRateLimitHeader(t *testing.T) {
	for _, tc := range []struct {
		v         string
		n, window int64
		ok        bool
	}{
		{"100;w=21600", 100, 21600, true}, {"100;w=3600", 100, 3600, true}, {" 7 ", 7, 0, true},
		{"0;w=21600", 0, 21600, true}, {"", 0, 0, false}, {"many", 0, 0, false}, {"-1", 0, 0, false},
	} {
		n, window, ok := rateLimitHeader(tc.v)
		if n != tc.n || window != tc.window || ok != tc.ok {
			t.Errorf("rateLimitHeader(%q) = %d, %d, %t, want %d, %d, %t", tc.v, n, window, ok, tc.n, tc.window, tc.ok)
		}
	}
}
