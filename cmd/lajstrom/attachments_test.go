package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/config"
)

// TestAttachmentTrust: the keys of attachments.trust are read from their files or inline and
// make the trust ingest labels with; a file that cannot be read, or a key that does not parse, is
// an error naming the entry, and no entry is the empty trust.
func TestAttachmentTrust(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	scanner, ci := attesttest.NewKey(t), attesttest.NewKey(t)
	file := filepath.Join(t.TempDir(), "scanner.pub")
	if err := os.WriteFile(file, scanner.PublicPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	trust, err := attachmentTrust(config.Attachments{Trust: []config.AttachmentTrust{
		{Name: "scanner", PublicKeyFile: file, PredicateTypes: []string{"https://cyclonedx.org/bom"}},
		{Name: "ci", PublicKey: string(ci.PublicPEM(t))},
	}}, log)
	if err != nil {
		t.Fatal(err)
	}
	want, err := attest.NewTrust([]attest.Entry{scanner.Entry(t, "scanner", "https://cyclonedx.org/bom"), ci.Entry(t, "ci")})
	if err != nil {
		t.Fatal(err)
	}
	if trust.Hash() != want.Hash() {
		t.Errorf("the trust read is not the trust of the keys: %s, %s", trust.Hash(), want.Hash())
	}
	empty, err := attachmentTrust(config.Attachments{}, log)
	if err != nil || empty.Hash() != (*attest.Trust)(nil).Hash() {
		t.Errorf("no entry: %v, %v", empty, err)
	}
	for _, tt := range []struct {
		name  string
		entry config.AttachmentTrust
		want  string
	}{
		{"a missing file", config.AttachmentTrust{Name: "x", PublicKeyFile: filepath.Join(t.TempDir(), "none.pub")}, "attachments.trust[0].publicKeyFile"},
		{"not a key", config.AttachmentTrust{Name: "x", PublicKey: "ssh-ed25519 AAAA"}, `attachments: attest: the key of "x": not PEM`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := attachmentTrust(config.Attachments{Trust: []config.AttachmentTrust{tt.entry}}, log); err == nil ||
				!strings.Contains(err.Error(), tt.want) {
				t.Errorf("attachmentTrust = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

// TestAttachmentTrustKeyless: attachments.keyless's trusted root is read from its file and makes,
// with the identities, the trust attest makes of them; a file that cannot be read or is no
// trusted root is an error naming the key, and without a trusted root nothing keyless is trusted.
func TestAttachmentTrustKeyless(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	sg := attesttest.NewSigstore(t)
	dir := t.TempDir()
	file, tr := filepath.Join(dir, "trusted_root.json"), sg.TrustedRoot(t)
	if err := os.WriteFile(file, tr, 0o600); err != nil {
		t.Fatal(err)
	}
	ids := []config.AttachmentIdentity{{Name: "release", Issuer: "https://token.actions.githubusercontent.com", SubjectRegex: "^https://github.com/acme/"}}
	trust, err := attachmentTrust(config.Attachments{Keyless: config.AttachmentKeyless{TrustedRoot: file, Identities: ids, SignedCertificateTimestamps: 1}}, log)
	if err != nil {
		t.Fatal(err)
	}
	want, err := attest.NewTrust(nil, attest.WithKeyless(attest.Keyless{TrustedRoot: tr, SignedCertificateTimestamps: 1,
		Identities: []attest.Identity{{Name: "release", Issuer: "https://token.actions.githubusercontent.com", SubjectRegex: "^https://github.com/acme/"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if trust.Hash() != want.Hash() {
		t.Errorf("the keyless trust read is not attest's of the same: %s, %s", trust.Hash(), want.Hash())
	}
	none, err := attachmentTrust(config.Attachments{Keyless: config.AttachmentKeyless{SignedCertificateTimestamps: 1}}, log)
	if err != nil || none.Hash() != (*attest.Trust)(nil).Hash() {
		t.Errorf("no trusted root: %v, %v", none, err)
	}
	notRoot := filepath.Join(dir, "not.json")
	if err := os.WriteFile(notRoot, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "none.json"): "attachments.keyless.trustedRoot",
		notRoot:                         "attest: the trusted root",
	} {
		if _, err := attachmentTrust(config.Attachments{Keyless: config.AttachmentKeyless{TrustedRoot: path}}, log); err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("trustedRoot %s: %v, want an error with %q", path, err, want)
		}
	}
}
