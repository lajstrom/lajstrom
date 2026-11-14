package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/config"
)

// attachmentTrust reads the keys of attachments.trust (a file, or the key inline) and the
// trusted root and identities of attachments.keyless, and parses them: the trust that labels
// attachments (#89; docs/configuration.md, attachments). A file that cannot be read, or a key, a
// trusted root or an identity that does not parse, is an error naming it; serve and reindex stop
// on it rather than label every attachment untrusted.
func attachmentTrust(c config.Attachments, log *slog.Logger) (*attest.Trust, error) {
	entries := make([]attest.Entry, 0, len(c.Trust))
	names := make([]string, 0, len(c.Trust))
	for i, e := range c.Trust {
		key := []byte(e.PublicKey)
		if e.PublicKeyFile != "" {
			b, err := os.ReadFile(e.PublicKeyFile)
			if err != nil {
				return nil, fmt.Errorf("attachments.trust[%d].publicKeyFile: %w", i, err)
			}
			key = b
		}
		entries = append(entries, attest.Entry{Name: e.Name, PublicKey: key, PredicateTypes: e.PredicateTypes})
		names = append(names, e.Name)
	}
	opts, identities, err := keylessOption(c.Keyless)
	if err != nil {
		return nil, err
	}
	trust, err := attest.NewTrust(entries, opts...)
	if err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	if len(names) > 0 || len(opts) > 0 {
		log.Info("attachments: trusted keys and keyless identities", "keys", names, "identities", identities,
			"trustedRoot", c.Keyless.TrustedRoot, "trust", trust.Hash())
	}
	return trust, nil
}

// keylessOption reads attachments.keyless: the trusted root's file, at start and only then, and
// the identities. Without a trusted root there is no option, and keyless attachments stay
// unsupported.
func keylessOption(k config.AttachmentKeyless) ([]attest.Option, []string, error) {
	if k.TrustedRoot == "" {
		return nil, nil, nil
	}
	tr, err := os.ReadFile(k.TrustedRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("attachments.keyless.trustedRoot: %w", err)
	}
	keyless := attest.Keyless{TrustedRoot: tr, SignedCertificateTimestamps: k.SignedCertificateTimestamps}
	names := make([]string, 0, len(k.Identities))
	for _, id := range k.Identities {
		keyless.Identities = append(keyless.Identities, attest.Identity{Name: id.Name, Issuer: id.Issuer, Subject: id.Subject,
			SubjectRegex: id.SubjectRegex, PredicateTypes: id.PredicateTypes})
		names = append(names, id.Name)
	}
	return []attest.Option{attest.WithKeyless(keyless)}, names, nil
}
