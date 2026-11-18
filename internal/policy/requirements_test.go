package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/index"
)

const (
	spdx       = "https://spdx.dev/Document"
	provenance = "https://slsa.dev/provenance"
	signature  = "https://sigstore.dev/cosign/sign/v1"
)

// trustOf is a trust of a key per entry, name and predicate types.
func trustOf(t *testing.T, entries map[string][]string) *attest.Trust {
	t.Helper()
	var es []attest.Entry
	for name, types := range entries {
		es = append(es, attesttest.NewKey(t).Entry(t, name, types...))
	}
	trust, err := attest.NewTrust(es)
	must(t, err)
	return trust
}

// TestCheckRequirements is the validation of policy.md §11.
func TestCheckRequirements(t *testing.T) {
	trust := trustOf(t, map[string][]string{"release": {spdx, signature}, "ci": {provenance + "/v1"}, "any": nil,
		PolicyKeySigner: {attest.VerdictPredicateType}})
	hosted := index.Repo{Format: "pypi", Name: "prod", Kind: index.KindHosted}
	req := func(predicateType string, names ...string) index.Requirement {
		return index.Requirement{PredicateType: predicateType, SignedBy: names}
	}
	ok := []index.Requirement{req(spdx, "release"), req(provenance, "ci", "any"), req(signature, "release"), req(spdx, "any")}
	if err := CheckRequirements(ok, hosted, trust); err != nil {
		t.Errorf("valid requirements: %v", err)
	}
	many := make([]index.Requirement, MaxRequirements+1)
	for i := range many {
		many[i] = req(fmt.Sprintf("https://example.com/t%d", i), "any")
	}
	names := make([]string, MaxSigners+1)
	for i := range names {
		names[i] = "any"
	}
	at := func(kind, format, policy string) index.Repo {
		return index.Repo{Format: format, Name: "r", Kind: kind, Policy: json.RawMessage(policy)}
	}
	for _, tt := range []struct {
		name  string
		reqs  []index.Requirement
		repo  index.Repo
		field string
	}{
		{"a proxy repo", ok[:1], at(index.KindProxy, "pypi", ""), "requireAttachments"},
		{"a group", ok[:1], at(index.KindGroup, "pypi", ""), "requireAttachments"},
		{"an oci repo", ok[:1], at(index.KindHosted, "oci", ""), "requireAttachments"},
		{"a repo encrypted at rest", ok[:1], at(index.KindHosted, "raw", `{"encryption": {"mode": "at-rest"}}`), "requireAttachments"},
		{"none", []index.Requirement{}, hosted, "requireAttachments"},
		{"too many", many, hosted, "requireAttachments"},
		{"no scheme", []index.Requirement{req("spdx.dev/Document", "release")}, hosted, "requireAttachments[0].predicateType"},
		{"a space", []index.Requirement{req("https://spdx.dev/a Document", "release")}, hosted, "requireAttachments[0].predicateType"},
		{"too long", []index.Requirement{req("https://e.com/"+strings.Repeat("a", 243), "any")}, hosted, "requireAttachments[0].predicateType"},
		{"a verdict", []index.Requirement{req(attest.VerdictPredicateType, "any")}, hosted, "requireAttachments[0].predicateType"},
		{"a verdict's prefix", []index.Requirement{req("https://lajstrom.dev/verdict", "any")}, hosted, "requireAttachments[0].predicateType"},
		{"no name", []index.Requirement{req(spdx)}, hosted, "requireAttachments[0].signedBy"},
		{"too many names", []index.Requirement{req(spdx, names...)}, hosted, "requireAttachments[0].signedBy"},
		{"the policy key", []index.Requirement{req(spdx, "any"), req(provenance, "ci", PolicyKeySigner)}, hosted, "requireAttachments[1].signedBy[1]"},
		{"a name the trust lacks", []index.Requirement{req(spdx, "relase")}, hosted, "requireAttachments[0].signedBy[0]"},
		{"a name trusted for other types", []index.Requirement{req(provenance, "release")}, hosted, "requireAttachments[0].signedBy[0]"},
		{"a name trusted for an older type", []index.Requirement{req(provenance+"/v1.1", "ci")}, hosted, "requireAttachments[0].signedBy[0]"},
		{"the same twice", []index.Requirement{req(spdx, "release", "any"), req(spdx, "any", "release")}, hosted, "requireAttachments[1]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckRequirements(tt.reqs, tt.repo, trust)
			var re *RuleError
			if !errors.As(err, &re) || re.Field != tt.field || !errors.Is(err, ErrInvalid) {
				t.Errorf("CheckRequirements = %v, want a RuleError of %s", err, tt.field)
			}
		})
	}
	if err := CheckRequirements(ok[:1], hosted, nil); err == nil {
		t.Error("a name checked without a trust")
	}
	if err := CheckPredicateType("https://e.com/" + strings.Repeat("a", 242)); err != nil {
		t.Errorf("a predicate type of 256 bytes: %v", err)
	}
}

// TestRequireAttachments: the requirements as the repo's policy has them; none without, or for
// a value that does not parse.
func TestRequireAttachments(t *testing.T) {
	for policy, want := range map[string]int{
		"": 0, `{"enforcePull": true}`: 0, `{"requireAttachments": null}`: 0, `{"requireAttachments": {"a": 1}}`: 0,
		`{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["a", "b"]}]}`: 1,
	} {
		got := RequireAttachments(index.Repo{Policy: json.RawMessage(policy)})
		if len(got) != want || want == 1 && (got[0].PredicateType != spdx || len(got[0].SignedBy) != 2) {
			t.Errorf("RequireAttachments(%s) = %+v", policy, got)
		}
	}
}
