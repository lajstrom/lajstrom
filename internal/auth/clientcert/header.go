package clientcert

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
)

// maxChain bounds the certificates a header may carry, the leaf and its intermediates.
const maxChain = 8

// ParseHeader returns the certificates of the value of the header in which an edge forwards a
// client certificate (docs/spec/rbac.md §9), the leaf first. With xfcc, the value is Envoy's
// X-Forwarded-Client-Cert: elements separated by ",", keys and values by ";" and "=", values in
// double quotes where they need them; the last element counts, its Cert, a URL-escaped PEM
// certificate, then its Chain, URL-escaped PEM intermediates. Otherwise it is a URL-escaped PEM
// certificate followed by any intermediates (nginx's $ssl_client_escaped_cert), or base64 DER
// certificates separated by ",", each URL-escaped or not (Traefik, HAProxy). Errors wrap
// ErrRefused.
func ParseHeader(value string, xfcc bool) ([]*x509.Certificate, error) {
	if xfcc {
		cert, chain, err := lastElement(value)
		if err != nil {
			return nil, err
		}
		return escapedPEM(cert + chain)
	}
	if strings.HasPrefix(strings.TrimSpace(value), "-----BEGIN") {
		return escapedPEM(value)
	}
	var out []*x509.Certificate
	for part := range strings.SplitSeq(value, ",") {
		part, err := url.PathUnescape(strings.Join(strings.Fields(part), ""))
		if err != nil {
			return nil, fmt.Errorf("%w: the header is neither PEM nor base64 DER", ErrRefused)
		}
		der, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			return nil, fmt.Errorf("%w: the header is neither PEM nor base64 DER", ErrRefused)
		}
		if out, err = appendCert(out, der); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// escapedPEM returns the PEM certificates of the URL-escaped s, in order.
func escapedPEM(s string) ([]*x509.Certificate, error) {
	data, err := url.PathUnescape(s)
	if err != nil {
		return nil, fmt.Errorf("%w: the certificate is not URL-escaped PEM", ErrRefused)
	}
	var out []*x509.Certificate
	for rest := []byte(data); ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("%w: a PEM block of type %q in the header", ErrRefused, block.Type)
		}
		if out, err = appendCert(out, block.Bytes); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no PEM certificate in the header", ErrRefused)
	}
	return out, nil
}

// appendCert parses der and appends it to certs, at most maxChain of them.
func appendCert(certs []*x509.Certificate, der []byte) ([]*x509.Certificate, error) {
	if len(certs) == maxChain {
		return nil, fmt.Errorf("%w: more than %d certificates in the header", ErrRefused, maxChain)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("%w: a certificate of the header does not parse", ErrRefused)
	}
	return append(certs, c), nil
}

// lastElement returns the Cert and Chain of the last element of an X-Forwarded-Client-Cert
// value, still URL-escaped: the element that the closest proxy wrote.
func lastElement(v string) (cert, chain string, err error) {
	elements := split(v, ',')
	last := strings.TrimSpace(elements[len(elements)-1])
	for _, pair := range split(last, ';') {
		key, value, _ := strings.Cut(strings.TrimSpace(pair), "=")
		value, err = unquote(value)
		if err != nil {
			return "", "", err
		}
		switch {
		case strings.EqualFold(key, "Cert"):
			cert = value
		case strings.EqualFold(key, "Chain"):
			chain = value
		}
	}
	if cert == "" {
		return "", "", fmt.Errorf("%w: the last element of %s has no Cert", ErrRefused, "X-Forwarded-Client-Cert")
	}
	return cert, chain, nil
}

// split splits s at each sep outside double quotes; a backslash in quotes escapes the next byte.
func split(s string, sep byte) []string {
	var out []string
	quoted, start := false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && c == sep:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// unquote returns v without its double quotes and escapes, or v when it is not quoted.
func unquote(v string) (string, error) {
	if !strings.HasPrefix(v, `"`) {
		return v, nil
	}
	if len(v) < 2 || !strings.HasSuffix(v, `"`) {
		return "", fmt.Errorf("%w: a quoted value of X-Forwarded-Client-Cert does not end", ErrRefused)
	}
	var b strings.Builder
	for i := 1; i < len(v)-1; i++ {
		if v[i] == '\\' && i+1 < len(v)-1 {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String(), nil
}
