package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// selfSigned writes the certificate and key of an httptest TLS server (127.0.0.1, example.com)
// as PEM files and returns their paths and the pool that trusts the certificate.
func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	pair := ts.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for _, f := range []struct {
		path, kind string
		der        []byte
	}{{certFile, "CERTIFICATE", pair.Certificate[0]}, {keyFile, "PRIVATE KEY", keyDER}} {
		if err := os.WriteFile(f.path, pem.EncodeToMemory(&pem.Block{Type: f.kind, Bytes: f.der}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool = x509.NewCertPool()
	pool.AddCert(ts.Certificate())
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
