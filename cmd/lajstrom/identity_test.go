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

// TestKubernetesClusters: no cluster is no authenticator; a cluster gets its verifier, with the
// audience of server.externalURL by default; a caFile that is missing or holds no certificate
// stops the start, naming its key.
func TestKubernetesClusters(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c := &config.Config{Server: config.Server{ExternalURL: "https://art.example.com/"}}
	if k, err := kubernetesClusters(c, log); k != nil || err != nil {
		t.Errorf("no cluster: %v, %v", k, err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, testutil.PEM(testutil.NewCA("a").Cert), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Auth.Kubernetes.Clusters = []config.KubernetesCluster{{Name: "prod", Issuer: "https://prod.example", CAFile: ca}}
	k, err := kubernetesClusters(c, log)
	if err != nil || len(k.Clusters) != 1 || k.Clusters[0].Name != "prod" || !strings.Contains(logs.String(), "audience=https://art.example.com\n") {
		t.Errorf("a cluster: %+v, %v\n%s", k, err, logs.String())
	}
	for _, file := range []string{ca + ".missing", filepath.Join(t.TempDir(), "empty.pem")} {
		_ = os.WriteFile(filepath.Join(filepath.Dir(file), "empty.pem"), []byte("no PEM\n"), 0o600)
		c.Auth.Kubernetes.Clusters[0].CAFile = file
		if _, err := kubernetesClusters(c, log); err == nil || !strings.HasPrefix(err.Error(), "auth.kubernetes.clusters[0].caFile: ") {
			t.Errorf("caFile %s: %v", filepath.Base(file), err)
		}
	}
}
