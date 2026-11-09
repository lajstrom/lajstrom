package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// client is where a request came from (ADR 0011): addr is the client's address, without a port,
// and proxy the peer of the connection when it is a trusted proxy that was skipped to find
// addr, "" when the peer is the client.
type client struct{ addr, proxy string }

type clientKey struct{}

// ClientAddr returns the address of the client of the request of ctx, without a port: the peer
// of the connection, or the client that X-Forwarded-For names when the peer is a trusted proxy
// (server.trustedProxies, ADR 0011). It is "" for a context that has not been through the
// server's middleware.
func ClientAddr(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(client)
	return c.addr
}

// clientAddress resolves the client of each request once (resolveClient) and puts it in the
// context, for the access log, the authentication and ClientAddr. No other code reads a
// forwarded header.
func (s *Server) clientAddress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, bad := resolveClient(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), s.opts.TrustedProxies)
		if bad {
			s.log.DebugContext(r.Context(), "X-Forwarded-For has an entry that is not an address: the peer is the client",
				"remoteAddr", r.RemoteAddr, "requestId", RequestID(r.Context()))
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, c)))
	})
}

// logTrust says once, at startup, where the client address comes from.
func (s *Server) logTrust() {
	if len(s.opts.TrustedProxies) == 0 {
		s.log.Info("server.trustedProxies is empty: X-Forwarded-For is ignored and the client address is the peer " +
			"of the connection; behind an edge, the log records the edge's address")
		return
	}
	list := make([]string, len(s.opts.TrustedProxies))
	for i, p := range s.opts.TrustedProxies {
		list[i] = p.String()
	}
	s.log.Info("the client address is read from X-Forwarded-For when the peer is a trusted proxy", "trustedProxies", list)
}

// resolveClient returns the client of a request whose connection's peer is remoteAddr
// (host:port) and whose X-Forwarded-For header lines are xff, with trusted the trusted
// proxies (ADR 0011). When the peer is not trusted, or there is no X-Forwarded-For, the peer is
// the client. Otherwise the entries are read from the right: each trusted one is skipped, the
// first that is not is the client, and when every entry is trusted the leftmost is. An entry
// on that walk that is not an address (with or without a port) makes the header untrusted and
// the peer the client, and bad true. The entries left of the client are never read: they are
// whatever the client sent.
func resolveClient(remoteAddr string, xff []string, trusted []netip.Prefix) (c client, bad bool) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		host, _, err := net.SplitHostPort(remoteAddr)
		if err != nil {
			return client{addr: remoteAddr}, false
		}
		return client{addr: host}, false
	}
	peer := ap.Addr().Unmap()
	if !isTrusted(peer, trusted) {
		return client{addr: peer.String()}, false
	}
	var hops []string
	for _, line := range xff {
		for e := range strings.SplitSeq(line, ",") {
			if e = strings.TrimSpace(e); e != "" {
				hops = append(hops, e)
			}
		}
	}
	if len(hops) == 0 {
		return client{addr: peer.String()}, false
	}
	var last netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHop(hops[i])
		if !ok {
			return client{addr: peer.String()}, true
		}
		last = a
		if !isTrusted(a, trusted) {
			break
		}
	}
	return client{addr: last.String(), proxy: peer.String()}, false
}

// parseHop parses an X-Forwarded-For entry: an address, or an address and a port as some edges
// write it (1.2.3.4:5678, [2001:db8::1]:443).
func parseHop(e string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(e); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(e); err == nil {
		return ap.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

// isTrusted reports whether a is in one of the prefixes.
func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.WithZone("")
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
