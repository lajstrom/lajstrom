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

// TestLoadKeyless: attachments.keyless is kept as written, one SCT by default and 0 when the file
// says so; identities need a trusted root, a name no key or identity has, an issuer, one of a
// subject and a regular expression that compiles.
func TestLoadKeyless(t *testing.T) {
	c, err := Load(writeConfig(t, minimal+`attachments:
  keyless:
    trustedRoot: /etc/lajstrom/trusted_root.json
    identities:
      - {name: release, issuer: https://token.actions.githubusercontent.com, subjectRegex: '^https://github\.com/acme/', predicateTypes: [https://slsa.dev/provenance/v1]}
`), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	k := c.Attachments.Keyless
	if k.TrustedRoot != "/etc/lajstrom/trusted_root.json" || k.SignedCertificateTimestamps != 1 || len(k.Identities) != 1 ||
		k.Identities[0].SubjectRegex != `^https://github\.com/acme/` || k.Identities[0].PredicateTypes[0] != "https://slsa.dev/provenance/v1" {
		t.Errorf("keyless = %+v", k)
	}
	c, err = Load(writeConfig(t, minimal+"attachments: {keyless: {signedCertificateTimestamps: 0}}\n"), nil)
	if err != nil || c.Attachments.Keyless.SignedCertificateTimestamps != 0 {
		t.Errorf("signedCertificateTimestamps: 0 = %v, %v", c.Attachments.Keyless.SignedCertificateTimestamps, err)
	}

	_, err = Load(writeConfig(t, minimal+`attachments:
  trust: [{name: scanner, publicKey: k}]
  keyless:
    signedCertificateTimestamps: -1
    identities:
      - {name: scanner, issuer: i, subject: s}
      - {name: x, subject: s, subjectRegex: r}
      - {name: x, issuer: i, subjectRegex: "("}
      - {issuer: i}
`), nil)
	for _, want := range []string{
		"attachments.keyless.signedCertificateTimestamps: must not be negative",
		"attachments.keyless.trustedRoot: must be set with identities",
		`attachments.keyless.identities[0].name: "scanner" is already used by attachments.trust[0]`,
		"attachments.keyless.identities[1].issuer: must not be empty",
		"attachments.keyless.identities[1]: set one of subject and subjectRegex",
		`attachments.keyless.identities[2].name: "x" is already used by attachments.keyless.identities[1]`,
		"attachments.keyless.identities[2].subjectRegex: error parsing regexp",
		"attachments.keyless.identities[3].name: must not be empty",
		"attachments.keyless.identities[3]: set one of subject and subjectRegex",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load = %v, want an error with %q", err, want)
		}
	}
}
