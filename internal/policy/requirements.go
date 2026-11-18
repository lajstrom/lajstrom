package policy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// The required attachments of a repo (docs/spec/policy.md §11, #1287): policy.requireAttachments,
// a list of requirements, each met by an attachment verified of its predicate type, signed by one
// of its names.

// KeyRequireAttachments is the key of a repo's policy that lists its requirements (policy.md §9).
const KeyRequireAttachments = "requireAttachments"

// PolicyKeySigner is the name of the policy key in the trust of attachments, which verifies
// verdicts only (manifest-profile.md §6.3): no name of a requirement.
const PolicyKeySigner = "lajstrom"

// The bounds of requireAttachments (policy.md §11).
const (
	MaxRequirements  = 16  // requirements per repo
	MaxSigners       = 16  // names per requirement
	maxPredicateType = 256 // bytes of a predicate type
)

// RequireAttachments returns policy.requireAttachments of repo, nil for none. A value that does
// not parse is none: the API checks the policy.
func RequireAttachments(repo index.Repo) []index.Requirement {
	if len(repo.Policy) == 0 {
		return nil
	}
	var p struct {
		Requirements []index.Requirement `json:"requireAttachments"`
	}
	if json.Unmarshal(repo.Policy, &p) != nil || len(p.Requirements) == 0 {
		return nil
	}
	return p.Requirements
}

// CheckRequirements checks reqs, the requirements of repo, as docs/spec/policy.md §11 does,
// against trust, the attachments' trust: 1 to MaxRequirements requirements, none the same as
// another (the same predicate type and the same names); each predicate type an absolute URI
// (CheckPredicateType) that is not a verdict's; each signedBy 1 to MaxSigners names, each a key
// or a keyless identity of trust, not PolicyKeySigner, trusted for a predicate type the
// requirement matches; and repo a hosted repo, not oci, not encrypted at rest. repo's Policy is
// the policy the repo will have. The error is a *RuleError naming the field below the policy:
// requireAttachments[1].signedBy[0].
func CheckRequirements(reqs []index.Requirement, repo index.Repo, trust *attest.Trust) error {
	switch {
	case repo.Kind == index.KindProxy:
		return &RuleError{KeyRequireAttachments, "a proxy repo cannot require attachments: a version it fetches has none yet, so it would fetch none"}
	case repo.Kind == index.KindGroup:
		return &RuleError{KeyRequireAttachments, "a group has no policy; its members' requirements hold through it"}
	case repo.Format == "oci":
		return &RuleError{KeyRequireAttachments, "an oci repo indexes no attachments, and its consumers verify them (ADR 0008)"}
	case encryptedAtRest(repo):
		return &RuleError{KeyRequireAttachments, "a repo encrypted at rest cannot require attachments: its attachments are encrypted, and never verify"}
	case len(reqs) == 0 || len(reqs) > MaxRequirements:
		return &RuleError{KeyRequireAttachments, fmt.Sprintf("must be a list of 1 to %d requirements", MaxRequirements)}
	}
	seen := map[string]int{}
	for i, r := range reqs {
		field := fmt.Sprintf("%s[%d]", KeyRequireAttachments, i)
		if err := checkRequirement(field, r, trust); err != nil {
			return err
		}
		key := r.PredicateType + "\x00" + strings.Join(slices.Compact(slices.Sorted(slices.Values(r.SignedBy))), "\x00")
		if j, ok := seen[key]; ok {
			return &RuleError{field, fmt.Sprintf("is the same as %s[%d]", KeyRequireAttachments, j)}
		}
		seen[key] = i
	}
	return nil
}

// checkRequirement checks the requirement r at field.
func checkRequirement(field string, r index.Requirement, trust *attest.Trust) error {
	if err := CheckPredicateType(r.PredicateType); err != nil {
		return &RuleError{field + ".predicateType", err.Error()}
	}
	if profile.IsType(attest.VerdictPredicateType, r.PredicateType) {
		return &RuleError{field + ".predicateType", fmt.Sprintf("%q matches the predicate type of verdicts, which policy decides by "+
			"their value (docs/spec/policy.md §2)", r.PredicateType)}
	}
	if len(r.SignedBy) == 0 || len(r.SignedBy) > MaxSigners {
		return &RuleError{field + ".signedBy", fmt.Sprintf("must be a list of 1 to %d names", MaxSigners)}
	}
	for j, name := range r.SignedBy {
		f := fmt.Sprintf("%s.signedBy[%d]", field, j)
		types, ok := trust.PredicateTypes(name)
		switch {
		case name == PolicyKeySigner:
			return &RuleError{f, fmt.Sprintf("%q is the policy key, which verifies verdicts only", name)}
		case !ok:
			return &RuleError{f, fmt.Sprintf("%q is neither a key of attachments.trust nor an identity of attachments.keyless.identities", name)}
		case types != nil && !slices.ContainsFunc(types, func(t string) bool { return profile.IsType(t, r.PredicateType) }):
			return &RuleError{f, fmt.Sprintf("%q is trusted for %s only, none of them %s or a longer version of it", name,
				strings.Join(types, ", "), r.PredicateType)}
		}
	}
	return nil
}

// CheckPredicateType checks the predicate type of a requirement (policy.md §11): an absolute URI
// of RFC 3986, with a scheme, of at most 256 bytes. It is the check of ?unmet= too.
func CheckPredicateType(t string) error {
	switch u, err := url.Parse(t); {
	case len(t) > maxPredicateType:
		return fmt.Errorf("is longer than %d bytes", maxPredicateType)
	case err != nil || !u.IsAbs() || strings.ContainsFunc(t, func(r rune) bool { return !uriChar(r) }):
		return fmt.Errorf("%q is not an absolute URI (RFC 3986, with a scheme)", t)
	}
	return nil
}

// uriChar reports whether r may be written in a URI (RFC 3986 §2): unreserved, reserved, or the
// % of a percent-encoding.
func uriChar(r rune) bool {
	return r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", r))
}

// encryptedAtRest reports whether repo's policy has encryption mode at-rest (format.EncryptionAtRest,
// which this package does not import).
func encryptedAtRest(repo index.Repo) bool {
	var p struct {
		Encryption struct {
			Mode string `json:"mode"`
		} `json:"encryption"`
	}
	return len(repo.Policy) > 0 && json.Unmarshal(repo.Policy, &p) == nil && p.Encryption.Mode == "at-rest"
}
