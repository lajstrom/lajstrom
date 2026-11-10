package generic

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
)

// TestBlobRedirectDowngrade checks that the registry's credentials never cross a redirect from
// https to http (threat model P-2). The registry answers both on one host and port, so the
// http hop has the key of the token cache that the https request filled; the hop is refused,
// and no request reaches the registry in clear text.
func TestBlobRedirectDowngrade(t *testing.T) {
	const name = "raw/scratch/notes"
	const repo = "art/" + name
	data := []byte("0123456789")
	d := digest.FromBytes(data)
	ops := []struct {
		name string
		do   func(p *Provider) error
	}{
		{"StatBlob", func(p *Provider) error {
			_, err := p.StatBlob(context.Background(), name, d)
			return err
		}},
		{"OpenBlob without redirect", func(p *Provider) error {
			b, err := p.OpenBlob(context.Background(), name, d, "", false)
			if b.Body != nil {
				_ = b.Body.Close()
			}
			return err
		}},
	}
	for _, mode := range []string{"basic", "bearer"} {
		for _, op := range ops {
			t.Run(mode+" "+op.name, func(t *testing.T) {
				f := newFakeRegistry(t)
				f.authMode = mode
				f.blobs[repo] = map[digest.Digest][]byte{d: data}
				hostport := newDualServer(t, http.HandlerFunc(f.serve))
				f.realm = "https://" + hostport + "/token"
				var mu sync.Mutex
				var plain []string // each request in clear text, with its Authorization
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.TLS == nil {
						mu.Lock()
						plain = append(plain, r.Method+" "+r.URL.Path+" Authorization="+r.Header.Get("Authorization"))
						mu.Unlock()
						return false
					}
					if !strings.Contains(r.URL.Path, "/blobs/") || r.Header.Get("Authorization") == "" {
						return false // the challenge first, so that the cache holds the credentials
					}
					w.Header().Set("Location", "http://"+hostport+r.URL.Path)
					w.WriteHeader(http.StatusTemporaryRedirect)
					return true
				}
				p := f.provider(t, nil, func(c *config.Registry) {
					c.URL = "https://" + hostport
					c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass}
				})
				err := op.do(p)
				mu.Lock()
				defer mu.Unlock()
				if len(plain) > 0 {
					t.Errorf("requests over http after the redirect: %q", plain)
				}
				if err == nil || !strings.Contains(err.Error(), "from https to http refused") {
					t.Errorf("%s through a redirect to http: %v, want it refused", op.name, err)
				}
				if f.count("HEAD /v2/")+f.count("GET /v2/"+repo) == 0 {
					t.Error("the registry got no request over https")
				}
			})
		}
	}
}

// TestBlobRedirectLeavesOrigin checks that a redirect chain that has left the registry's
// origin goes on without credentials, even back to the registry: the object store sends the
// request back, and the registry sees it without an Authorization.
func TestBlobRedirectLeavesOrigin(t *testing.T) {
	const name = "raw/scratch/notes"
	const repo = "art/" + name
	data := []byte("0123456789")
	d := digest.FromBytes(data)
	f := newFakeRegistry(t)
	f.authMode = "basic"
	f.blobs[repo] = map[digest.Digest][]byte{d: data}
	var mu sync.Mutex
	var store []string
	back := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		store = append(store, r.Method+" "+r.URL.Path+" Authorization="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Location", f.srv.URL+r.URL.Path+"?followed=1")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(back.Close)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/blobs/") || r.URL.Query().Get("followed") == "1" || r.Header.Get("Authorization") == "" {
			return false
		}
		w.Header().Set("Location", back.URL+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return true
	}
	p := f.provider(t, nil, func(c *config.Registry) {
		c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass}
	})
	if _, err := p.StatBlob(context.Background(), name, d); err == nil {
		t.Error("StatBlob back on the registry without credentials succeeded, want its 401")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "HEAD /v2/" + repo + "/blobs/" + d.String() + " Authorization="; len(store) != 1 || store[0] != want {
		t.Errorf("object store requests %q, want %q", store, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if last := f.auths[len(f.auths)-1]; last != "" || !strings.Contains(f.requests[len(f.requests)-1], "followed=1") {
		t.Errorf("the hop back to the registry, %q, carried Authorization %q", f.requests[len(f.requests)-1], last)
	}
}

// TestTokenRealm checks which token realms of a bearer challenge get the registry's
// credentials (threat model P-3): the registry's host name, on any port; not another host
// name, not http for a registry on https, not a realm that is no URL. A refused realm gets no
// request, and the error names the registry, not a URL.
func TestTokenRealm(t *testing.T) {
	tests := []struct {
		name  string
		tls   bool                           // the registry is on https
		realm func(own, token string) string // own: the registry's URL, token: the token server's
		ok    bool
	}{
		{name: "the registry's own origin", realm: func(own, _ string) string { return own + "/token" }, ok: true},
		{name: "another port of the registry's host", realm: func(_, token string) string { return token + "/token" }, ok: true},
		{name: "another host name", realm: func(_, token string) string {
			return strings.Replace(token, "127.0.0.1", "localhost", 1) + "/token"
		}},
		{name: "http for a registry on https", tls: true, realm: func(_, token string) string { return token + "/token" }},
		{name: "not a URL", realm: func(string, string) string { return "token" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.authMode = "bearer"
			var tokens atomic.Int32
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokens.Add(1)
				f.serveToken(w, r)
			}))
			t.Cleanup(token.Close)
			own := f.srv.URL
			if tt.tls {
				srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
				t.Cleanup(srv.Close)
				pool := x509.NewCertPool()
				pool.AddCert(srv.Certificate())
				trusting(t, pool)
				own = srv.URL
			}
			f.realm = tt.realm(own, token.URL)
			p := f.provider(t, nil, func(c *config.Registry) {
				c.URL = own
				c.Auth = config.RegistryAuth{Username: f.user, Password: f.pass}
			})
			err := p.Ping(context.Background())
			n := int(tokens.Load()) + f.count("GET /token")
			if tt.ok {
				if err != nil || n != 1 {
					t.Errorf("Ping = %v with %d token requests, want success with one", err, n)
				}
				return
			}
			if err == nil {
				t.Fatalf("Ping with the realm %s succeeded", f.realm)
			}
			if msg := err.Error(); !strings.Contains(msg, `registry "fake"`) || strings.Contains(msg, "://") || strings.Contains(msg, f.realm) {
				t.Errorf("error %q, want it to name the registry and no URL", msg)
			}
			if n != 0 {
				t.Errorf("%d token requests to a refused realm", n)
			}
		})
	}
}

// newDualServer serves h on one port of 127.0.0.1 over TLS and over plain HTTP, told apart by
// the first byte of each connection (22 opens a TLS handshake), and returns its host:port. The
// providers built until the end of the test trust its certificate.
func newDualServer(t *testing.T, h http.Handler) string {
	t.Helper()
	tmp := httptest.NewUnstartedServer(nil)
	tmp.StartTLS() // for its certificate
	cfg := &tls.Config{Certificates: tmp.TLS.Certificates, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
	pool := x509.NewCertPool()
	pool.AddCert(tmp.Certificate())
	tmp.Close()
	trusting(t, pool)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ErrorLog: slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)}
	go func() { _ = srv.Serve(sniffListener{Listener: l, tls: cfg}) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String()
}

// sniffListener hands out its connections as TLS or plain by their first byte.
type sniffListener struct {
	net.Listener
	tls *tls.Config
}

func (l sniffListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	first, err := r.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	pc := &peekedConn{Conn: c, r: r}
	if err == nil && first[0] == 22 {
		return tls.Server(pc, l.tls), nil
	}
	return pc, nil
}

// peekedConn is a connection whose first bytes were read into r.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// trusting makes the providers built until the end of the test trust the certificates of
// pool: New clones http.DefaultTransport. The tests of this package do not run in parallel.
func trusting(t *testing.T, pool *x509.CertPool) {
	t.Helper()
	old := http.DefaultTransport
	tr := old.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = old })
}
