package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// ciFixture is the API with the CI trust policies of a GitHub issuer, one policy of the
// configuration, the policies of the API that write within oci/*, approved by a second admin; the
// admin role for admin only.
func ciFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, Options{})
	cfg := config.CI{APIWriteRepos: []string{"oci/*"}, APIWriteApproval: config.CIApprovalSecondAdmin,
		Issuers: []config.CIIssuer{{Name: "github", Platform: config.CIPlatformGitHub, Issuer: "https://token.actions.example"}},
		Policies: []config.CIPolicy{{Name: "from-config", Issuer: "github", Claims: map[string]config.CIValues{"repository_owner_id": {"9"}},
			Grants: []config.CIGrant{{Role: "reader", Mask: "*/*"}}}}}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, CIPolicies: &ci.Service{Index: f.st, Config: cfg},
		Auth: fakeAuth{grants: map[string]bool{"user:admin manage */*": true}}}))
	return f
}

const writerBody = `{"name": "w", "issuer": "github", "claims": {"repository_id": ["1"], "ref": ["refs/heads/main"]}, "grants": [{"role": "writer", "mask": "oci/app"}]}`

const readBody = `{"name": "acme-read", "issuer": "github", "claims": {"repository_id": ["123"]}, "grants": [{"role": "reader", "mask": "oci/*"}]}`

// TestCIPoliciesAPI: the admin makes, reads, replaces and deletes the CI trust policies of the
// API, each change an audit row; the configuration's are listed and read-only (409); the rules
// are the configuration's (400 naming the field); a policy that writes is 403 without a fresh
// sign-in; anyone else is 403 before the body is read.
func TestCIPoliciesAPI(t *testing.T) {
	f := ciFixture(t)
	rr := f.do(http.MethodPost, "/api/v1/ci/policies", readBody, admin)
	var p ci.Policy
	decodeJSON(t, rr, http.StatusCreated, &p)
	if rr.Header().Get("Location") != "/api/v1/ci/policies/acme-read" || p.Source != "api" || p.CreatedBy == "" || p.CreatedAt == nil {
		t.Errorf("POST = %+v, Location %q", p, rr.Header().Get("Location"))
	}
	var list CIPolicyList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/ci/policies", "", admin), http.StatusOK, &list)
	if len(list.Policies) != 2 || list.Policies[0].Source != "config" || list.Policies[1].Name != "acme-read" {
		t.Errorf("GET = %+v", list)
	}
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/ci/policies/from-config", "", admin), http.StatusOK, &p)
	if p.Source != "config" || p.Claims["repository_owner_id"][0] != "9" {
		t.Errorf("GET of the configuration's = %+v", p)
	}
	replaced := strings.Replace(readBody, `["123"]`, `["124"]`, 1)
	decodeJSON(t, f.do(http.MethodPut, "/api/v1/ci/policies/acme-read", replaced, admin), http.StatusOK, &p)
	if p.Claims["repository_id"][0] != "124" || p.Name != "acme-read" {
		t.Errorf("PUT = %+v", p)
	}
	for _, tt := range []struct {
		name, method, target, body string
		status                     int
		code, msg                  string
	}{
		{"a stranger", http.MethodPost, "/api/v1/ci/policies", "not json", http.StatusForbidden, codeForbidden, ""},
		{"no id", http.MethodPost, "/api/v1/ci/policies", `{"name": "x", "issuer": "github", "claims": {"repository": ["acme/app"]}, "grants": [{"role": "reader", "mask": "*/*"}]}`,
			http.StatusBadRequest, codeInvalid, "claims: must name the project by an id"},
		{"a wildcard alone", http.MethodPost, "/api/v1/ci/policies", strings.Replace(readBody, `"claims": {`, `"claims": {"ref": ["**"], `, 1),
			http.StatusBadRequest, codeInvalid, `claims.ref: "**" is only *`},
		{"an unknown issuer", http.MethodPost, "/api/v1/ci/policies", strings.Replace(readBody, `"github"`, `"gitlab"`, 1),
			http.StatusBadRequest, codeInvalid, "issuer"},
		{"a policy that writes", http.MethodPost, "/api/v1/ci/policies", writerBody, http.StatusForbidden, codeForbidden,
			"a policy of the API that grants write: this needs a sign-in with a second factor"},
		{"a name taken", http.MethodPost, "/api/v1/ci/policies", readBody, http.StatusConflict, codeConflict, "exists already"},
		{"the configuration's name", http.MethodPost, "/api/v1/ci/policies", strings.Replace(readBody, "acme-read", "from-config", 1),
			http.StatusConflict, codeConflict, "configuration"},
		{"replacing the configuration's", http.MethodPut, "/api/v1/ci/policies/from-config", strings.Replace(readBody, "acme-read", "from-config", 1),
			http.StatusConflict, codeConflict, "configuration"},
		{"deleting the configuration's", http.MethodDelete, "/api/v1/ci/policies/from-config", "", http.StatusConflict, codeConflict, "configuration"},
		{"an unknown field", http.MethodPost, "/api/v1/ci/policies", `{"name": "x", "role": "admin"}`, http.StatusBadRequest, codeInvalid, ""},
		{"none", http.MethodGet, "/api/v1/ci/policies/none", "", http.StatusNotFound, codeNotFound, ""},
	} {
		who := admin
		if tt.name == "a stranger" {
			who = alice
		}
		wantError(t, f.do(tt.method, tt.target, tt.body, who), tt.status, tt.code, tt.msg)
	}
	if rr := f.do(http.MethodDelete, "/api/v1/ci/policies/acme-read", "", admin); rr.Code != http.StatusNoContent {
		t.Errorf("DELETE: %d %s", rr.Code, rr.Body)
	}
	checkCIPolicyRows(t, f.st)
}

// TestCIPoliciesAPIOff: without the service, the routes are 501.
func TestCIPoliciesAPIOff(t *testing.T) {
	wantError(t, newFixture(t, Options{}).do(http.MethodGet, "/api/v1/ci/policies", "", admin), http.StatusNotImplemented, codeUnavailable, "not enabled")
}

// checkCIPolicyRows checks the rows of TestCIPoliciesAPI: a create, a replace and a delete, each
// by the admin with the policy and the source api.
func checkCIPolicyRows(t *testing.T, st index.Store) {
	t.Helper()
	rows, err := st.ListAudit(t.Context(), index.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		if !strings.HasPrefix(r.Action, "ci.policy.") {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal(r.Details, &d)
		if r.Subject != "acme-read" || d["source"] != "api" || d["issuer"] != "github" || !strings.HasPrefix(r.Principal, "user:") {
			t.Errorf("%s row %+v, details %v", r.Action, r, d)
		}
		got = append(got, r.Action)
	}
	if strings.Join(got, " ") != "ci.policy.delete ci.policy.update ci.policy.create" {
		t.Errorf("rows %v", got)
	}
}
