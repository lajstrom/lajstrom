package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/config"
)

// attachmentTrust reads the keys of attachments.trust (a file, or the key inline) and parses
// them: the trust that labels attachments (#89; docs/configuration.md, attachments). A file that
// cannot be read or a key that does not parse is an error naming the entry; serve and reindex
// stop on it rather than label every attachment untrusted.
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
	trust, err := attest.NewTrust(entries)
	if err != nil {
		return nil, fmt.Errorf("attachments.trust: %w", err)
	}
	if len(names) > 0 {
		log.Info("attachments: trusted keys", "names", names, "trust", trust.Hash())
	}
	return trust, nil
}
