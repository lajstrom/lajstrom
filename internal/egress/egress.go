// Package egress decides where a client of Lajstrom's may connect when a host it does not
// choose names the target: a redirect of an upstream, a URL in an upstream's document. The
// hosts the operator configured, and the egress proxies the environment chooses, are dialed as
// they are; any other host only over https and only at public unicast addresses, so that an
// upstream cannot send Lajstrom to loopback, a private network or the cloud metadata endpoint
// (docs/spec/proxy-and-group.md §9.2, threat model S-1).
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ErrRefused marks a target a Guard does not connect to.
var ErrRefused = errors.New("egress: refused target")

// cgnat is the shared address space of carrier-grade NAT, 100.64.0.0/10.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// Public reports whether ip is a public unicast address: not loopback, private (RFC 1918, RFC
// 4193), link-local, CGNAT, unspecified, multicast or broadcast.
func Public(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified(), cgnat.Contains(ip), ip.Equal(net.IPv4bcast):
		return false
	}
	return ip.IsGlobalUnicast()
}

// Guard holds the hosts that a client dials as they are: those the operator configured, and
// the egress proxies the environment chose. It is safe for concurrent use.
type Guard struct {
	mu      sync.Mutex
	trusted map[string]bool
}

// NewGuard returns a Guard that trusts the host names hosts, compared without case and
// without a final dot.
func NewGuard(hosts ...string) *Guard {
	g := &Guard{trusted: map[string]bool{}}
	for _, h := range hosts {
		g.trusted[canonical(h)] = true
	}
	return g
}

func canonical(host string) string { return strings.ToLower(strings.TrimSuffix(host, ".")) }

// Trusted reports whether host is dialed as it is.
func (g *Guard) Trusted(host string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.trusted[canonical(host)]
}

// CheckURL allows a URL on a trusted host, and one on any other host that is https and, when
// the host is an address, a public one; the dial checks the addresses a name resolves to.
func (g *Guard) CheckURL(u *url.URL) error {
	if g.Trusted(u.Hostname()) {
		return nil
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: a URL on another host than the configured one must be https", ErrRefused)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !Public(ip) {
		return fmt.Errorf("%w: the address %s is not public", ErrRefused, ip)
	}
	return nil
}

// Proxy wraps the choice of an egress proxy: the hosts of the proxies it chooses are trusted,
// since they are the environment's and not a target. With an egress proxy, the client does not
// resolve the target: CheckURL refuses an address literal, and the proxy enforces the rest.
func (g *Guard) Proxy(choose func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		p, err := choose(req)
		if err == nil && p != nil {
			g.mu.Lock()
			g.trusted[canonical(p.Hostname())] = true
			g.mu.Unlock()
		}
		return p, err
	}
}

// DialContext dials a trusted host as d does, and any other only at public addresses: every
// address the name resolves to must be public, and the dial goes to those addresses, not to
// the name again, so that a second resolution cannot change the answer.
func (g *Guard) DialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if g.Trusted(host) {
			return d.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !Public(ip) {
				return nil, fmt.Errorf("%w: %s resolves to %s, which is not public", ErrRefused, host, ip)
			}
		}
		var last error
		for _, ip := range ips {
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}
