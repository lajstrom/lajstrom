package config

import "fmt"

// Attachments configures the verification of the attachments of versions: the signatures, SBOMs,
// scan reports and attestations pushed as referrers (#89, docs/spec/manifest-profile.md §6.3).
type Attachments struct {
	// Trust is the keys whose signatures make an attachment verified; none verifies nothing.
	Trust []AttachmentTrust `yaml:"trust"`
	// MaxBlobSize is the largest layer read to judge an attachment, in bytes; a larger one is
	// unsupported. Zero means the default, 16 MiB; must not be negative.
	MaxBlobSize int64 `yaml:"maxBlobSize"`
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
}
