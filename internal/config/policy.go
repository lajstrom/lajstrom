package config

// Policy configures the policy of docs/spec/policy.md (#101).
type Policy struct {
	// SigningKey is the server's policy key (policy.md §5): "local" (or empty), an ECDSA P-256 key
	// sealed with the master key, or a KMS key URI of ADR 0020 (awskms:///…, gcpkms://…).
	SigningKey string `yaml:"signingKey"`
	// Denylist configures the signed deny list (policy.md §8).
	Denylist Denylist `yaml:"denylist"`
}

// Denylist configures the deny-list artifact.
type Denylist struct {
	// Registry is the registry the deny list is written to, by name; empty: the first of
	// registries.
	Registry string `yaml:"registry"`
}

// validate checks the policy's keys against the registries of the configuration.
func (c Policy) validate(p *problems, regs []Registry) {
	if c.SigningKey != "" && c.SigningKey != "local" {
		if _, err := ParseKMSKey(c.SigningKey); err != nil {
			p.add("policy.signingKey", "%s; or local", err.Error())
		}
	}
	if c.Denylist.Registry == "" {
		return
	}
	for _, r := range regs {
		if r.Name == c.Denylist.Registry {
			return
		}
	}
	p.add("policy.denylist.registry", "%q is not a registry of registries", c.Denylist.Registry)
}
