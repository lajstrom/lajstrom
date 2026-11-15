package server

import (
	"crypto/x509"
	"errors"
	"net/http"
	"net/netip"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/clientcert"
)

// certificate returns the principal and the credential of the client certificate of r, a GET of
// /v2/token without an Authorization header (docs/spec/rbac.md §9), and ok false when r carries
// none. The certificate comes from the TLS connection when its peer is not a trusted proxy, and
// from Options.ClientCertHeader when it is: the certificate of a trusted proxy is never its
// client's, and the header of any other peer is never read. A certificate that does not verify,
// or has no name, is auth.ErrBadCredentials, its reason and bundle logged at debug, never the
// certificate.
func (s *Server) certificate(r *http.Request) (p auth.Principal, cred auth.Credential, ok bool, err error) {
	ctx := r.Context()
	trusted := s.peerTrusted(r.RemoteAddr)
	var chain []*x509.Certificate
	from := "tls"
	switch {
	case !trusted && r.TLS != nil && len(r.TLS.PeerCertificates) > 0:
		chain = r.TLS.PeerCertificates
	case trusted && s.opts.ClientCertHeader != "":
		lines := r.Header.Values(s.opts.ClientCertHeader)
		if len(lines) == 0 || lines[len(lines)-1] == "" {
			return auth.Principal{}, auth.Credential{}, false, nil
		}
		from = "header"
		chain, err = clientcert.ParseHeader(s.opts.ClientCertHeader, lines[len(lines)-1])
	default:
		return auth.Principal{}, auth.Credential{}, false, nil
	}
	bundle := ""
	if err == nil {
		p, bundle, err = s.opts.ClientCerts.Verify(chain)
	}
	if errors.Is(err, clientcert.ErrRefused) {
		s.log.DebugContext(ctx, "client certificate refused", "from", from, "bundle", bundle, "reason", err.Error(),
			"requestId", RequestID(ctx))
		return auth.Principal{}, auth.Credential{}, true, auth.ErrBadCredentials
	}
	if err != nil {
		return auth.Principal{}, auth.Credential{}, true, err
	}
	s.log.DebugContext(ctx, "client certificate", "principal", p.String(), "bundle", bundle, "from", from,
		"requestId", RequestID(ctx))
	return p, auth.Credential{Kind: auth.CredCertificate}, true, nil
}

// peerTrusted reports whether the peer of a connection, remoteAddr (host:port), is in
// server.trustedProxies.
func (s *Server) peerTrusted(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	return err == nil && isTrusted(ap.Addr().Unmap(), s.opts.TrustedProxies)
}
