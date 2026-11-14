//go:build integration

package awskms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// TestLocalKMS runs the client against an AWS KMS emulator at LAJSTROM_TEST_AWSKMS, such as
// nsmithuk/local-kms (docker run -p 127.0.0.1:4599:8080 nsmithuk/local-kms:3, then
// LAJSTROM_TEST_AWSKMS=http://127.0.0.1:4599, region LAJSTROM_TEST_AWSKMS_REGION or eu-west-2).
// It makes an RSA_4096 and an ECC_NIST_P256 key there, with an alias for the first, and signs an
// apt repo's InRelease and Release.gpg and an rpm repomd.xml.asc through keys.Service: under
// keys.kms by the alias, then after a rotation onto the P-256 key, both keys signing. The
// documents verify with go-crypto and, when gpgv is in PATH, with gpgv under apt's
// --assert-pubkey-algo. A KMS that does not answer is a 503. Never run it against a real AWS
// account: a KMS key cannot be deleted at once.
func TestLocalKMS(t *testing.T) {
	endpoint := os.Getenv("LAJSTROM_TEST_AWSKMS")
	if endpoint == "" {
		t.Skip("LAJSTROM_TEST_AWSKMS is not set: no AWS KMS emulator")
	}
	if strings.Contains(endpoint, "amazonaws.com") {
		t.Fatal("LAJSTROM_TEST_AWSKMS is AWS itself: the test makes keys that cannot be deleted at once")
	}
	region := os.Getenv("LAJSTROM_TEST_AWSKMS_REGION")
	if region == "" {
		region = "eu-west-2"
	}
	ctx := t.Context()
	c := New(Options{Endpoint: endpoint, Region: region, Log: slog.New(slog.DiscardHandler),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDLOCALKMS", SecretAccessKey: "local-kms"}, nil
		})})
	rsaARN, p256ARN := createKey(t, c, "RSA_4096"), createKey(t, c, "ECC_NIST_P256")
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	alias := "alias/lajstrom-test-" + hex.EncodeToString(suffix)
	must(t, c.call(ctx, "", "CreateAlias", map[string]string{"AliasName": alias, "TargetKeyId": rsaARN}, &struct{}{}))

	st, err := sqlite.Open(ctx, "file:localkms?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	repo := index.Repo{Format: "apt", Name: "ubuntu", Registry: "local", Prefix: "apt/ubuntu", Kind: "hosted"}
	must(t, st.CreateRepo(ctx, &repo))
	m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	must(t, err)
	newService := func(kms keys.KMS) *keys.Service {
		svc, err := keys.New(keys.Options{Index: st, Master: m, RetireGrace: time.Hour, Log: slog.New(slog.DiscardHandler),
			KMS: map[string]keys.KMS{config.KMSSchemeAWS: kms}, KMSKey: "awskms:///" + alias})
		must(t, err)
		return svc
	}
	svc := newService(c)

	first, err := svc.KeyFor(ctx, repo)
	must(t, err)
	signAndVerify(t, svc, repo, 1)
	rotated, err := svc.RotateTo(ctx, repo, "awskms:///"+p256ARN)
	must(t, err)
	if rotated.Key.Fingerprint == first.Key.Fingerprint {
		t.Fatal("the rotation kept the fingerprint")
	}
	signAndVerify(t, svc, repo, 2)

	down := newService(New(Options{Endpoint: "http://127.0.0.1:1", Region: region, Retries: -1, Credentials: c.o.Credentials,
		Log: slog.New(slog.DiscardHandler)}))
	_, err = down.ClearSign(ctx, repo, []byte("Origin: Lajstrom\n"))
	if format.StatusOf(err) != http.StatusServiceUnavailable {
		t.Errorf("a KMS that does not answer: %v, status %d", err, format.StatusOf(err))
	}
}

// createKey makes a SIGN_VERIFY key of spec in the emulator and returns its ARN.
func createKey(t *testing.T, c *Client, spec string) string {
	t.Helper()
	var out struct{ KeyMetadata struct{ Arn string } }
	must(t, c.call(t.Context(), "", "CreateKey", map[string]string{"KeySpec": spec, "KeyUsage": "SIGN_VERIFY",
		"Description": "lajstrom test, delete me"}, &out))
	return out.KeyMetadata.Arn
}

// signAndVerify signs an InRelease, a Release.gpg and a repomd.xml.asc with the repo's keys and
// checks that want signatures of each verify against key.asc.
func signAndVerify(t *testing.T, svc *keys.Service, repo index.Repo, want int) {
	t.Helper()
	ctx := t.Context()
	release := []byte("Origin: Lajstrom\nLabel: ubuntu\nSuite: jammy\n")
	repomd := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<repomd/>\n")
	pub, err := svc.PublicKeys(ctx, repo)
	must(t, err)
	kr, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pub))
	must(t, err)
	if len(kr) != want {
		t.Fatalf("key.asc holds %d keys, want %d", len(kr), want)
	}
	in, err := svc.ClearSign(ctx, repo, release)
	must(t, err)
	sig, err := svc.DetachSign(ctx, repo, release)
	must(t, err)
	mdSig, err := svc.DetachSign(ctx, repo, repomd)
	must(t, err)
	for _, d := range []struct{ data, sig []byte }{{release, sig}, {repomd, mdSig}} {
		if _, err := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(d.data), bytes.NewReader(d.sig), nil); err != nil {
			t.Errorf("a detached signature does not verify: %v", err)
		}
	}
	gpgv, err := exec.LookPath("gpgv")
	if err != nil {
		t.Log("no gpgv in PATH: verified with go-crypto only")
		return
	}
	dir := t.TempDir()
	block, err := armor.Decode(bytes.NewReader(pub))
	must(t, err)
	ring, err := io.ReadAll(block.Body)
	must(t, err)
	for name, b := range map[string][]byte{"keyring.gpg": ring, "InRelease": in, "Release": release, "Release.gpg": sig,
		"repomd.xml": repomd, "repomd.xml.asc": mdSig} {
		must(t, os.WriteFile(filepath.Join(dir, name), b, 0o600))
	}
	for _, files := range [][]string{{"InRelease"}, {"Release.gpg", "Release"}, {"repomd.xml.asc", "repomd.xml"}} {
		args := append([]string{"--homedir", t.TempDir(), "--keyring", filepath.Join(dir, "keyring.gpg"),
			"--assert-pubkey-algo=>=rsa2048,ed25519,ed448,nistp256,nistp384,nistp521"}, files...)
		cmd := exec.CommandContext(ctx, gpgv, args...) //nolint:gosec // gpgv on the test's own files
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil || strings.Count(string(out), "Good signature") != want {
			t.Errorf("gpgv %v: %v, want %d good signatures\n%s", files, err, want, out)
		}
	}
}
