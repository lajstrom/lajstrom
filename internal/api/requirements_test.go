package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/attest"
	"github.com/lajstrom/lajstrom/internal/attest/attesttest"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
)

// requirementsTrust is a trust of the keys release (SPDX and signatures) and ci (every type).
func requirementsTrust(t *testing.T) *attest.Trust {
	t.Helper()
	trust, err := attest.NewTrust([]attest.Entry{attesttest.NewKey(t).Entry(t, "release", "https://spdx.dev/Document",
		"https://sigstore.dev/cosign/sign/v1"), attesttest.NewKey(t).Entry(t, "ci")})
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

// TestRequireAttachmentsPolicy: policy.requireAttachments is checked when a repo is created and
// when its policy changes (docs/spec/policy.md §11): its form, its names against the trust, and
// the repos that cannot have one; null removes it; a change of another key of a repo whose
// requirements the trust no longer takes is refused, one of anonymousRead is not.
func TestRequireAttachmentsPolicy(t *testing.T) {
	f := newProxyFixture(t)
	f.repos.Formats = oneOf("raw", "pypi", "npm")
	f.repos.Trust = requirementsTrust(t)
	f.repos.Encryption = true
	const sbom = `{"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]}`
	create := func(name, kind, policy string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"format": "pypi", "name": %q, "registry": "local", "policy": %s}`, name, policy)
		if kind == "proxy" {
			body = fmt.Sprintf(`{"format": "npm", "name": %q, "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs"}, "policy": %s}`,
				name, policy)
		}
		return f.do(http.MethodPost, "/api/v1/repos", body, admin)
	}
	var created Repo
	decodeJSON(t, (create("prod", "", `{"requireAttachments": [`+sbom+`, {"predicateType": "https://sigstore.dev/cosign/sign/v1", "signedBy": ["ci", "release"]}]}`)),
		http.StatusCreated, &created)
	if !strings.Contains(string(created.Policy), `"signedBy":["ci","release"]`) {
		t.Errorf("the created policy: %s", created.Policy)
	}

	many := strings.TrimSuffix(strings.Repeat(`{"predicateType": "https://example.com/t", "signedBy": ["ci"]}, `, 17), ", ")
	for _, tt := range []struct {
		name, kind, policy, want string
	}{
		{"not a list", "", `{"requireAttachments": {"predicateType": "x"}}`, "policy.requireAttachments: must be a list"},
		{"an empty list", "", `{"requireAttachments": []}`, "policy.requireAttachments: must be a list of 1 to 16 requirements"},
		{"17 requirements", "", `{"requireAttachments": [` + many + `]}`, "policy.requireAttachments: must be a list of 1 to 16"},
		{"not an object", "", `{"requireAttachments": ["https://spdx.dev/Document"]}`, "policy.requireAttachments[0]: must be a JSON object"},
		{"another key", "", `{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release"], "kind": "sbom"}]}`,
			"policy.requireAttachments[0].kind: unknown key; the keys are predicateType, signedBy"},
		{"a type that is no string", "", `{"requireAttachments": [{"predicateType": 1, "signedBy": ["release"]}]}`,
			"policy.requireAttachments[0].predicateType: must be a string"},
		{"no type", "", `{"requireAttachments": [{"signedBy": ["release"]}]}`, "policy.requireAttachments[0].predicateType: \"\" is not an absolute URI"},
		{"names that are no strings", "", `{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": [1]}]}`,
			"policy.requireAttachments[0].signedBy: must be a list of names"},
		{"a name the trust lacks", "", `{"requireAttachments": [` + sbom + `, {"predicateType": "https://spdx.dev/Document", "signedBy": ["relase"]}]}`,
			`policy.requireAttachments[1].signedBy[0]: "relase" is neither a key of attachments.trust`},
		{"a name trusted for other types", "", `{"requireAttachments": [{"predicateType": "https://slsa.dev/provenance", "signedBy": ["ci"]}, ` +
			`{"predicateType": "https://slsa.dev/provenance/v1", "signedBy": ["release"]}]}`, "policy.requireAttachments[1].signedBy[0]"},
		{"the policy key", "", `{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["lajstrom"]}]}`,
			"policy.requireAttachments[0].signedBy[0]: \"lajstrom\" is the policy key"},
		{"twice the same", "", `{"requireAttachments": [` + sbom + `, ` + sbom + `]}`, "policy.requireAttachments[1]: is the same as requireAttachments[0]"},
		{"a proxy repo", "proxy", `{"requireAttachments": [` + sbom + `]}`, "policy.requireAttachments: a proxy repo cannot require attachments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, (create("other", tt.kind, tt.policy)), http.StatusBadRequest, codeInvalid, tt.want)
		})
	}
	if _, err := f.repos.Create(t.Context(), CreateRequest{Format: "oci", Name: "images", Registry: "local",
		Policy: json.RawMessage(`{"requireAttachments": [` + sbom + `]}`)}, Origin{}); err == nil || !strings.Contains(err.Error(), "an oci repo") {
		t.Errorf("an oci repo with requirements: %v", err)
	}

	// A change: encryption at rest is refused beside requirements; another key keeps them, and
	// null removes them.
	patch := func(body string) *httptest.ResponseRecorder {
		return f.do(http.MethodPatch, "/api/v1/repos/pypi/prod", body, admin)
	}
	wantError(t, (patch(fmt.Sprintf(`{"policy": %s}`, atRestFor(t)))), http.StatusBadRequest, codeInvalid,
		"policy.requireAttachments: a repo encrypted at rest cannot require attachments")
	var updated Repo
	decodeJSON(t, (patch(`{"policy": {"maxFileSize": 1024}}`)), http.StatusOK, &updated)
	if !strings.Contains(string(updated.Policy), "requireAttachments") || !strings.Contains(string(updated.Policy), "maxFileSize") {
		t.Errorf("after a change of another key: %s", updated.Policy)
	}
	if ev := auditOf(t, f.st, "repo.update")[0]; !strings.Contains(fmt.Sprint(ev.details["changed"]), "requireAttachments") {
		t.Errorf("the repo.update row's details: %v, want the policy as the change left it", ev.details)
	}

	// The trust no longer takes a name: a change of the policy is refused, one of anonymousRead is
	// not.
	f.repos.Trust = nil
	wantError(t, (patch(`{"policy": {"maxFileSize": 2048}}`)), http.StatusBadRequest, codeInvalid, "policy.requireAttachments[0].signedBy[0]")
	decodeJSON(t, (patch(`{"anonymousRead": true}`)), http.StatusOK, &updated)
	decodeJSON(t, (patch(`{"policy": {"requireAttachments": null}}`)), http.StatusOK, &updated)
	if strings.Contains(string(updated.Policy), "requireAttachments") {
		t.Errorf("after null: %s", updated.Policy)
	}
}

// TestRequireAttachmentsBootstrap: a configured repo whose requirements name a signer the trust
// lacks is an error of the bootstrap, naming the entry and the field.
func TestRequireAttachmentsBootstrap(t *testing.T) {
	s := &Repos{Index: newStore(t), Formats: oneOf("raw"), Registries: oneOf("local"), Trust: requirementsTrust(t)}
	repo := func(name, signer string) config.Repo {
		return config.Repo{Format: "raw", Name: name, Registry: "local", Policy: map[string]any{"requireAttachments": []any{
			map[string]any{"predicateType": "https://sigstore.dev/cosign/sign/v1", "signedBy": []any{signer}}}}}
	}
	if created, err := s.Bootstrap(t.Context(), []config.Repo{repo("signed", "release")}); err != nil || created != 1 {
		t.Fatalf("Bootstrap of a valid requirement: %d, %v", created, err)
	}
	_, err := s.Bootstrap(t.Context(), []config.Repo{repo("typo", "relase")})
	if err == nil || !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "repos[0]") ||
		!strings.Contains(err.Error(), "policy.requireAttachments[0].signedBy[0]") {
		t.Errorf("Bootstrap of a name the trust lacks: %v", err)
	}
}

// TestRequireAttachmentsInvalidate: a change of a repo's requirements invalidates its documents
// in the transaction of the change; a change of another key of its policy does not (docs/spec/
// policy.md §11).
func TestRequireAttachmentsInvalidate(t *testing.T) {
	ctx := t.Context()
	s := &Repos{Index: newStore(t), Formats: oneOf("raw"), Registries: oneOf("local"), Trust: requirementsTrust(t)}
	_, err := s.Create(ctx, CreateRequest{Format: "raw", Name: "prod", Registry: "local"}, Origin{})
	must(t, err)
	row, err := s.Index.GetRepo(ctx, "raw", "prod")
	must(t, err)
	gen := func() int64 {
		t.Helper()
		g, err := s.Index.IndexCacheGeneration(ctx, row.ID)
		must(t, err)
		return g
	}
	for _, tt := range []struct {
		policy string
		moves  bool
	}{
		{`{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]}]}`, true},
		{`{"maxFileSize": 1024}`, false},
		{`{"requireAttachments": [{"predicateType": "https://spdx.dev/Document", "signedBy": ["release", "ci"]}]}`, true},
		{`{"requireAttachments": null}`, true},
	} {
		before := gen()
		_, err := s.Update(ctx, "raw", "prod", UpdateRequest{Policy: json.RawMessage(tt.policy)}, Origin{})
		must(t, err)
		if moved := gen() > before; moved != tt.moves {
			t.Errorf("%s: the documents invalidated %v, want %v", tt.policy, moved, tt.moves)
		}
	}
}

// TestListVersionsUnmet (#1287, http-routing.md §4): ?unmet=<predicateType>&signedBy=<name> keeps
// the variants that have no attachment verified of the type, or of a longer version of it, signed
// by one of the names the trust has, whatever the repo's policy says; with ?without=, a variant
// meets both; the refusals of what is no requirement.
func TestListVersionsUnmet(t *testing.T) {
	f := newFixture(t, Options{})
	trust, err := attest.NewTrust([]attest.Entry{attesttest.NewKey(t).Entry(t, "scanner"), attesttest.NewKey(t).Entry(t, "other")})
	must(t, err)
	f.repos.Trust = trust
	repo := f.create(t, "pypi", "libs")
	publishDetailed(t, f.st, repo.ID)
	attachDetailed(t, f.st, repo.ID) // the wheel: a CycloneDX SBOM verified, of scanner; the sdist: an unsigned SPDX
	target := "/api/v1/repos/pypi/libs/packages/requests/versions"
	variants := func(query string) string {
		t.Helper()
		var got struct {
			Versions []struct {
				Version  string `json:"version"`
				Variants []struct {
					Variant string `json:"variant"`
				} `json:"variants"`
			} `json:"versions"`
		}
		decodeJSON(t, f.do(http.MethodGet, target+query, "", admin), http.StatusOK, &got)
		var out []string
		for _, v := range got.Versions {
			for _, va := range v.Variants {
				out = append(out, v.Version+"/"+va.Variant)
			}
		}
		return strings.Join(out, " ")
	}
	const both = "1.0/py3-none-any 1.0/sdist"
	cases := []struct{ query, want string }{
		{"?unmet=https://cyclonedx.org/bom&signedBy=scanner", "1.0/sdist"},
		{"?unmet=https://cyclonedx.org&signedBy=scanner", "1.0/sdist"},
		{"?unmet=https://cyclonedx.org/bom/v1.6&signedBy=scanner", both},
		{"?unmet=https://cyclonedx.org/bom&signedBy=other", both},
		{"?unmet=https://cyclonedx.org/bom&signedBy=gone&signedBy=scanner", "1.0/sdist"},
		{"?unmet=https://cyclonedx.org/bom&signedBy=gone", both},
		{"?unmet=https://cyclonedx.org/bom&signedBy=scanner&without=scan", "1.0/sdist"},
		{"?unmet=https://cyclonedx.org/bom&signedBy=scanner&without=sbom", ""},
	}
	for _, tt := range cases {
		if got := variants(tt.query); got != tt.want {
			t.Errorf("versions%s: %q, want %q", tt.query, got, tt.want)
		}
	}
	// A trust that dropped scanner: its attachment meets nothing.
	f.repos.Trust = nil
	if got := variants(cases[0].query); got != both {
		t.Errorf("versions%s without scanner in the trust: %q, want both", cases[0].query, got)
	}
	names := strings.Repeat("&signedBy=a", 17)
	for _, tt := range []struct{ query, want string }{
		{"?unmet=https://cyclonedx.org/bom", "unmet: needs signedBy"},
		{"?signedBy=scanner", "signedBy: needs unmet"},
		{"?unmet=https://cyclonedx.org/bom&unmet=https://spdx.dev/Document&signedBy=scanner", "unmet: given 2 times"},
		{"?unmet=cyclonedx&signedBy=scanner", `unmet: "cyclonedx" is not an absolute URI`},
		{"?unmet=https://e.com/" + strings.Repeat("a", 243) + "&signedBy=scanner", "unmet: is longer than 256 bytes"},
		{"?unmet=https://cyclonedx.org/bom" + names, "signedBy: 17 names, more than 16"},
	} {
		wantError(t, f.do(http.MethodGet, target+tt.query, "", admin), http.StatusBadRequest, "INVALID", tt.want)
	}
}
