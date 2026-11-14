package config

import (
	"strings"
	"testing"
)

// TestLoadAttachments: attachments.trust and maxBlobSize are kept as written; Load opens no key
// file. An entry without a name, a name twice, both or neither of the two forms of the key, and
// a negative bound are errors that name their key.
func TestLoadAttachments(t *testing.T) {
	c, err := Load(writeConfig(t, minimal+`attachments:
  maxBlobSize: 1048576
  trust:
    - {name: scanner, publicKeyFile: /etc/lajstrom/scanner.pub, predicateTypes: [https://cyclonedx.org/bom]}
    - name: ci
      publicKey: |
        -----BEGIN PUBLIC KEY-----
        AAAA
        -----END PUBLIC KEY-----
`), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := c.Attachments
	if a.MaxBlobSize != 1<<20 || len(a.Trust) != 2 || a.Trust[0].PublicKeyFile != "/etc/lajstrom/scanner.pub" ||
		a.Trust[0].PredicateTypes[0] != "https://cyclonedx.org/bom" || !strings.Contains(a.Trust[1].PublicKey, "BEGIN PUBLIC KEY") {
		t.Errorf("attachments = %+v", a)
	}

	_, err = Load(writeConfig(t, minimal+`attachments:
  maxBlobSize: -1
  trust:
    - {publicKeyFile: /a}
    - {name: x, publicKeyFile: /a, publicKey: k}
    - {name: x}
`), nil)
	for _, want := range []string{
		"attachments.maxBlobSize: must not be negative",
		"attachments.trust[0].name: must not be empty",
		"attachments.trust[1]: set one of publicKey and publicKeyFile",
		`attachments.trust[2].name: "x" is already used by attachments.trust[1]`,
		"attachments.trust[2]: set one of publicKey and publicKeyFile",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load = %v, want an error with %q", err, want)
		}
	}
}
