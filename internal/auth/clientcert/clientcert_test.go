package clientcert

import (
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// pki is the fixture of the tests: a SPIFFE CA of the trust domain cluster-a with an
// intermediate, a plain CA, and a CA that no bundle has.
type pki struct {
	spiffe, issuer, plain, other *testutil.CA
}

func newPKI() pki {
	spiffe := testutil.NewCA("cluster-a root")
	return pki{spiffe: spiffe, issuer: spiffe.Intermediate("cluster-a issuer"), plain: testutil.NewCA("corp"), other: testutil.NewCA("other")}
}

// verifier returns the verifier of the bundles cluster-a (spiffe, the root only), corp (cn) and
// hosts (dns, the plain CA too).
func (k pki) verifier(t *testing.T) *Verifier {
	t.Helper()
	var bundles []*Bundle
	for _, b := range []struct {
		cfg config.CertBundle
		ca  *testutil.CA
	}{
		{config.CertBundle{Name: "cluster-a", TrustDomain: "cluster-a"}, k.spiffe},
		{config.CertBundle{Name: "corp", Identity: config.CertIdentityCN}, k.plain},
		{config.CertBundle{Name: "hosts", Identity: config.CertIdentityDNS}, k.plain},
	} {
		bundle, err := NewBundle(b.cfg, testutil.PEM(b.ca.Cert))
		if err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, bundle)
	}
	return NewVerifier(bundles, nil)
}

// TestVerify: the principal of each bundle's certificates, by the first bundle that verifies
// them, and every refusal of docs/spec/rbac.md §9.
func TestVerify(t *testing.T) {
	k := newPKI()
	v := k.verifier(t)
	chain := func(ca *testutil.CA, o testutil.CertOptions, more ...*x509.Certificate) []*x509.Certificate {
		c, _ := ca.Issue(o)
		return append([]*x509.Certificate{c}, more...)
	}
	node := testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w1"}}
	for _, tt := range []struct {
		name   string
		chain  []*x509.Certificate
		want   string // the principal, or "" for a refusal
		bundle string
		reason string
	}{
		{"a SPIFFE ID through the intermediate", chain(k.issuer, node, k.issuer.Cert), "cert:spiffe://cluster-a/node/w1", "cluster-a", ""},
		{"by the root", chain(k.spiffe, node), "cert:spiffe://cluster-a/node/w1", "cluster-a", ""},
		{"without an extended key usage", chain(k.spiffe, testutil.CertOptions{URIs: node.URIs, NoExtKeyUsage: true}), "cert:spiffe://cluster-a/node/w1", "cluster-a", ""},
		{"any extended key usage", chain(k.spiffe, testutil.CertOptions{URIs: node.URIs, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}), "cert:spiffe://cluster-a/node/w1", "cluster-a", ""},
		{"a CN: the first plain bundle", chain(k.plain, testutil.CertOptions{CN: "node-1", DNS: []string{"node-1.example.com"}}), "cert:corp/node-1", "corp", ""},
		{"no CN", chain(k.plain, testutil.CertOptions{DNS: []string{"node-1.example.com"}}), "", "corp", "a certificate's name is 1 to 255 bytes"},
		{"a CN with a star", chain(k.plain, testutil.CertOptions{CN: "node-*"}), "", "corp", "printable ASCII without a space or *"},
		{"without the intermediate", chain(k.issuer, node), "", "", "no bundle verifies"},
		{"another CA", chain(k.other, node), "", "", "no bundle verifies"},
		{"expired", chain(k.spiffe, testutil.CertOptions{URIs: node.URIs, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour)}), "", "", "no bundle verifies"},
		{"not yet valid", chain(k.spiffe, testutil.CertOptions{URIs: node.URIs, NotBefore: time.Now().Add(time.Hour)}), "", "", "no bundle verifies"},
		{"for servers only", chain(k.spiffe, testutil.CertOptions{URIs: node.URIs, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}), "", "", "no bundle verifies"},
		{"a CA as the leaf", chain(k.spiffe, testutil.CertOptions{URIs: node.URIs, IsCA: true}), "", "", "the leaf is a CA"},
		{"another trust domain", chain(k.spiffe, testutil.CertOptions{URIs: []string{"spiffe://cluster-b/node/w1"}}), "", "cluster-a", "not a SPIFFE ID of the trust domain cluster-a"},
		{"two URI SANs", chain(k.spiffe, testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w1", "spiffe://cluster-a/node/w2"}}), "", "cluster-a", "2 URI SANs"},
		{"no URI SAN", chain(k.spiffe, testutil.CertOptions{CN: "w1"}), "", "cluster-a", "0 URI SANs"},
		{"not spiffe", chain(k.spiffe, testutil.CertOptions{URIs: []string{"https://cluster-a/node/w1"}}), "", "cluster-a", "not a SPIFFE ID"},
		{"no path", chain(k.spiffe, testutil.CertOptions{URIs: []string{"spiffe://cluster-a"}}), "", "cluster-a", "a SPIFFE ID has a path"},
		{"a dot segment", chain(k.spiffe, testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/../admin"}}), "", "cluster-a", "is not . or .. alone"},
		{"an escape", chain(k.spiffe, testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w%2F1"}}), "", "cluster-a", "segment of a SPIFFE ID"},
		{"a query", chain(k.spiffe, testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w1?x=1"}}), "", "cluster-a", "not a SPIFFE ID"},
		{"none", nil, "", "", "no certificate"},
	} {
		p, bundle, err := v.Verify(tt.chain)
		switch {
		case tt.want != "" && (err != nil || p.String() != tt.want || bundle != tt.bundle):
			t.Errorf("%s: Verify = %s, %q, %v; want %s of %s", tt.name, p, bundle, err, tt.want, tt.bundle)
		case tt.want == "" && (!errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tt.reason) || bundle != tt.bundle):
			t.Errorf("%s: Verify = %s, %q, %v; want ErrRefused with %q of bundle %q", tt.name, p, bundle, err, tt.reason, tt.bundle)
		}
	}
}

// TestVerifyDNS: a dns bundle takes the first DNS SAN, and one without any is refused rather
// than tried against the next bundle.
func TestVerifyDNS(t *testing.T) {
	k := newPKI()
	hosts, err := NewBundle(config.CertBundle{Name: "hosts", Identity: config.CertIdentityDNS}, testutil.PEM(k.plain.Cert))
	if err != nil {
		t.Fatal(err)
	}
	corp, err := NewBundle(config.CertBundle{Name: "corp", Identity: config.CertIdentityCN}, testutil.PEM(k.plain.Cert))
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier([]*Bundle{hosts, corp}, nil)
	c, _ := k.plain.Issue(testutil.CertOptions{CN: "node-1", DNS: []string{"node-1.example.com", "node-1"}})
	if p, b, err := v.Verify([]*x509.Certificate{c}); err != nil || p != (auth.Principal{Kind: auth.KindCert, Name: "hosts/node-1.example.com"}) || b != "hosts" {
		t.Errorf("Verify = %s, %s, %v", p, b, err)
	}
	c, _ = k.plain.Issue(testutil.CertOptions{CN: "node-1"})
	if p, b, err := v.Verify([]*x509.Certificate{c}); !errors.Is(err, ErrRefused) || b != "hosts" {
		t.Errorf("Verify without a DNS name = %s, %s, %v; want the refusal of hosts", p, b, err)
	}
}

// TestNewBundle: a CA file of PEM certificates, and the files that are refused.
func TestNewBundle(t *testing.T) {
	k := newPKI()
	b, err := NewBundle(config.CertBundle{Name: "a", TrustDomain: "a"}, append(testutil.PEM(k.spiffe.Cert, k.issuer.Cert), "trailing text\n"...))
	if err != nil || b.Identity != config.CertIdentitySPIFFE || len(b.cas) != 2 {
		t.Errorf("NewBundle = %+v, %v", b, err)
	}
	for _, data := range []string{"", "not PEM", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"} {
		if _, err := NewBundle(config.CertBundle{Name: "a"}, []byte(data)); err == nil {
			t.Errorf("NewBundle(%q) took it", data)
		}
	}
	if _, err := LoadBundle(config.CertBundle{Name: "a", CAFile: t.TempDir() + "/missing.pem"}); err == nil {
		t.Error("LoadBundle of a missing file")
	}
	if n := len(k.verifier(t).Pool().Subjects()); n != 2 { //nolint:staticcheck // Subjects counts the CAs of a pool built here
		t.Errorf("the pool has %d CAs, want 2: cluster-a's, and corp's once for two bundles", n)
	}
}

// TestParseHeader: the forms of the edges, X-Forwarded-Client-Cert's last element, and the
// values that are refused.
func TestParseHeader(t *testing.T) {
	k := newPKI()
	leaf, _ := k.issuer.Issue(testutil.CertOptions{URIs: []string{"spiffe://cluster-a/node/w1"}})
	other, _ := k.plain.Issue(testutil.CertOptions{CN: "edge"})
	escaped := func(certs ...*x509.Certificate) string { return url.PathEscape(string(testutil.PEM(certs...))) }
	b64 := func(c *x509.Certificate) string { return base64.StdEncoding.EncodeToString(c.Raw) }
	for _, tt := range []struct {
		name  string
		value string
		xfcc  bool
		want  []*x509.Certificate
	}{
		{"nginx", escaped(leaf), false, []*x509.Certificate{leaf}},
		{"nginx with a chain", escaped(leaf, k.issuer.Cert), false, []*x509.Certificate{leaf, k.issuer.Cert}},
		{"PEM unescaped", string(testutil.PEM(leaf)), false, []*x509.Certificate{leaf}},
		{"base64 DER", b64(leaf) + ", " + b64(k.issuer.Cert), false, []*x509.Certificate{leaf, k.issuer.Cert}},
		{"traefik: base64 DER, URL-escaped", url.QueryEscape(b64(leaf)) + "," + url.QueryEscape(b64(k.issuer.Cert)), false,
			[]*x509.Certificate{leaf, k.issuer.Cert}},
		{"envoy", `By=spiffe://cluster-a/lajstrom;Hash=abc;Cert="` + escaped(leaf) + `";Chain="` + escaped(k.issuer.Cert) +
			`";Subject="CN=x,O=\"a;b\"";URI=spiffe://cluster-a/node/w1`, true, []*x509.Certificate{leaf, k.issuer.Cert}},
		{"envoy, the last element", `Hash=1;Cert="` + escaped(other) + `",By=x;Cert=` + escaped(leaf), true, []*x509.Certificate{leaf}},
	} {
		got, err := ParseHeader(header(tt.xfcc), tt.value)
		if err != nil || len(got) != len(tt.want) {
			t.Errorf("%s: ParseHeader = %d certificates, %v; want %d", tt.name, len(got), err, len(tt.want))
			continue
		}
		for i := range got {
			if !got[i].Equal(tt.want[i]) {
				t.Errorf("%s: certificate %d is another", tt.name, i)
			}
		}
	}
	for _, tt := range []struct {
		name, value string
		xfcc        bool
	}{
		{"empty", "", false},
		{"not base64", "!!!", false},
		{"base64 of no certificate", base64.StdEncoding.EncodeToString([]byte("x")), false},
		{"a private key", url.PathEscape("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"), false},
		{"a bad escape", "-----BEGIN%ZZ", false},
		{"too many", strings.Repeat(escaped(leaf), maxChain+1), false},
		{"envoy without Cert", `By=x;URI=spiffe://cluster-a/node/w1`, true},
		{"envoy, Cert in an earlier element only", `Cert="` + escaped(leaf) + `",By=x`, true},
		{"envoy, a quote that does not end", `Cert="` + escaped(leaf), true},
	} {
		if got, err := ParseHeader(header(tt.xfcc), tt.value); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: ParseHeader = %d certificates, %v; want ErrRefused", tt.name, len(got), err)
		}
	}
}

// header is the name of the header of a case of TestParseHeader: Envoy's in another case, or
// nginx's.
func header(xfcc bool) string {
	if xfcc {
		return "x-forwarded-client-cert"
	}
	return "X-SSL-Client-Cert"
}
