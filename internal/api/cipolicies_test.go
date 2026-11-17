package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// ciFixture is the API with the CI trust policies of a GitHub issuer, one policy of the
// configuration, the policies of the API that write within oci/*, approved by a second admin; the
// admin role for admin and root only.
func ciFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, Options{})
	cfg := config.CI{APIWriteRepos: []string{"oci/*"}, APIWriteApproval: config.CIApprovalSecondAdmin,
		Issuers: []config.CIIssuer{{Name: "github", Platform: config.CIPlatformGitHub, Issuer: "https://token.actions.example"}},
		Policies: []config.CIPolicy{{Name: "from-config", Issuer: "github", Claims: map[string]config.CIValues{"repository_owner_id": {"9"}},
			Grants: []config.CIGrant{{Role: "reader", Mask: "*/*"}}}}}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, CIPolicies: &ci.Service{Index: f.st, Config: cfg},
		Auth: fakeAuth{grants: map[string]bool{"user:admin manage */*": true, "user:root manage */*": true}}}))
	return f
}

// root is the second admin of the tests of the CI trust policies.
var root = auth.Principal{Kind: auth.KindUser, Name: "root"}

// doSigned runs a request as p with the token of a web UI's sign-in a minute old with a second
// factor, and the header If-Match when etag is not "".
func (f *fixture) doSigned(method, target, body string, p auth.Principal, etag string) *httptest.ResponseRecorder {
	return f.doSignedIn(method, target, body, p, etag, &auth.SignIn{At: time.Now().Add(-time.Minute), AMR: []string{"pwd", "otp"}})
}

// doSignedIn is doSigned with the sign-in in, nil for the token of a password's sign-in.
func (f *fixture) doSignedIn(method, target, body string, p auth.Principal, etag string, in *auth.SignIn) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	cred := auth.Credential{Kind: auth.CredPAT, TokenID: 3, SignIn: in}
	r = r.WithContext(auth.WithCredential(auth.WithPrincipal(r.Context(), f.asStored(p)), cred))
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, r)
	return rr
}

const writerBody = `{"name": "w", "issuer": "github", "claims": {"repository_id": ["1"], "ref": ["refs/heads/main"]}, "grants": [{"role": "writer", "mask": "oci/app"}]}`

const readBody = `{"name": "acme-read", "issuer": "github", "claims": {"repository_id": ["123"]}, "grants": [{"role": "reader", "mask": "oci/*"}]}`

// TestCIPoliciesAPI: the admin makes, reads, replaces and deletes the CI trust policies of the
// API, each change an audit row, each answer of one of the API with its ETag; the
// configuration's are listed and read-only (409); the rules are the configuration's (400 naming
// the field); a policy that writes needs a fresh sign-in (403); anyone else is 403 before the
// body is read.
func TestCIPoliciesAPI(t *testing.T) {
	f := ciFixture(t)
	rr := f.do(http.MethodPost, "/api/v1/ci/policies", readBody, admin)
	var p ci.Policy
	decodeJSON(t, rr, http.StatusCreated, &p)
	if rr.Header().Get("Location") != "/api/v1/ci/policies/acme-read" || p.Source != "api" || p.State != "active" ||
		p.CreatedBy == "" || p.CreatedAt == nil || rr.Header().Get("ETag") == "" {
		t.Errorf("POST = %+v, Location %q, ETag %q", p, rr.Header().Get("Location"), rr.Header().Get("ETag"))
	}
	etag := rr.Header().Get("ETag")
	checkCIPolicyReads(t, f, etag)
	replaced := strings.Replace(readBody, `["123"]`, `["124"]`, 1)
	decodeJSON(t, f.do(http.MethodPut, "/api/v1/ci/policies/acme-read", replaced, admin), http.StatusOK, &p)
	if p.Claims["repository_id"][0] != "124" || p.Name != "acme-read" {
		t.Errorf("PUT = %+v", p)
	}
	checkCIPolicyRefusals(t, f, etag)
	if rr := f.do(http.MethodDelete, "/api/v1/ci/policies/acme-read", "", admin); rr.Code != http.StatusNoContent {
		t.Errorf("DELETE: %d %s", rr.Code, rr.Body)
	}
	checkCIPolicyRows(t, f.st)
}

// checkCIPolicyReads checks the reads of TestCIPoliciesAPI: acme-read with its ETag etag, listed
// after the configuration's, which is read without one.
func checkCIPolicyReads(t *testing.T, f *fixture, etag string) {
	t.Helper()
	if got := f.do(http.MethodGet, "/api/v1/ci/policies/acme-read", "", admin); got.Header().Get("ETag") != etag {
		t.Errorf("GET's ETag %q, POST's %q", got.Header().Get("ETag"), etag)
	}
	var list CIPolicyList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/ci/policies", "", admin), http.StatusOK, &list)
	if len(list.Policies) != 2 || list.Policies[0].Source != "config" || list.Policies[1].Name != "acme-read" {
		t.Errorf("GET = %+v", list)
	}
	var p ci.Policy
	rr := f.do(http.MethodGet, "/api/v1/ci/policies/from-config", "", admin)
	decodeJSON(t, rr, http.StatusOK, &p)
	if p.Source != "config" || p.State != "active" || p.Claims["repository_owner_id"][0] != "9" || rr.Header().Get("ETag") != "" {
		t.Errorf("GET of the configuration's = %+v, ETag %q", p, rr.Header().Get("ETag"))
	}
}

// checkCIPolicyRefusals checks the refusals of TestCIPoliciesAPI, whose fixture f has acme-read,
// changed since the ETag stale.
func checkCIPolicyRefusals(t *testing.T, f *fixture, stale string) {
	t.Helper()
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
		{"a stale revision", http.MethodDelete, "/api/v1/ci/policies/acme-read", "", http.StatusPreconditionFailed, codeConflict, "changed since it was read"},
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
		r := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
		if tt.name == "a stale revision" {
			r.Header.Set("If-Match", stale)
		}
		rr := httptest.NewRecorder()
		f.h.ServeHTTP(rr, r.WithContext(auth.WithPrincipal(r.Context(), f.asStored(who))))
		wantError(t, rr, tt.status, tt.code, tt.msg)
	}
}

// TestCIPoliciesAPIWrite: a policy that writes, made with a fresh sign-in, waits pending until
// another admin, signed in so too, approves it with the ETag of the revision read (400 without,
// 412 of another, 403 for its maker); a renewal answers the renewal that waits; without the
// service, the routes are 501 (docs/spec/rbac.md §11).
func TestCIPoliciesAPIWrite(t *testing.T) {
	f := ciFixture(t)
	rr := f.doSigned(http.MethodPost, "/api/v1/ci/policies", writerBody, admin, "")
	var p ci.Policy
	decodeJSON(t, rr, http.StatusCreated, &p)
	if p.State != "pending" || p.ExpiresAt == nil || p.UpdatedBy == "" {
		t.Fatalf("POST of a policy that writes = %+v", p)
	}
	etag := rr.Header().Get("ETag")
	wantError(t, f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/approve", "", root, ""), http.StatusBadRequest, codeInvalid,
		"If-Match: an approval names the revision it approves")
	wantError(t, f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/approve", "", root, "*"), http.StatusBadRequest, codeInvalid, "If-Match")
	wantError(t, f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/approve", "", root, `"other"`), http.StatusPreconditionFailed,
		codeConflict, "CI trust policy w changed since it was read")
	wantError(t, f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/approve", "", admin, etag), http.StatusForbidden, codeForbidden,
		"the admin who made the change that waits does not approve it")
	wantError(t, f.do(http.MethodPost, "/api/v1/ci/policies/w/approve", "", root), http.StatusBadRequest, codeInvalid, "If-Match")
	rr = f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/approve", "", root, etag)
	decodeJSON(t, rr, http.StatusOK, &p)
	if p.State != "active" || p.ApprovedBy == "" || rr.Header().Get("ETag") == etag {
		t.Errorf("approve = %+v, ETag %q", p, rr.Header().Get("ETag"))
	}
	rr = f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/renew", "", root, rr.Header().Get("ETag"))
	decodeJSON(t, rr, http.StatusOK, &p)
	if p.State != "active" || p.Renewal == nil {
		t.Errorf("renew = %+v", p)
	}
	wantError(t, f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/renew", `{"expiresAt": "2001-01-01T00:00:00Z"}`, root, ""),
		http.StatusBadRequest, codeInvalid, "expiresAt: must be in the future")
	wantError(t, f.doSigned(http.MethodPost, "/api/v1/ci/policies/from-config/approve", "", root, etag), http.StatusConflict, codeConflict,
		"configuration")
	wantError(t, newFixture(t, Options{}).do(http.MethodGet, "/api/v1/ci/policies", "", admin), http.StatusNotImplemented, codeUnavailable, "not enabled")
}

// TestCIPoliciesAskForMore: on the four routes of a policy that writes, its create, change,
// renewal and approval, a web UI's sign-in through OIDC that is not fresh or shows no second
// factor is asked for more: 401 with RFC 9470's challenge, what is missing and max_age=600, the
// step needing a fresh sign-in (docs/spec/rbac.md §10, #1296). The token of a password's
// sign-in, which cannot sign in again so, is 403.
func TestCIPoliciesAskForMore(t *testing.T) {
	f := ciFixture(t)
	rr := f.doSigned(http.MethodPost, "/api/v1/ci/policies", writerBody, admin, "")
	decodeJSON(t, rr, http.StatusCreated, &ci.Policy{})
	rr = f.doSigned(http.MethodPost, "/api/v1/ci/policies/w/approve", "", root, rr.Header().Get("ETag"))
	decodeJSON(t, rr, http.StatusOK, &ci.Policy{})
	etag := rr.Header().Get("ETag")
	stale := &auth.SignIn{At: time.Now().Add(-time.Hour), AMR: []string{"pwd", "otp"}}
	oneFactor := &auth.SignIn{At: time.Now().Add(-time.Minute), AMR: []string{"pwd"}}
	for _, tc := range []struct {
		name, method, target, body string
		in                         *auth.SignIn
		missing                    string
	}{
		{"create, stale", http.MethodPost, "/api/v1/ci/policies", strings.Replace(writerBody, `"w"`, `"w2"`, 1), stale, "a fresh sign-in"},
		{"change, one factor", http.MethodPut, "/api/v1/ci/policies/w", writerBody, oneFactor, "a second factor"},
		{"renewal, stale", http.MethodPost, "/api/v1/ci/policies/w/renew", "", stale, "a fresh sign-in"},
		{"approval, one factor", http.MethodPost, "/api/v1/ci/policies/w/approve", "", oneFactor, "a second factor"},
		{"approval, neither", http.MethodPost, "/api/v1/ci/policies/w/approve", "", &auth.SignIn{AMR: []string{"pwd"}},
			"a fresh sign-in with a second factor"},
	} {
		rr := f.doSignedIn(tc.method, tc.target, tc.body, root, etag, tc.in)
		wantError(t, rr, http.StatusUnauthorized, codeUnauthenticated, "this needs a sign-in with a second factor within the last 10 minutes",
			"sign in again in a web UI through OIDC (prompt=login)")
		want := `DPoP error="insufficient_user_authentication", error_description="` + tc.missing + `", max_age=600, algs="ES256"`
		if got := rr.Header().Get("WWW-Authenticate"); got != want {
			t.Errorf("%s: WWW-Authenticate %q, want %q", tc.name, got, want)
		}
	}
	rr = f.doSignedIn(http.MethodPost, "/api/v1/ci/policies/w/renew", "", root, etag, nil)
	wantError(t, rr, http.StatusForbidden, codeForbidden, "credential is a personal access token")
	if got := rr.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("the 403 of a password's sign-in has the challenge %q", got)
	}
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
