package server

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/clientcert"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// certServer returns a server over usersReader whose facade records what it sees, with the
// bundle cluster-a of ca (spiffe) and o's other options.
func certServer(t *testing.T, ca *testutil.CA, o Options) (*Server, *seen) {
	t.Helper()
	bundle, err := clientcert.NewBundle(config.CertBundle{Name: "cluster-a", TrustDomain: "cluster-a"}, testutil.PEM(ca.Cert))
	if err != nil {
		t.Fatal(err)
	}
	sw := &seen{}
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sw = seen{called: true, principal: auth.PrincipalFrom(r.Context()), credential: auth.CredentialFrom(r.Context())}
		w.WriteHeader(http.StatusNoContent)
	})
	o.Facade, o.Authenticator = record, auth.NewAuthenticator(auth.Options{Index: usersReader{}})
	o.ClientCerts = clientcert.NewVerifier([]*clientcert.Bundle{bundle}, nil)
	s, _ := newTestServer(t, o)
	return s, sw
}

var node1 = auth.Principal{Kind: auth.KindCert, Name: "spiffe://cluster-a/node/w1"}

// TestClientCertificateTLS: with server.tls and a bundle, a client certificate is the credential
// of a GET of /v2/token without Authorization (docs/spec/rbac.md §9): a client without one is
// anonymous, Basic credentials win over it, other routes do not read it, a certificate of
// another CA fails the handshake, and a trusted proxy's certificate is not its client's.
func TestClientCertificateTLS(t *testing.T) {
	certFile, keyFile, pool := selfSigned(t)
	ca := testutil.NewCA("cluster-a")
	issuer := ca.Intermediate("cluster-a issuer")
	leaf, key := issuer.Issue(testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w1"}})
	stranger, strangerKey := testutil.NewCA("other").Issue(testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w1"}})
	client := func(certs ...tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: certs, MinVersion: tls.VersionTLS12}}}
	}
	for _, tt := range []struct {
		name    string
		proxies []netip.Prefix
		client  *http.Client
		path    string
		authz   string
		want    auth.Principal
		cred    auth.CredentialKind
	}{
		{"a node's certificate", nil, client(testutil.TLS(leaf, key, issuer.Cert)), "/v2/token?scope=x", "", node1, auth.CredCertificate},
		{"no certificate", nil, client(), "/v2/token", "", auth.Anonymous, ""},
		{"Basic wins", nil, client(testutil.TLS(leaf, key, issuer.Cert)), "/v2/token", basic("alice", alicePassword),
			auth.Principal{Kind: auth.KindUser, Name: aliceUID}, auth.CredPassword},
		{"another route", nil, client(testutil.TLS(leaf, key, issuer.Cert)), "/v2/", "", auth.Anonymous, ""},
		{"from a trusted proxy", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, client(testutil.TLS(leaf, key, issuer.Cert)),
			"/v2/token", "", auth.Anonymous, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, sw := certServer(t, ca, Options{TLSCertFile: certFile, TLSKeyFile: keyFile, TrustedProxies: tt.proxies})
			addr, _, _ := start(t, s)
			req, _ := http.NewRequest(http.MethodGet, "https://"+addr+tt.path, nil)
			if tt.authz != "" {
				req.Header.Set("Authorization", tt.authz)
			}
			resp, err := tt.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent || sw.principal != tt.want || sw.credential.Kind != tt.cred {
				t.Errorf("GET %s = %d as %v with %q, want 204 as %v with %q", tt.path, resp.StatusCode, sw.principal, sw.credential.Kind, tt.want, tt.cred)
			}
		})
	}
	// A Go client sends no certificate that chains to none of the acceptable CAs, and goes on
	// anonymous; one that sends it anyway fails the handshake.
	s, sw := certServer(t, ca, Options{TLSCertFile: certFile, TLSKeyFile: keyFile})
	addr, _, _ := start(t, s)
	resp, err := client(testutil.TLS(stranger, strangerKey)).Get("https://" + addr + "/v2/token")
	if err != nil || resp.StatusCode != http.StatusNoContent || sw.principal != auth.Anonymous {
		t.Errorf("a Go client with a certificate of another CA: %v, %v", resp, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	forced := testutil.TLS(stranger, strangerKey)
	insist := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &forced, nil }}}}
	if resp, err := insist.Get("https://" + addr + "/v2/token"); err == nil {
		_ = resp.Body.Close()
		t.Errorf("a certificate of another CA was served: %d", resp.StatusCode)
	}
}

// TestClientCertificateHeader: behind an edge, the certificate comes from the header of a
// trusted proxy only, in nginx's and Envoy's forms; a header that does not verify is 401 with
// the Basic challenge, and one from any other peer is not read.
func TestClientCertificateHeader(t *testing.T) {
	ca := testutil.NewCA("cluster-a")
	leaf, _ := ca.Issue(testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w1"}})
	other, _ := ca.Issue(testutil.CertOptions{URIs: []string{"spiffe://cluster-b/node/w1"}})
	escaped := func(c *x509.Certificate) string { return url.PathEscape(string(testutil.PEM(c))) }
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tt := range []struct {
		name, header, value, peer string
		status                    int
		want                      auth.Principal
	}{
		{"nginx", "X-SSL-Client-Cert", escaped(leaf), "10.1.2.3:5000", http.StatusNoContent, node1},
		{"envoy", config.XForwardedClientCert, `Hash=abc;Cert="` + escaped(leaf) + `";URI=spiffe://cluster-a/node/w1`, "10.1.2.3:5000",
			http.StatusNoContent, node1},
		{"no header", "X-SSL-Client-Cert", "", "10.1.2.3:5000", http.StatusNoContent, auth.Anonymous},
		{"another peer", "X-SSL-Client-Cert", escaped(leaf), "192.0.2.1:5000", http.StatusNoContent, auth.Anonymous},
		{"another trust domain", "X-SSL-Client-Cert", escaped(other), "10.1.2.3:5000", http.StatusUnauthorized, auth.Principal{}},
		{"not a certificate", "X-SSL-Client-Cert", "garbage", "10.1.2.3:5000", http.StatusUnauthorized, auth.Principal{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, sw := certServer(t, ca, Options{TrustedProxies: proxies, ClientCertHeader: tt.header})
			r := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
			r.RemoteAddr = tt.peer
			if tt.value != "" {
				r.Header.Set(tt.header, tt.value)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			if rr.Code != tt.status || sw.principal != tt.want {
				t.Errorf("GET /v2/token = %d as %v, want %d as %v", rr.Code, sw.principal, tt.status, tt.want)
			}
			if tt.status == http.StatusUnauthorized && (sw.called || rr.Header().Get("WWW-Authenticate") != basicChallenge ||
				!strings.Contains(rr.Body.String(), "UNAUTHORIZED")) {
				t.Errorf("a refused certificate: %d %q %q, facade called %v", rr.Code, rr.Header().Get("WWW-Authenticate"), rr.Body, sw.called)
			}
		})
	}
}
