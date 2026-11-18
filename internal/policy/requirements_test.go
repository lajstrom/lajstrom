package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/auth"
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

// requiring is the policy of a repo that requires reqs.
func requiring(t *testing.T, reqs ...index.Requirement) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{KeyRequireAttachments: reqs})
	must(t, err)
	return string(b)
}

// version writes a version of pkg whose manifest has one attachment verified per (kind,
// predicate type, signer) of attached, and returns its subject.
func (e *env) version(t *testing.T, repo index.Repo, pkg index.Package, version, variant string, attached ...[3]string) Subject {
	t.Helper()
	ctx := t.Context()
	v := index.Version{PackageID: pkg.ID, Version: version, Variant: variant, Tag: version + "_" + variant,
		Digest: digest.FromString(pkg.Name + version + variant), ArtifactType: "application/vnd.lajstrom.pypi.v1", CreatedAt: e.now}
	must(t, e.st.UpsertVersion(ctx, &v))
	for i, a := range attached {
		d := digest.FromString(fmt.Sprintf("%s referrer %d", v.Digest, i))
		must(t, e.st.ReplaceAttachments(ctx, v.ID, d, []index.Attachment{{Layer: d, MediaType: attest.BundleMediaType, Kind: a[0],
			PredicateType: a[1], Verification: index.AttachmentVerified, SignedBy: a[2], Trust: "t"}}))
	}
	return Subject{Repo: repo, Name: pkg.Name, Version: version, Variant: variant, Digest: v.Digest, Published: e.now}
}

// TestDecideRequirements is the decision of policy.md §3, step 2: the first requirement not met
// denies, before any allow rule and after a deny verdict; a name the trust lacks meets nothing; a
// predicate type is matched with its longer versions, a signature as cosign's; each variant on its
// own; an upload leaves the requirements out.
func TestDecideRequirements(t *testing.T) {
	e := newEnv(t)
	e.d.o.Trust = trustOf(t, map[string][]string{"release": {spdx, signature}, "ci": nil})
	reqs := []index.Requirement{{PredicateType: spdx, SignedBy: []string{"release", "release-2027"}},
		{PredicateType: provenance, SignedBy: []string{"ci"}}}
	repo := e.repo(t, "pypi", "prod", index.KindHosted, requiring(t, reqs...))
	pkg := index.Package{RepoID: repo.ID, Name: "app", NameOCI: "app"}
	must(t, e.st.UpsertPackage(t.Context(), &pkg))
	sbom, slsa := [3]string{index.AttachmentSBOM, spdx, "release"}, [3]string{index.AttachmentProvenance, provenance + "/v1", "ci"}

	both := e.version(t, repo, pkg, "1.0", "a", sbom, slsa)
	other := e.version(t, repo, pkg, "1.0", "b")
	noProvenance := e.version(t, repo, pkg, "1.1", "", sbom)
	none := e.version(t, repo, pkg, "1.2", "")
	dropped := e.version(t, repo, pkg, "1.3", "", [3]string{index.AttachmentSBOM, spdx, "release-2027"}, slsa)
	lookalike := e.version(t, repo, pkg, "1.4", "", [3]string{index.AttachmentSBOM, spdx + "X", "release"}, slsa)
	otherSigner := e.version(t, repo, pkg, "1.5", "", [3]string{index.AttachmentSBOM, spdx, "ci"}, slsa)

	if v := e.decide(t, both); !v.Allowed() || v.Requirement != nil {
		t.Errorf("a version with both: %+v", v)
	}
	first := fmt.Sprintf("requires an attachment %s signed by release or release-2027", spdx)
	for name, tt := range map[string]struct {
		s      Subject
		reason string
		req    index.Requirement
	}{
		"the other variant of a version that has both": {other, first, reqs[0]},
		"without provenance":                           {noProvenance, "requires an attachment " + provenance + " signed by ci", reqs[1]},
		"without anything":                             {none, first, reqs[0]},
		"signed by a name the trust dropped":           {dropped, first, reqs[0]},
		"of a type that starts like it":                {lookalike, first, reqs[0]},
		"signed by a trusted name it does not name":    {otherSigner, first, reqs[0]},
		"of a digest that is no version":               {Subject{Repo: repo, Name: "app", Version: "9", Digest: digest.FromString("x")}, first, reqs[0]},
		"without a digest":                             {Subject{Repo: repo, Name: "app", Version: "9"}, first, reqs[0]},
	} {
		v := e.decide(t, tt.s)
		if v.Effect != EffectDeny || v.Reason != tt.reason || v.Requirement == nil || v.Requirement.PredicateType != tt.req.PredicateType ||
			strings.Join(v.Requirement.SignedBy, " ") != strings.Join(tt.req.SignedBy, " ") || v.Rule != 0 {
			t.Errorf("%s: %+v, want denied: %s", name, v, tt.reason)
		}
	}
	if err := Refusal(none, e.decide(t, none)); err == nil || err.Error() != "app 1.2 is denied by policy: "+first {
		t.Errorf("the refusal: %v", err)
	}

	// Leaving them out: an upload, a push, a producer's read.
	if v := e.decide(t, Subject{Repo: none.Repo, Name: none.Name, Version: none.Version, Digest: none.Digest, NoRequirements: true}); !v.Allowed() {
		t.Errorf("a subject without requirements: %+v", v)
	}
}

// TestDecideRequirementOrder: no allow rule lets a version without its attachments through, an
// allow list beside a deny rule neither; a deny verdict comes before the requirement, a quarantine
// verdict after it.
func TestDecideRequirementOrder(t *testing.T) {
	e := newEnv(t)
	e.d.o.Trust = trustOf(t, map[string][]string{"release": {spdx}, "ci": nil})
	repo := e.repo(t, "pypi", "prod", index.KindHosted, requiring(t, index.Requirement{PredicateType: spdx, SignedBy: []string{"release"}},
		index.Requirement{PredicateType: provenance, SignedBy: []string{"ci"}}))
	pkg := index.Package{RepoID: repo.ID, Name: "app", NameOCI: "app"}
	must(t, e.st.UpsertPackage(t.Context(), &pkg))
	sbom := [3]string{index.AttachmentSBOM, spdx, "release"}
	both := e.version(t, repo, pkg, "1.0", "", sbom, [3]string{index.AttachmentProvenance, provenance + "/v1", "ci"})
	noProvenance := e.version(t, repo, pkg, "1.1", "", sbom)
	none := e.version(t, repo, pkg, "1.2", "")

	e.rule(t, "pypi/prod", "app", "*", "allow", "ours")
	e.rule(t, "pypi/*", "*", "*", "deny", "only what is allowed")
	if v := e.decide(t, none); v.Allowed() || v.Requirement == nil {
		t.Errorf("under an allow rule: %+v", v)
	}
	if v := e.decide(t, both); !v.Allowed() || v.Rule == 0 {
		t.Errorf("a version that meets them, under the allow rule: %+v", v)
	}

	// A deny verdict comes first; a quarantine verdict shows once the requirement is met.
	ref := e.verdict(t, repo, none.Digest, "deny", "", nil)
	if v := e.decide(t, none); v.Attestation != ref || v.Requirement != nil {
		t.Errorf("a deny verdict and a requirement not met: %+v, want the verdict", v)
	}
	until := e.now.Add(time.Hour)
	e.verdict(t, repo, noProvenance.Digest, "quarantine", "", &until)
	e.rule(t, "pypi/prod", "app", "==1.1", "deny", "not this one") // the allow rule wins over it
	if v := e.decide(t, noProvenance); v.Requirement == nil {
		t.Errorf("quarantined and without provenance: %+v, want the requirement", v)
	}
}

// TestDecideRequirementKinds: a signature counts as cosign's predicate type, and a quarantine by
// a verdict shows once the attachment is there.
func TestDecideRequirementKinds(t *testing.T) {
	e := newEnv(t)
	e.d.o.Trust = trustOf(t, map[string][]string{"release": {signature}})
	repo := e.repo(t, "raw", "files", index.KindHosted, requiring(t, index.Requirement{PredicateType: signature, SignedBy: []string{"release"}}))
	pkg := index.Package{RepoID: repo.ID, Name: "tool", NameOCI: "tool"}
	must(t, e.st.UpsertPackage(t.Context(), &pkg))
	signed := e.version(t, repo, pkg, "1.0", "", [3]string{index.AttachmentSignature, "", "release"})
	if v := e.decide(t, signed); !v.Allowed() {
		t.Errorf("a legacy signature without a predicate type: %+v", v)
	}
	unsigned := e.version(t, repo, pkg, "2.0", "")
	until := e.now.Add(time.Hour)
	e.verdict(t, repo, unsigned.Digest, "quarantine", "", &until)
	if v := e.decide(t, unsigned); v.Effect != EffectDeny || v.Requirement == nil {
		t.Errorf("quarantined and unsigned: %+v, want the requirement", v)
	}
	e.version(t, repo, pkg, "2.0", "", [3]string{index.AttachmentSignature, signature, "release"})
	if v := e.decide(t, unsigned); v.Effect != EffectQuarantine || !v.Lifts.Equal(until) {
		t.Errorf("quarantined and signed: %+v, want the quarantine", v)
	}
	// Without a trust, no name meets anything.
	e.d.o.Trust = nil
	if v := e.decide(t, signed); v.Allowed() {
		t.Error("a decider without a trust lets a requirement through")
	}
}

// TestRefuseRequirement: the audit row of a requirement's refusal carries it as the repo's
// policy has it.
func TestRefuseRequirement(t *testing.T) {
	e := newEnv(t)
	req := index.Requirement{PredicateType: spdx, SignedBy: []string{"release", "gone"}}
	repo := e.repo(t, "pypi", "prod", index.KindHosted, requiring(t, req))
	s := Subject{Repo: repo, Name: "app", Version: "1.4.0", Digest: digest.FromString("app")}
	var de *DeniedError
	if err := e.d.Check(t.Context(), s); !errors.As(err, &de) {
		t.Fatalf("Check: %v", err)
	}
	e.d.Refuse(t.Context(), SurfaceDownload, de, auth.Anonymous)
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: ActionRefuse})
	must(t, err)
	if len(evs) != 1 {
		t.Fatalf("%d rows", len(evs))
	}
	var details struct {
		Reason      string             `json:"reason"`
		Effect      string             `json:"effect"`
		Requirement *index.Requirement `json:"requirement"`
		Rule        *int64             `json:"rule"`
	}
	must(t, json.Unmarshal(evs[0].Details, &details))
	if details.Effect != EffectDeny || details.Reason != "requires an attachment "+spdx+" signed by release or gone" ||
		details.Requirement == nil || details.Requirement.PredicateType != spdx || strings.Join(details.Requirement.SignedBy, ",") != "release,gone" ||
		details.Rule != nil {
		t.Errorf("the row's details: %s", evs[0].Details)
	}
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
