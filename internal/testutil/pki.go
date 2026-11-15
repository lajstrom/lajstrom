package testutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"sync/atomic"
	"time"
)

// CA is a certificate authority made in memory, for the tests of client certificates
// (docs/spec/rbac.md §9): its keys are ECDSA P-256 and live only in the test's process, so no
// private key is ever written to the repository.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// CertOptions are the fields of a certificate a CA issues; the zero value is a leaf for client
// authentication, valid from an hour ago for a day.
type CertOptions struct {
	CN   string
	DNS  []string
	URIs []string
	// NotBefore and NotAfter default to an hour ago and a day from now.
	NotBefore, NotAfter time.Time
	// ExtKeyUsage defaults to client authentication; NoExtKeyUsage leaves it out.
	ExtKeyUsage   []x509.ExtKeyUsage
	NoExtKeyUsage bool
	// IsCA makes a CA certificate.
	IsCA bool
}

// serial numbers the certificates of the process.
var serial atomic.Int64

// NewCA returns a self-signed root CA named cn. It panics on a failure of crypto/rand.
func NewCA(cn string) *CA {
	key := newKey()
	tmpl := template(CertOptions{CN: cn, IsCA: true})
	return &CA{Cert: create(tmpl, tmpl, key.Public(), key), Key: key}
}

// Intermediate returns a CA named cn that ca signed.
func (ca *CA) Intermediate(cn string) *CA {
	key := newKey()
	return &CA{Cert: create(template(CertOptions{CN: cn, IsCA: true}), ca.Cert, key.Public(), ca.Key), Key: key}
}

// Issue returns a certificate of o that ca signed, and its key.
func (ca *CA) Issue(o CertOptions) (*x509.Certificate, crypto.Signer) {
	key := newKey()
	return create(template(o), ca.Cert, key.Public(), ca.Key), key
}

// TLS returns the certificate c with its key and the chain above it, as a TLS client or server
// presents them.
func TLS(c *x509.Certificate, key crypto.Signer, chain ...*x509.Certificate) tls.Certificate {
	out := tls.Certificate{Certificate: [][]byte{c.Raw}, PrivateKey: key, Leaf: c}
	for _, i := range chain {
		out.Certificate = append(out.Certificate, i.Raw)
	}
	return out
}

// PEM returns the certificates in PEM, one CERTIFICATE block each.
func PEM(certs ...*x509.Certificate) []byte {
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

func newKey() *ecdsa.PrivateKey {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return key
}

// template is the certificate of o with its defaults.
func template(o CertOptions) *x509.Certificate {
	now := time.Now()
	if o.NotBefore.IsZero() {
		o.NotBefore = now.Add(-time.Hour)
	}
	if o.NotAfter.IsZero() {
		o.NotAfter = now.Add(24 * time.Hour)
	}
	t := &x509.Certificate{
		SerialNumber: big.NewInt(serial.Add(1)),
		Subject:      pkix.Name{CommonName: o.CN},
		DNSNames:     o.DNS,
		NotBefore:    o.NotBefore,
		NotAfter:     o.NotAfter,
	}
	for _, s := range o.URIs {
		u, err := url.Parse(s)
		if err != nil {
			panic(err)
		}
		t.URIs = append(t.URIs, u)
	}
	switch {
	case o.IsCA:
		t.IsCA, t.BasicConstraintsValid = true, true
		t.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	default:
		t.BasicConstraintsValid = true
		t.KeyUsage = x509.KeyUsageDigitalSignature
		t.ExtKeyUsage = o.ExtKeyUsage
		if t.ExtKeyUsage == nil && !o.NoExtKeyUsage {
			t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
	}
	return t
}

func create(tmpl, parent *x509.Certificate, pub crypto.PublicKey, signer crypto.Signer) *x509.Certificate {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		panic(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return c
}
