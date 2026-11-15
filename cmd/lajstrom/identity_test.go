package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestClientCertificates: no bundle is no verifier; the bundles load from their files, and a
// file that is missing stops the start, naming the bundle's key; a bundle without a way in
// warns.
func TestClientCertificates(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c := &config.Config{}
	if v, err := clientCertificates(c, log); v != nil || err != nil {
		t.Errorf("no bundle: %v, %v", v, err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, testutil.PEM(testutil.NewCA("a").Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Auth.ClientCertificates.Bundles = []config.CertBundle{{Name: "a", CAFile: ca, TrustDomain: "a"}}
	if v, err := clientCertificates(c, log); v == nil || err != nil || !strings.Contains(logs.String(), "no certificate can arrive") {
		t.Errorf("a bundle: %v, %v\n%s", v, err, logs.String())
	}
	c.Auth.ClientCertificates.Bundles = append(c.Auth.ClientCertificates.Bundles, config.CertBundle{Name: "b", CAFile: ca + ".missing"})
	if _, err := clientCertificates(c, log); err == nil || !strings.HasPrefix(err.Error(), "auth.clientCertificates.bundles[1]: ") {
		t.Errorf("a missing file: %v", err)
	}
}
