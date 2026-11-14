package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The FQDN mode of the tests: the repo hosts are below art.test, and the main host, the host of
// the external URL, is below it too.
const (
	fqdnDomain      = "art.test"
	fqdnExternalURL = "https://reg.art.test:8443"
)

// TestRepoHostMatch reads Host headers as the FQDN mode does (docs/spec/http-routing.md §1.1):
// the main host, every host outside the domain and the domain are the main host; below the
// domain, one label or a label and a served format, the repo label a DNS label; anything else
// below the domain is no repo host. Forged hosts that only contain the domain stay the main host.
func TestRepoHostMatch(t *testing.T) {
	f, err := newFQDN(fqdnDomain, fqdnExternalURL, []format.Plugin{stubPlugin("npm"), stubPlugin("pypi")})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 63)
	for _, tt := range []struct {
		host          string
		below         bool
		label, format string
		refused       bool // below the domain, but no repo host
	}{
		{host: "reg.art.test"},
		{host: "REG.Art.Test:8443"},
		{host: "reg.art.test."},
		{host: "art.test"},
		{host: "art.test.:443"},
		{host: ""},
		{host: "evil.test"},
		{host: "internal.art.test.evil.test"},
		{host: "internalart.test"},
		{host: "art.test.internal"},
		{host: "127.0.0.1:8080"},
		{host: "[::1]:8080"},
		{host: "[::1]"},
		{host: "internal.art.test", below: true, label: "internal"},
		{host: "Internal.ART.test.", below: true, label: "internal"},
		{host: "internal.art.test:8443", below: true, label: "internal"},
		{host: "internal.art.test:1", below: true, label: "internal"},
		{host: "npm.art.test", below: true, label: "npm"},
		{host: "my-repo2.art.test", below: true, label: "my-repo2"},
		{host: long + ".art.test", below: true, label: long},
		{host: "internal.npm.art.test", below: true, label: "internal", format: "npm"},
		{host: "internal.PyPI.art.test", below: true, label: "internal", format: "pypi"},
		{host: "internal.oci.art.test", below: true, refused: true},
		{host: "internal.maven.art.test", below: true, refused: true},
		{host: "a.npm.b.art.test", below: true, refused: true},
		{host: "x.reg.art.test", below: true, refused: true},
		{host: ".art.test", below: true, refused: true},
		{host: "my_repo.art.test", below: true, refused: true},
		{host: "-x.art.test", below: true, refused: true},
		{host: "x-.art.test", below: true, refused: true},
		{host: "x--y.art.test", below: true, refused: true},
		{host: long + "a.art.test", below: true, refused: true},
		{host: "x@y.art.test", below: true, refused: true},
		{host: "*.art.test", below: true, refused: true},
	} {
		t.Run(tt.host, func(t *testing.T) {
			m := f.match(tt.host)
			if m.below != tt.below || (m.why != "") != tt.refused {
				t.Fatalf("match(%q) = %+v, want below %t, refused %t", tt.host, m, tt.below, tt.refused)
			}
			if !tt.refused && (m.label != tt.label || m.format != tt.format) {
				t.Errorf("match(%q) = label %q, format %q; want %q, %q", tt.host, m.label, m.format, tt.label, tt.format)
			}
		})
	}
}

// TestNewFQDN: the mode needs the external URL, whose scheme and port the repo hosts take.
func TestNewFQDN(t *testing.T) {
	if _, err := New(Options{Listen: "127.0.0.1:0", FQDNDomain: fqdnDomain}); err == nil || !strings.Contains(err.Error(), "ExternalURL") {
		t.Errorf("New without ExternalURL: %v, want an error naming it", err)
	}
	f, err := newFQDN(fqdnDomain, "https://art.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := f.repoHost(hostMatch{label: "internal"}, index.Repo{Format: "npm", Name: "internal"})
	if h.URL != "https://internal.art.test/" || h.PathURL != "https://art.test/npm/internal/" {
		t.Errorf("repoHost = %+v", h)
	}
	h = f.repoHost(hostMatch{label: "internal", format: "npm"}, index.Repo{Format: "npm", Name: "internal"})
	if h.URL != "https://internal.npm.art.test/" {
		t.Errorf("repoHost of the second form = %+v", h)
	}
}

// fqdnIndex has fake/scratch, a name both formats have, a name only other has, and a repo whose
// name is no DNS label.
func fqdnIndex() *formattest.Index {
	return &formattest.Index{Repos: []index.Repo{
		{ID: 1, Format: "fake", Name: "scratch"},
		{ID: 2, Format: "fake", Name: "shared"},
		{ID: 3, Format: "other", Name: "shared"},
		{ID: 4, Format: "other", Name: "only"},
		{ID: 5, Format: "fake", Name: "my_repo"},
		{ID: 6, Format: "oci", Name: "only"},
	}}
}

// fqdnPlugins are fake and other, the formats of fqdnIndex, with the routes of the tests of the
// Location, which name fake/scratch: a redirect as pypi's, an upload's Location as raw's, a
// Location in the path form of the external URL, one of another repo, and a directory the
// ServeMux redirects to.
func fqdnPlugins() []format.Plugin {
	mount := func(r format.Router, _ format.Deps) {
		r.HandleFunc("GET /{repo}/redirect", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/fake/scratch/simple/x/", http.StatusMovedPermanently)
		})
		r.HandleFunc("PUT /{repo}/upload/{path...}", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Location", req.URL.EscapedPath())
			w.WriteHeader(http.StatusCreated)
		})
		r.HandleFunc("GET /{repo}/absolute", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, fqdnExternalURL+"/fake/scratch/x?y=1", http.StatusFound)
		})
		r.HandleFunc("GET /{repo}/elsewhere", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/fake/scratch-2/x", http.StatusFound)
		})
		r.HandleFunc("GET /{repo}/dir/", func(w http.ResponseWriter, _ *http.Request) {
			writeText(w, http.StatusOK, "dir\n")
		})
	}
	return []format.Plugin{&formattest.Fake{Mount: mount}, &formattest.Fake{FormatName: "other"}}
}

// newFQDNServer is a server of fqdnPlugins over idx in the FQDN mode, with a facade and an API
// that answer 418, so that a test sees a request reach them.
func newFQDNServer(t *testing.T, idx *formattest.Index, authz auth.Authorizer) http.Handler {
	t.Helper()
	teapot := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	s, _ := newTestServer(t, Options{Plugins: fqdnPlugins(), Deps: format.Deps{Index: idx, Auth: authz}, ExternalURL: fqdnExternalURL,
		FQDNDomain: fqdnDomain, Facade: teapot, API: teapot})
	return s.Handler()
}

// hostRequest is a request of method to host and path, as alice when user is set.
func hostRequest(method, host, path string, user bool) *http.Request {
	req := httptest.NewRequest(method, "http://placeholder"+path, nil)
	req.Host = host
	if user {
		req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Kind: auth.KindUser, Name: "alice"}))
	}
	return req
}

// TestRepoHosts serves requests on repo hosts as the path form of their repos, and refuses the
// hosts below the domain that name none: unknown repos, names two formats share, names that are
// no DNS label, formats not served, more labels. No path of a repo host reaches the facade, the
// API or a system endpoint, and forged hosts are the main host.
func TestRepoHosts(t *testing.T) {
	const noRepo = "no repo is served at the host "
	for _, tt := range []struct {
		name, host, path string
		status           int
		body             string // a substring of the body
		lookups          int64  // GetRepo calls, the router's included
	}{
		{name: "main host", host: "reg.art.test", path: "/fake/scratch/ping", status: 200, body: "pong fake/scratch", lookups: 1},
		{name: "repo host", host: "scratch.art.test", path: "/ping", status: 200, body: "pong fake/scratch", lookups: 3},
		{name: "repo host with port", host: "Scratch.Art.Test.:8443", path: "/ping", status: 200, body: "pong fake/scratch", lookups: 3},
		{name: "second form", host: "scratch.fake.art.test", path: "/ping", status: 200, body: "pong fake/scratch", lookups: 2},
		{name: "second form of the other format", host: "shared.other.art.test", path: "/ping", status: 200, body: "pong other/shared", lookups: 2},
		{name: "a name only the other format has", host: "only.art.test", path: "/ping", status: 200, body: "pong other/only", lookups: 3},
		{name: "a name two formats have", host: "shared.art.test", path: "/ping", status: 404, body: noRepo + "shared.art.test", lookups: 2},
		{name: "second form of a repo the format lacks", host: "scratch.other.art.test", path: "/ping", status: 404, body: noRepo, lookups: 1},
		{name: "unknown repo", host: "unknown.art.test", path: "/ping", status: 404, body: noRepo + "unknown.art.test: a repo host is <repo>.art.test", lookups: 2},
		{name: "a name that is no DNS label", host: "my_repo.art.test", path: "/ping", status: 404, body: noRepo},
		{name: "oci is no format served", host: "only.oci.art.test", path: "/ping", status: 404, body: noRepo},
		{name: "more labels", host: "a.scratch.art.test", path: "/ping", status: 404, body: noRepo},
		{name: "v2 stays the repo's", host: "scratch.art.test", path: "/v2/", status: 404, body: "404 page not found", lookups: 2},
		{name: "token endpoint stays the repo's", host: "scratch.art.test", path: "/v2/token", status: 404, lookups: 2},
		{name: "api stays the repo's", host: "scratch.art.test", path: "/api/v1/repos", status: 404, lookups: 2},
		{name: "system endpoints stay the repo's", host: "scratch.art.test", path: "/-/healthz", status: 404, lookups: 2},
		{name: "a path that climbs out", host: "scratch.art.test", path: "/../../v2/", status: 307, body: `href="/v2/"`, lookups: 2},
		{name: "forged host outside the domain", host: "evil.test", path: "/v2/", status: 418},
		{name: "forged host that ends in the domain", host: "scratch.art.test.evil.test", path: "/fake/scratch/ping", status: 200,
			body: "pong fake/scratch", lookups: 1},
		{name: "the domain itself", host: "art.test", path: "/api/v1/repos", status: 418},
		{name: "main host", host: "reg.art.test", path: "/v2/", status: 418},
	} {
		t.Run(tt.name, func(t *testing.T) {
			idx := fqdnIndex()
			h := newFQDNServer(t, idx, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, hostRequest(http.MethodGet, tt.host, tt.path, false))
			if rr.Code != tt.status || !strings.Contains(rr.Body.String(), tt.body) {
				t.Errorf("GET %s%s: %d %q, want %d with %q", tt.host, tt.path, rr.Code, rr.Body, tt.status, tt.body)
			}
			if got := idx.GetRepoCalls.Load(); got != tt.lookups {
				t.Errorf("GetRepo calls: %d, want %d", got, tt.lookups)
			}
		})
	}
}

// denyAll is an authorizer that allows nothing.
type denyAll struct{}

func (denyAll) Allowed(context.Context, auth.Principal, auth.Action, string, string) (bool, error) {
	return false, nil
}

// TestRepoHostRefusal: with an authorizer, a host that names no repo is 401 with the Basic
// challenge to anonymous, as an unknown or a private repo is in the path form, and 404 to a
// user; a lookup the index fails is its 503.
func TestRepoHostRefusal(t *testing.T) {
	for _, tt := range []struct {
		name      string
		user      bool
		status    int
		challenge bool
	}{
		{name: "anonymous", status: 401, challenge: true},
		{name: "user", user: true, status: 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newFQDNServer(t, fqdnIndex(), denyAll{})
			for _, host := range []string{"unknown.art.test", "shared.art.test", "my_repo.art.test"} {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, hostRequest(http.MethodGet, host, "/ping", tt.user))
				if rr.Code != tt.status || (rr.Header().Get("WWW-Authenticate") != "") != tt.challenge {
					t.Errorf("%s: %d %v %q", host, rr.Code, rr.Header(), rr.Body)
				}
			}
		})
	}
	idx := fqdnIndex()
	idx.GetRepoErr = index.ErrUnavailable
	rr := httptest.NewRecorder()
	newFQDNServer(t, idx, nil).ServeHTTP(rr, hostRequest(http.MethodGet, "scratch.art.test", "/ping", false))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Errorf("the index failing: %d %v %q", rr.Code, rr.Header(), rr.Body)
	}
}

// TestRepoHostLocation: on a repo host, a Location below the path form of the repo is written
// below the repo host, whoever set it: a frontend's redirect, an upload's Location, a redirect
// of the ServeMux, an absolute URL under the external URL. A Location of another repo stays,
// and on the main host every Location stays.
func TestRepoHostLocation(t *testing.T) {
	for _, tt := range []struct {
		method, host, path, location string
	}{
		{http.MethodGet, "scratch.art.test", "/redirect", "/simple/x/"},
		{http.MethodGet, "reg.art.test", "/fake/scratch/redirect", "/fake/scratch/simple/x/"},
		{http.MethodPut, "scratch.art.test", "/upload/a/b%20c.txt", "/upload/a/b%20c.txt"},
		{http.MethodPut, "reg.art.test", "/fake/scratch/upload/a/b.txt", "/fake/scratch/upload/a/b.txt"},
		{http.MethodGet, "scratch.art.test", "/dir?q=1", "/dir/?q=1"},
		{http.MethodGet, "scratch.fake.art.test", "/dir", "/dir/"},
		{http.MethodGet, "reg.art.test", "/fake/scratch/dir", "/fake/scratch/dir/"},
		{http.MethodGet, "scratch.art.test", "/absolute", "https://scratch.art.test:8443/x?y=1"},
		{http.MethodGet, "scratch.fake.art.test", "/absolute", "https://scratch.fake.art.test:8443/x?y=1"},
		{http.MethodGet, "reg.art.test", "/fake/scratch/absolute", fqdnExternalURL + "/fake/scratch/x?y=1"},
		{http.MethodGet, "scratch.art.test", "/elsewhere", "/fake/scratch-2/x"},
		{http.MethodGet, "scratch.art.test", "/a/../../../v2/", "/v2/"},
	} {
		t.Run(tt.method+" "+tt.host+tt.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			newFQDNServer(t, fqdnIndex(), nil).ServeHTTP(rr, hostRequest(tt.method, tt.host, tt.path, false))
			if got := rr.Header().Get("Location"); got != tt.location {
				t.Errorf("Location %q (%d), want %q", got, rr.Code, tt.location)
			}
		})
	}
}

// TestRepoHostContext: the handler of a request on a repo host gets its RepoHost, whose URL is
// built from the repo, the domain and the external URL, never from the Host; on the main host
// it gets none. ClientLinks writes the links of the repo below the repo host and leaves those of
// another repo.
func TestRepoHostContext(t *testing.T) {
	var got []RepoHost
	mount := func(r format.Router, _ format.Deps) {
		r.HandleFunc("GET /{repo}/host", func(w http.ResponseWriter, req *http.Request) {
			h, _ := RepoHostFrom(req.Context())
			got = append(got, h)
			doc := `{"tarball":"` + fqdnExternalURL + `/fake/scratch/a/-/a.tgz","x":"` + fqdnExternalURL + `/fake/scratch/b",` +
				`"other":{"tarball":"` + fqdnExternalURL + `/fake/shared/a/-/a.tgz"}}`
			_, _ = w.Write(ClientLinks(req.Context(), []byte(doc), `"tarball":"`))
		})
	}
	s, _ := newTestServer(t, Options{Plugins: []format.Plugin{&formattest.Fake{Mount: mount}}, Deps: format.Deps{Index: fqdnIndex()},
		ExternalURL: fqdnExternalURL, FQDNDomain: fqdnDomain})
	var bodies []string
	for _, host := range []string{"SCRATCH.art.test:1", "reg.art.test"} {
		path := "/host"
		if host == "reg.art.test" {
			path = "/fake/scratch/host"
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, hostRequest(http.MethodGet, host, path, false))
		bodies = append(bodies, rr.Body.String())
	}
	want := RepoHost{Format: "fake", Repo: "scratch", URL: "https://scratch.art.test:8443/", PathURL: fqdnExternalURL + "/fake/scratch/"}
	if len(got) != 2 || got[0] != want || got[1] != (RepoHost{}) {
		t.Fatalf("RepoHost: %+v, want %+v and none", got, want)
	}
	wantBody := `{"tarball":"https://scratch.art.test:8443/a/-/a.tgz","x":"` + fqdnExternalURL + `/fake/scratch/b",` +
		`"other":{"tarball":"` + fqdnExternalURL + `/fake/shared/a/-/a.tgz"}}`
	if bodies[0] != wantBody || !strings.Contains(bodies[1], `"tarball":"`+fqdnExternalURL+`/fake/scratch/a/-/a.tgz"`) {
		t.Errorf("ClientLinks:\n%s\n%s", bodies[0], bodies[1])
	}
}

// certificate returns a TLS certificate for names, self-signed.
func certificate(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestFQDNCertificate: serve warns at start when the certificate of server.tls does not cover
// the repo hosts, which it is sent to whatever the SNI name.
func TestFQDNCertificate(t *testing.T) {
	for _, tt := range []struct {
		names []string
		warn  bool
	}{
		{names: []string{"reg.art.test"}, warn: true},
		{names: []string{"reg.art.test", "*.art.test"}},
		{names: []string{"art.test", "*.npm.art.test"}, warn: true},
	} {
		t.Run(strings.Join(tt.names, ","), func(t *testing.T) {
			s, buf := newTestServer(t, Options{ExternalURL: fqdnExternalURL, FQDNDomain: fqdnDomain})
			s.srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate(t, tt.names...)}, MinVersion: tls.VersionTLS12}
			s.logFQDN()
			if got := strings.Contains(buf.String(), "does not cover the repo hosts"); got != tt.warn {
				t.Errorf("warned %t, want %t:\n%s", got, tt.warn, buf)
			}
		})
	}
}
