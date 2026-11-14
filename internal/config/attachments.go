package config

import (
	"fmt"
	"regexp"
)

// Attachments configures the verification of the attachments of versions: the signatures, SBOMs,
// scan reports and attestations pushed as referrers (#89, docs/spec/manifest-profile.md §6.3).
type Attachments struct {
	// Trust is the keys whose signatures make an attachment verified; none verifies nothing.
	Trust []AttachmentTrust `yaml:"trust"`
	// MaxBlobSize is the largest layer read to judge an attachment, in bytes; a larger one is
	// unsupported. Zero means the default, 16 MiB; must not be negative.
	MaxBlobSize int64 `yaml:"maxBlobSize"`
	// Keyless is the verification of keyless bundles (Fulcio and Rekor), offline.
	Keyless AttachmentKeyless `yaml:"keyless"`
}

// DefaultSignedCertificateTimestamps is attachments.keyless.signedCertificateTimestamps when the
// file leaves it out: the public-good Fulcio embeds one in every certificate.
const DefaultSignedCertificateTimestamps = 1

// AttachmentKeyless is attachments.keyless (docs/spec/manifest-profile.md §6.3): the trusted root,
// a file serve and reindex read at start and Load does not open, and the trusted identities.
type AttachmentKeyless struct {
	// TrustedRoot is the path of a trusted_root.json; required with identities.
	TrustedRoot string `yaml:"trustedRoot"`
	// SignedCertificateTimestamps is how many SCTs a certificate must carry; 0 for a Fulcio
	// without a CT log. Must not be negative.
	SignedCertificateTimestamps int `yaml:"signedCertificateTimestamps"`
	// Identities are the signers trusted.
	Identities []AttachmentIdentity `yaml:"identities"`
}

// AttachmentIdentity is one entry of attachments.keyless.identities.
type AttachmentIdentity struct {
	// Name is the signedBy of what it verifies; unique among keys and identities.
	Name string `yaml:"name"`
	// Issuer is the certificate's OIDC issuer, exactly.
	Issuer string `yaml:"issuer"`
	// Subject is its subject alternative name, exactly; SubjectRegex a Go regular expression it
	// matches. One of the two.
	Subject      string `yaml:"subject"`
	SubjectRegex string `yaml:"subjectRegex"`
	// PredicateTypes are the predicate types the identity is trusted for; empty: every type.
	PredicateTypes []string `yaml:"predicateTypes"`
}

// AttachmentTrust is one entry of attachments.trust: a public key and the predicate types it is
// trusted for. serve and reindex read PublicKeyFile and parse the key (attest.NewTrust); Load
// does not open the file.
type AttachmentTrust struct {
	// Name is the signedBy of what its key verifies; unique, not empty.
	Name string `yaml:"name"`
	// PublicKey is a PEM PUBLIC KEY inline; PublicKeyFile a file that holds one. One of the two.
	PublicKey     string `yaml:"publicKey"`
	PublicKeyFile string `yaml:"publicKeyFile"`
	// PredicateTypes are the predicate types the key is trusted for; empty: every type.
	PredicateTypes []string `yaml:"predicateTypes"`
}

func (a Attachments) validate(p *problems) {
	if a.MaxBlobSize < 0 {
		p.add("attachments.maxBlobSize", "must not be negative")
	}
	names := map[string]int{}
	for i, e := range a.Trust {
		key := fmt.Sprintf("attachments.trust[%d]", i)
		switch first, dup := names[e.Name]; {
		case e.Name == "":
			p.add(key+".name", "must not be empty")
		case dup:
			p.add(key+".name", "%q is already used by attachments.trust[%d]", e.Name, first)
		default:
			names[e.Name] = i
		}
		if (e.PublicKey == "") == (e.PublicKeyFile == "") {
			p.add(key, "set one of publicKey and publicKeyFile")
		}
	}
	a.Keyless.validate(p, names)
}

// validate checks the keyless keys; names are the names the keys of attachments.trust took.
func (k AttachmentKeyless) validate(p *problems, names map[string]int) {
	if k.SignedCertificateTimestamps < 0 {
		p.add("attachments.keyless.signedCertificateTimestamps", "must not be negative")
	}
	if len(k.Identities) > 0 && k.TrustedRoot == "" {
		p.add("attachments.keyless.trustedRoot", "must be set with identities: a trusted_root.json (lajstrom admin trusted-root fetch)")
	}
	identities := map[string]int{}
	for i, id := range k.Identities {
		key := fmt.Sprintf("attachments.keyless.identities[%d]", i)
		_, key2 := names[id.Name]
		first, dup := identities[id.Name]
		switch {
		case id.Name == "":
			p.add(key+".name", "must not be empty")
		case key2:
			p.add(key+".name", "%q is already used by attachments.trust[%d]", id.Name, names[id.Name])
		case dup:
			p.add(key+".name", "%q is already used by attachments.keyless.identities[%d]", id.Name, first)
		default:
			identities[id.Name] = i
		}
		if id.Issuer == "" {
			p.add(key+".issuer", "must not be empty")
		}
		if (id.Subject == "") == (id.SubjectRegex == "") {
			p.add(key, "set one of subject and subjectRegex")
		} else if id.SubjectRegex != "" {
			if _, err := regexp.Compile(id.SubjectRegex); err != nil {
				p.add(key+".subjectRegex", "%v", err)
			}
		}
	}
}
