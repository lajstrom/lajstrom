package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/ci"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/policy"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// reasonFixture is the API with every service whose changes take a reason (docs/spec/rbac.md
// §13): the rules, a deny's policy key and verdicter, the webhooks and the CI trust policies; the
// users admin, alice and carol, the repo raw/open with the version tool 0123456789abcdef; the
// grants of the authorization tests, admin's everything; auth.requireReason as require says.
func reasonFixture(t *testing.T, require bool) (*fixture, *fakeVerdicter) {
	t.Helper()
	f := newFixture(t, Options{})
	master, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVerdicter{st: f.st}
	hooks := &webhook.Service{Index: f.st, Master: master, Formats: oneOf("raw", "pypi"), Replica: "replica-test",
		Deliverer: webhook.NewDeliverer(webhook.Options{}),
		Source:    "https://art.example.com"}
	cis := &ci.Service{Index: f.st, Config: config.CI{APIWriteApproval: config.CIApprovalSecondAdmin,
		Issuers: []config.CIIssuer{{Name: "github", Platform: config.CIPlatformGitHub, Issuer: "https://token.actions.example"}}}}
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, Auth: fakeAuth{grants: grants}, RequireReason: require,
		Rules:     &Rules{Index: f.st, Formats: oneOf("raw", "pypi"), Now: func() time.Time { return testNow }},
		PolicyKey: newTestPolicyKey(t), Verdicter: v, Webhooks: hooks, CIPolicies: cis}))
	f.addUsers(t, "admin", "alice", "carol")
	addVersion(t, f.st, f.create(t, "raw", "open").ID, "tool")
	return f, v
}

const ciBody = `{"name": "app", "issuer": "github", "claims": {"repository_id": ["1"]}, "grants": [{"role": "reader", "mask": "raw/*"}]`

// TestChangeReason: a change of the control plane takes changeReason in its body, or
// ?changeReason= on a route without one, into its audit rows as details.changeReason, a deny's
// into the rows that ingest writes; both forms, a parameter twice and a reason outside its bounds
// are 400 naming it, and nothing is written.
func TestChangeReason(t *testing.T) {
	f, v := reasonFixture(t, false)
	var b auth.Binding
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/bindings",
		`{"principal": "user:carol", "role": "reader", "repoMask": "raw/*", "changeReason": "CHG-1 carol reads"}`, admin), 201, &b)
	wantStatus(t, f.do(http.MethodDelete, fmt.Sprintf("/api/v1/bindings/%d?changeReason=CHG-2", b.ID), "", admin), 204)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/users/carol/tokens", `{"name": "ci", "changeReason": "CHG-3"}`, admin), 201)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/policy/rules",
		`{"repoMask": "raw/open", "name": "evil", "reason": "CVE-2026-1", "changeReason": "CHG-4"}`, admin), 201)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/ci/policies", ciBody+`, "changeReason": "CHG-5"}`, admin), 201)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/webhooks",
		`{"mask": "raw/*", "url": "https://ci.example.com/hook", "changeReason": "CHG-6"}`, admin), 201)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos/raw/open/packages/tool/versions/0123456789abcdef/deny",
		`{"reason": "CVE-2026-2", "changeReason": "CHG-7"}`, admin), 201)
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/raw/open?force=true&changeReason=CHG-8", "", admin), 204)
	for action, want := range map[string]string{auth.ActionBindingCreate: "CHG-1 carol reads", auth.ActionBindingDelete: "CHG-2",
		auth.ActionTokenCreate: "CHG-3", policy.ActionRuleCreate: "CHG-4", ci.ActionPolicyCreate: "CHG-5",
		webhook.ActionCreate: "CHG-6", actionDelete: "CHG-8"} {
		if got := oneAudit(t, f.st, action).details["changeReason"]; got != want {
			t.Errorf("%s: changeReason %v, want %q", action, got, want)
		}
	}
	if len(v.reasons) != 1 || v.reasons[0] != "CHG-7" {
		t.Errorf("the reasons of the deny's rows: %q, want CHG-7", v.reasons)
	}

	before, err := f.st.ListAudit(context.Background(), index.AuditFilter{})
	must(t, err)
	for _, tt := range []struct {
		name, method, target, body, msg string
	}{
		{"both forms", http.MethodPost, "/api/v1/bindings?changeReason=a",
			`{"principal": "user:carol", "role": "reader", "repoMask": "raw/*", "changeReason": "b"}`, "in the body or in the query, not both"},
		{"twice in the query", http.MethodDelete, "/api/v1/users/carol?changeReason=a&changeReason=b", "", "once in the query"},
		{"empty", http.MethodPost, "/api/v1/users", `{"name": "dave", "password": "dave-password", "changeReason": ""}`, "empty"},
		{"too long", http.MethodPatch, "/api/v1/users/carol", `{"disabled": true, "changeReason": "` + strings.Repeat("é", 513) + `"}`,
			"1026 bytes, at most 1024"},
		{"a line break", http.MethodPost, "/api/v1/users/carol/password", `{"password": "carol-password", "changeReason": "a\nb"}`,
			"a control character"},
		{"not UTF-8", http.MethodPost, "/api/v1/users/carol/unlock?changeReason=%ff", "", "not UTF-8"},
	} {
		rr := f.do(tt.method, tt.target, tt.body, admin)
		wantError(t, rr, http.StatusBadRequest, codeInvalid, "changeReason: "+tt.msg)
	}
	after, err := f.st.ListAudit(context.Background(), index.AuditFilter{})
	must(t, err)
	if len(after) != len(before) {
		t.Errorf("the refused changes wrote %d audit rows", len(after)-len(before))
	}
	if _, err := f.st.GetUser(context.Background(), "dave"); err == nil {
		t.Error("a user made with a refused reason")
	}
}

// TestRequireReason: with auth.requireReason, every change of the control plane of
// docs/spec/rbac.md §13 without changeReason is 400 naming it and the setting, before anything is
// written and after the role: a principal without it gets its 401 or 403. A person's own tokens
// need none.
func TestRequireReason(t *testing.T) {
	f, v := reasonFixture(t, true)
	var hook CreatedWebhook
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/webhooks", `{"mask": "raw/*", "url": "https://ci.example.com/hook", "changeReason": "x"}`,
		admin), 201, &hook)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/ci/policies", ciBody+`, "changeReason": "x"}`, admin), 201)
	hooks := fmt.Sprintf("/api/v1/webhooks/%d", hook.ID)
	const version = "/api/v1/repos/raw/open/packages/tool/versions/0123456789abcdef/deny"
	before, err := f.st.ListAudit(context.Background(), index.AuditFilter{})
	must(t, err)
	for _, tt := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/v1/repos", `{"format": "raw", "name": "new", "registry": "local"}`},
		{http.MethodPatch, "/api/v1/repos/raw/open", `{"anonymousRead": true}`},
		{http.MethodDelete, "/api/v1/repos/raw/open", ""},
		{http.MethodPost, "/api/v1/users", `{"name": "dave", "password": "dave-password"}`},
		{http.MethodPatch, "/api/v1/users/carol", `{"disabled": true}`},
		{http.MethodDelete, "/api/v1/users/carol", ""},
		{http.MethodPost, "/api/v1/users/carol/password", `{"password": "carol-password"}`},
		{http.MethodPost, "/api/v1/users/carol/unlock", ""},
		{http.MethodPost, "/api/v1/users/carol/tokens", `{"name": "ci"}`},
		{http.MethodDelete, "/api/v1/users/carol/tokens/1", ""},
		{http.MethodPost, "/api/v1/bindings", `{"principal": "user:carol", "role": "reader", "repoMask": "raw/*"}`},
		{http.MethodDelete, "/api/v1/bindings/1", ""},
		{http.MethodPost, "/api/v1/repos/raw/open/deploy-tokens", `{"name": "d", "scopes": ["read"]}`},
		{http.MethodDelete, "/api/v1/repos/raw/open/deploy-tokens/1", ""},
		{http.MethodPost, "/api/v1/deploy-tokens", `{"repoMask": "raw/*", "name": "m", "scopes": ["read"]}`},
		{http.MethodDelete, "/api/v1/deploy-tokens/1", ""},
		{http.MethodPost, "/api/v1/webhooks", `{"mask": "raw/*", "url": "https://ci.example.com/other"}`},
		{http.MethodPatch, hooks, `{"active": false}`},
		{http.MethodDelete, hooks, ""},
		{http.MethodPost, hooks + "/secret", ""},
		{http.MethodPost, "/api/v1/policy/rules", `{"repoMask": "raw/open", "name": "evil", "reason": "CVE-2026-1"}`},
		{http.MethodPost, version, `{"reason": "CVE-2026-2"}`},
		{http.MethodDelete, version, ""},
		{http.MethodPost, "/api/v1/ci/policies", strings.Replace(ciBody, `"app"`, `"other"`, 1) + "}"},
		{http.MethodPut, "/api/v1/ci/policies/app", ciBody + "}"},
		{http.MethodPost, "/api/v1/ci/policies/app/renew", ""},
		{http.MethodPost, "/api/v1/ci/policies/app/approve", ""},
		{http.MethodDelete, "/api/v1/ci/policies/app", ""},
	} {
		rr := f.do(tt.method, tt.target, tt.body, admin)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "changeReason: required (auth.requireReason)") {
			t.Errorf("%s %s without a reason: %d %s", tt.method, tt.target, rr.Code, rr.Body)
		}
	}
	after, err := f.st.ListAudit(context.Background(), index.AuditFilter{})
	must(t, err)
	if len(after) != len(before) || len(v.attached) != 0 {
		t.Errorf("the changes without a reason wrote %d audit rows and %d verdicts", len(after)-len(before), len(v.attached))
	}
	if _, err := f.st.GetRepo(context.Background(), "raw", "open"); err != nil {
		t.Errorf("raw/open after its delete without a reason: %v", err)
	}

	// The role first: whoever does not have it is not told about the reason.
	wantStatus(t, f.do(http.MethodPost, "/api/v1/bindings", `{"principal": "user:carol", "role": "admin", "repoMask": "*/*"}`, bob), 403)
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/bindings/1", "", auth.Anonymous), 401)
	// A person's own token needs none; another's, with one, is made.
	var own created
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/users/alice/tokens", `{"name": "laptop"}`, alice), 201, &own)
	wantStatus(t, f.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/alice/tokens/%d", own.ID), "", alice), 204)
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/raw/open?force=true&changeReason=CHG-9", "", admin), 204)
}
