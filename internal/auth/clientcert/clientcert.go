// Package clientcert turns a client certificate into the principal of docs/spec/rbac.md §9: the
// bundles of auth.clientCertificates, the verification of a chain against them, the name each
// bundle takes from the leaf, and the header in which an edge forwards a certificate.
//
// It is written on crypto/x509. No certificate it reads is logged or put in an error: an error
// names the bundle and the rule that failed.
package clientcert

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
)

// ErrRefused means a certificate is no credential: no bundle verifies it, or the bundle that
// does finds no name in it. The message is for the log, never the client.
var ErrRefused = errors.New("clientcert: refused")

// Bundle is a bundle of auth.clientCertificates: its CAs and how it names a certificate.
type Bundle struct {
	// Name is the bundle's name, in cert:<name>/<CN or DNS name>.
	Name string
	// Identity is config.CertIdentitySPIFFE, config.CertIdentityCN or config.CertIdentityDNS.
	Identity string
	// TrustDomain is the SPIFFE trust domain of a spiffe bundle.
	TrustDomain string

	roots *x509.CertPool
	cas   []*x509.Certificate
}

// LoadBundle reads the CA file of b and returns the bundle.
func LoadBundle(b config.CertBundle) (*Bundle, error) {
	data, err := os.ReadFile(b.CAFile)
	if err != nil {
		return nil, fmt.Errorf("clientcert: bundle %s: %w", b.Name, err)
	}
	return NewBundle(b, data)
}

// NewBundle returns the bundle b with the CA certificates of pemData, PEM blocks of type
// CERTIFICATE; anything else in it is an error, and so is no certificate at all. An identity ""
// is spiffe.
func NewBundle(b config.CertBundle, pemData []byte) (*Bundle, error) {
	out := &Bundle{Name: b.Name, Identity: b.Identity, TrustDomain: b.TrustDomain, roots: x509.NewCertPool()}
	if out.Identity == "" {
		out.Identity = config.CertIdentitySPIFFE
	}
	for rest := pemData; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("clientcert: bundle %s: a PEM block of type %q, not CERTIFICATE", b.Name, block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("clientcert: bundle %s: %w", b.Name, err)
		}
		out.roots.AddCert(c)
		out.cas = append(out.cas, c)
	}
	if len(out.cas) == 0 {
		return nil, fmt.Errorf("clientcert: bundle %s: no PEM certificate in its CA file", b.Name)
	}
	return out, nil
}

// Verifier verifies client certificates against the bundles, in their order. Create it with
// NewVerifier; its methods may be called concurrently.
type Verifier struct {
	bundles []*Bundle
	now     func() time.Time
}

// NewVerifier returns the verifier of bundles; now is the clock of the verification, nil
// time.Now.
func NewVerifier(bundles []*Bundle, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{bundles: bundles, now: now}
}

// Pool returns the CAs of every bundle: the acceptable CAs the TLS listener names to its
// clients, and verifies their certificates with first.
func (v *Verifier) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	for _, b := range v.bundles {
		for _, c := range b.cas {
			pool.AddCert(c)
		}
	}
	return pool
}

// Verify returns the principal of chain, the leaf first and the intermediates that came with it
// after, and the name of the bundle that verified it (docs/spec/rbac.md §9). The bundles are
// tried in order; the first whose CAs verify the chain at the clock for client authentication
// decides, and a leaf that is a CA is refused. Every refusal wraps ErrRefused.
func (v *Verifier) Verify(chain []*x509.Certificate) (auth.Principal, string, error) {
	if len(chain) == 0 {
		return auth.Principal{}, "", fmt.Errorf("%w: no certificate", ErrRefused)
	}
	leaf := chain[0]
	if leaf.IsCA {
		return auth.Principal{}, "", fmt.Errorf("%w: the leaf is a CA", ErrRefused)
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	for _, b := range v.bundles {
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots: b.roots, Intermediates: intermediates, CurrentTime: v.now(),
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
		if err != nil {
			continue
		}
		p, err := b.principal(leaf)
		if err != nil {
			return auth.Principal{}, b.Name, fmt.Errorf("%w: bundle %s: %w", ErrRefused, b.Name, err)
		}
		return p, b.Name, nil
	}
	return auth.Principal{}, "", fmt.Errorf("%w: no bundle verifies the certificate", ErrRefused)
}

// principal returns the principal of leaf by the bundle's identity: its one URI SAN, a SPIFFE ID
// of the trust domain; its CN; or its first DNS SAN.
func (b *Bundle) principal(leaf *x509.Certificate) (auth.Principal, error) {
	switch b.Identity {
	case config.CertIdentityCN, config.CertIdentityDNS:
		name := leaf.Subject.CommonName
		if b.Identity == config.CertIdentityDNS {
			if len(leaf.DNSNames) == 0 {
				return auth.Principal{}, errors.New("the certificate has no DNS name")
			}
			name = leaf.DNSNames[0]
		}
		if err := auth.CheckCertificateName(name); err != nil {
			return auth.Principal{}, err
		}
		return auth.Principal{Kind: auth.KindCert, Name: b.Name + "/" + name}, nil
	}
	if len(leaf.URIs) != 1 {
		return auth.Principal{}, fmt.Errorf("the certificate has %d URI SANs, not one SPIFFE ID", len(leaf.URIs))
	}
	id := leaf.URIs[0]
	if id.Scheme != "spiffe" || id.Host != b.TrustDomain || id.User != nil || id.RawQuery != "" || id.Fragment != "" || id.Opaque != "" {
		return auth.Principal{}, fmt.Errorf("the URI SAN is not a SPIFFE ID of the trust domain %s", b.TrustDomain)
	}
	s := spiffeString(id)
	if err := auth.CheckSPIFFEID(s); err != nil {
		return auth.Principal{}, err
	}
	return auth.Principal{Kind: auth.KindCert, Name: s}, nil
}

// spiffeString is the SPIFFE ID of u as the certificate spells it: its raw path, so that an
// escaped character is refused by the grammar rather than decoded.
func spiffeString(u *url.URL) string {
	return "spiffe://" + u.Host + u.EscapedPath()
}
