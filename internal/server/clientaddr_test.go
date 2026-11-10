package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, s ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

func TestResolveClient(t *testing.T) {
	edge := prefixes(t, "10.0.0.0/8", "2001:db8:e::/48")
	tests := []struct {
		name    string
		remote  string
		xff     []string
		trusted []netip.Prefix
		want    client
		bad     bool
	}{
		{name: "no list", remote: "192.0.2.7:51234", xff: []string{"198.51.100.1"}, want: client{addr: "192.0.2.7"}},
		{name: "untrusted peer", remote: "192.0.2.7:51234", xff: []string{"198.51.100.1"}, trusted: edge, want: client{addr: "192.0.2.7"}},
		{name: "trusted peer without the header", remote: "10.1.2.3:443", trusted: edge, want: client{addr: "10.1.2.3"}},
		{name: "trusted peer, empty header", remote: "10.1.2.3:443", xff: []string{" , "}, trusted: edge, want: client{addr: "10.1.2.3"}},
		{name: "one hop", remote: "10.1.2.3:443", xff: []string{"198.51.100.1"}, trusted: edge,
			want: client{addr: "198.51.100.1", proxy: "10.1.2.3"}},
		{name: "a forgery at the left is not read", remote: "10.1.2.3:443", xff: []string{"203.0.113.9, 198.51.100.1"}, trusted: edge,
			want: client{addr: "198.51.100.1", proxy: "10.1.2.3"}},
		{name: "trusted hops skipped from the right", remote: "10.1.2.3:443", xff: []string{"203.0.113.9, 198.51.100.1, 10.9.9.9", "10.8.8.8"}, trusted: edge,
			want: client{addr: "198.51.100.1", proxy: "10.1.2.3"}},
		{name: "every hop trusted: the leftmost", remote: "10.1.2.3:443", xff: []string{"10.7.7.7, 10.8.8.8"}, trusted: edge,
			want: client{addr: "10.7.7.7", proxy: "10.1.2.3"}},
		{name: "everything trusted", remote: "192.0.2.7:1", xff: []string{"203.0.113.9, 198.51.100.1"}, trusted: prefixes(t, "0.0.0.0/0"),
			want: client{addr: "203.0.113.9", proxy: "192.0.2.7"}},
		{name: "a port in an entry", remote: "10.1.2.3:443", xff: []string{"198.51.100.1:5678"}, trusted: edge,
			want: client{addr: "198.51.100.1", proxy: "10.1.2.3"}},
		{name: "IPv6", remote: "[2001:db8:e::5]:443", xff: []string{"[2001:db8:c::1]:5678"}, trusted: edge,
			want: client{addr: "2001:db8:c::1", proxy: "2001:db8:e::5"}},
		{name: "IPv4-mapped peer and hop", remote: "[::ffff:10.1.2.3]:443", xff: []string{"::ffff:198.51.100.1"}, trusted: edge,
			want: client{addr: "198.51.100.1", proxy: "10.1.2.3"}},
		{name: "garbage on the walk", remote: "10.1.2.3:443", xff: []string{"198.51.100.1, unknown"}, trusted: edge,
			want: client{addr: "10.1.2.3"}, bad: true},
		{name: "garbage left of the client is never read", remote: "10.1.2.3:443", xff: []string{"<script>, 198.51.100.1"}, trusted: edge,
			want: client{addr: "198.51.100.1", proxy: "10.1.2.3"}},
		{name: "not host:port", remote: "@", trusted: edge, want: client{addr: "@"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, bad := resolveClient(tt.remote, tt.xff, tt.trusted)
			if got != tt.want || bad != tt.bad {
				t.Errorf("resolveClient(%q, %q) = %+v, %v; want %+v, %v", tt.remote, tt.xff, got, bad, tt.want, tt.bad)
			}
		})
	}
}

// TestClientAddress checks the middleware: the address in the context, the access log line
// with the client as remoteAddr and the peer as proxy, and the startup line.
func TestClientAddress(t *testing.T) {
	tests := []struct {
		name      string
		trusted   []netip.Prefix
		tls       bool
		remote    string
		wantAddr  string
		wantLog   string
		wantProxy any
		wantStart string
	}{
		{name: "no list", remote: "10.1.2.3:443", wantAddr: "10.1.2.3", wantLog: "10.1.2.3:443",
			wantStart: "server.trustedProxies is empty: X-Forwarded-For is ignored, the client address is the peer of the connection, " +
				"and the per-address throttle of failed logins and /v2/token is off; behind an edge, the log records the edge's address"},
		{name: "behind a trusted proxy", trusted: prefixes(t, "10.0.0.0/8"), remote: "10.1.2.3:443", wantAddr: "198.51.100.1",
			wantLog: "198.51.100.1", wantProxy: "10.1.2.3:443",
			wantStart: "the client address is read from X-Forwarded-For when the peer is a trusted proxy, " +
				"and the per-address throttle of failed logins and /v2/token is on"},
		{name: "server.tls, no list", tls: true, remote: "10.1.2.3:443", wantAddr: "10.1.2.3", wantLog: "10.1.2.3:443",
			wantStart: "server.tls is set and server.trustedProxies is empty: X-Forwarded-For is ignored, the client address " +
				"is the peer of the connection, and the per-address throttle of failed logins and /v2/token is on with it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{TrustedProxies: tt.trusted}
			if tt.tls {
				o.TLSCertFile, o.TLSKeyFile, _ = selfSigned(t)
			}
			s, buf := newTestServer(t, o)
			var got string
			s.mux.HandleFunc("GET /test/client", func(_ http.ResponseWriter, r *http.Request) { got = ClientAddr(r.Context()) })
			req := httptest.NewRequest(http.MethodGet, "/test/client", nil)
			req.RemoteAddr = tt.remote
			req.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.1")
			s.Handler().ServeHTTP(httptest.NewRecorder(), req)
			if got != tt.wantAddr {
				t.Errorf("ClientAddr = %q, want %q", got, tt.wantAddr)
			}
			l := findLog(t, buf, "request")
			if l["remoteAddr"] != tt.wantLog || l["proxy"] != tt.wantProxy {
				t.Errorf("access log remoteAddr %v, proxy %v; want %v, %v", l["remoteAddr"], l["proxy"], tt.wantLog, tt.wantProxy)
			}
			s.logTrust()
			findLog(t, buf, tt.wantStart)
		})
	}
	if got := ClientAddr(context.Background()); got != "" {
		t.Errorf("ClientAddr outside the middleware = %q", got)
	}
}
