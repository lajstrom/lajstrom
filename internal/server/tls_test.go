package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSigned writes a self-signed certificate for 127.0.0.1 and its key as PEM files and
// returns their paths and the pool that trusts the certificate.
func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "lajstrom test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func TestTLS(t *testing.T) {
	certFile, keyFile, pool := selfSigned(t)
	s, err := New(Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler), TLSCertFile: certFile, TLSKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	addr, _, _ := start(t, s)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}}
	resp, err := client.Get("https://" + addr + "/-/healthz")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" || resp.Proto != "HTTP/2.0" {
		t.Errorf("GET over TLS = %d %q %s, want 200 ok over HTTP/2", resp.StatusCode, body, resp.Proto)
	}

	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}}} //nolint:gosec // the version the server must refuse
	if resp, err := old.Get("https://" + addr + "/-/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Error("a TLS 1.1 client was served")
	}

	resp, err = http.Get("http://" + addr + "/-/healthz")
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("plain HTTP to the TLS listener = %d, want 400", resp.StatusCode)
		}
	}
}

func TestTLSFiles(t *testing.T) {
	certFile, keyFile, _ := selfSigned(t)
	tests := []struct {
		name      string
		cert, key string
		want      string // a substring of New's error
	}{
		{name: "missing key file", cert: certFile, key: filepath.Join(t.TempDir(), "none.key"), want: "server: tls:"},
		{name: "key and certificate swapped", cert: keyFile, key: certFile, want: "server: tls:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Options{Listen: "127.0.0.1:0", Log: slog.New(slog.DiscardHandler), TLSCertFile: tt.cert, TLSKeyFile: tt.key})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New = %v, want an error with %q", err, tt.want)
			}
		})
	}
}
