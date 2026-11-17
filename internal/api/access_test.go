package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// accessAt is the time of the rows of the tests of the access report, with a fraction of a
// second that the document drops.
var accessAt = time.Date(2026, 11, 18, 10, 0, 0, 500_000_000, time.UTC)

// accessUID is the uid of the n-th user of accessRows.
func accessUID(n int) string { return fmt.Sprintf("019b0000-0000-7000-8000-%012d", n) }

// accessRows are the rows of an index.Access at accessAt: alice, a local admin with a personal
// access token unused for 200 days; bob, an OIDC user of the groups ops and dev, which lapse in an
// hour and two, never seen; robot, a service account of alice with an admin binding on raw/app
// and a token without an expiry; zed, a disabled local user with nothing; a cert: pattern and a
// k8s: service account with bindings; a deploy token of raw/app without an expiry and one over
// */*; a CI trust policy of the API that writes raw/app, and the repo raw/public that anonymous
// reads.
func accessRows() *index.Access {
	day := 24 * time.Hour
	ago := func(d time.Duration) *time.Time { t := accessAt.Add(-d); return &t }
	ahead := func(d time.Duration) *time.Time { t := accessAt.Add(d); return &t }
	user := func(n int, name, kind string) index.User {
		return index.User{ID: int64(n), UID: accessUID(n), Name: name, Kind: kind, CreatedAt: accessAt.Add(-300*day + 123*time.Millisecond)}
	}
	alice, bob, robot, zed := user(1, "alice", "local"), user(2, "bob", "oidc"), user(3, "robot", "service"), user(4, "zed", "local")
	alice.LastSeenAt = ago(time.Hour)
	robot.Owner, robot.LastSeenAt = "user:"+alice.UID, ago(time.Minute)
	zed.Disabled = true
	binding := func(id int64, principal, role, mask string) index.Binding {
		return index.Binding{ID: id, Principal: principal, Role: role, RepoMask: mask, CreatedAt: accessAt.Add(-10 * day)}
	}
	app, public := index.Repo{ID: 7, Format: "raw", Name: "app"}, index.Repo{ID: 8, Format: "raw", Name: "public", AnonymousRead: true}
	return &index.Access{
		ReadAt: accessAt,
		Users:  []index.User{alice, bob, robot, zed},
		IdentityGroups: []index.IdentityGroup{
			{IdentityID: 1, UserID: 2, UID: bob.UID, Name: "dev", ExpiresAt: ahead(2 * time.Hour)},
			{IdentityID: 1, UserID: 2, UID: bob.UID, Name: "ops", ExpiresAt: ahead(time.Hour)},
		},
		Bindings: []index.Binding{
			binding(1, "user:"+alice.UID, "admin", "*/*"), binding(2, "group:ops", "writer", "raw/*"),
			binding(3, "user:"+robot.UID, "admin", "raw/app"), binding(4, "k8s:prod/build/runner", "writer", "raw/*"),
			binding(5, "cert:spiffe://corp/ci/*", "reader", "raw/app"), binding(6, "group:nobody", "reader", "raw/app"),
		},
		Tokens: []index.Token{
			{ID: 10, Kind: "pat", UserID: &alice.ID, Name: "laptop", Hash: strings.Repeat("a", 64), ExpiresAt: ahead(30 * day),
				LastUsedAt: ago(200 * day), CreatedAt: *ago(300 * day)},
			{ID: 11, Kind: "pat", UserID: &robot.ID, Name: "ci", Hash: strings.Repeat("b", 64), Scopes: []string{"read", "write"},
				CreatedAt: *ago(5 * day)},
			{ID: 12, Kind: "deploy", RepoID: &app.ID, Name: "node", Hash: strings.Repeat("c", 64), Scopes: []string{"read"},
				CreatedAt: *ago(day)},
			{ID: 13, Kind: "deploy", RepoMask: "*/*", Name: "fleet", Hash: strings.Repeat("d", 64), Scopes: []string{"read"},
				ExpiresAt: ahead(10 * day), LastUsedAt: ago(day), CreatedAt: *ago(2 * day)},
		},
		TokenRepos: []index.Repo{app},
		CIPolicies: []index.CIPolicy{{ID: 1, Name: "app-release", Issuer: "github", Claims: map[string][]string{"repository_id": {"1"}},
			Grants: []index.CIGrant{{Role: "writer", Mask: "raw/app"}}, State: index.CIPolicyActive, ExpiresAt: ahead(5 * day),
			CreatedBy: "user:" + alice.UID, UpdatedBy: "user:" + alice.UID, CreatedAt: *ago(day), UpdatedAt: *ago(day), Revision: 1}},
		AnonymousRepos: []index.Repo{public},
	}
}

// accessConfig is the configuration of the tests: every flag that a setting turns on, and a CI
// trust policy of the configuration that reads */*.
var accessConfig = AccessConfig{AdminMFA: true, AnonymousRead: true, InactiveAfter: 90 * 24 * time.Hour,
	DeployMaxTTL: 30 * 24 * time.Hour, ServiceMaxTTL: 90 * 24 * time.Hour,
	CIPolicies: []config.CIPolicy{{Name: "mirror", Issuer: "gitlab", Claims: map[string]config.CIValues{"project_id": {"9"}},
		Grants: []config.CIGrant{{Role: "reader", Mask: "*/*"}}}}}

// canonicalOf returns doc in the canonical form, decoded and encoded again: a document in it is
// its own canonical form.
func canonicalOf(t *testing.T, doc []byte) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	must(t, dec.Decode(&v))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	must(t, enc.Encode(v))
	return buf.Bytes()
}

// TestAccessReportEmpty: the document of an index with no rows, in its canonical form byte for
// byte: compact, the keys of every object in byte order, generatedAt to the second, the sections
// of the scope present and empty, anonymous absent while auth.anonymousRead is off, and a line
// feed at the end.
func TestAccessReportEmpty(t *testing.T) {
	a := &index.Access{ReadAt: accessAt}
	doc, err := NewAccessReport(a, AccessConfig{}, AccessQuery{Scope: auth.AccessAll, UnusedDays: 90}).JSON()
	must(t, err)
	want := `{"ciPolicies":[],"deployTokens":[],"generatedAt":"2026-11-18T10:00:00Z","groups":[],"scope":"all","settings":` +
		`{"adminMFA":false,"anonymousRead":false,"deployTokensMaxTTL":0,"disableInactiveUsersAfter":0,"serviceAccountsMaxTokenTTL":0},` +
		`"unusedDays":90,"users":[],"workloads":[]}` + "\n"
	if string(doc) != want {
		t.Errorf("the empty document\n%s\nwant\n%s", doc, want)
	}
}

// TestAccessReportDocument: the sections of accessRows, each entry in the shape the API answers
// it with its principal and its flags, in their fixed orders, every time to the second; the
// document is its own canonical form, and the same rows at the same time are the same bytes.
func TestAccessReportDocument(t *testing.T) {
	q := AccessQuery{Scope: auth.AccessAll, UnusedDays: 90}
	doc, err := NewAccessReport(accessRows(), accessConfig, q).JSON()
	must(t, err)
	again, err := NewAccessReport(accessRows(), accessConfig, q).JSON()
	must(t, err)
	if !bytes.Equal(doc, again) {
		t.Errorf("the same rows at the same time, two documents:\n%s\n%s", doc, again)
	}
	if !bytes.Equal(doc, canonicalOf(t, doc)) {
		t.Errorf("the document is not in its canonical form:\n%s", doc)
	}
	if frac := regexp.MustCompile(`T\d\d:\d\d:\d\d\.`).Find(doc); frac != nil {
		t.Errorf("a time with a fraction of a second, %s, in\n%s", frac, doc)
	}

	var rep AccessReport
	must(t, json.Unmarshal(doc, &rep))
	wantJSON(t, "zed, a disabled user with nothing", rep.Users[3], `{"uid": "`+accessUID(4)+`", "name": "zed", "kind": "local",
		"disabled": true, "createdAt": "2026-01-22T10:00:00Z", "lastSeenAt": null, "principal": "user:`+accessUID(4)+`",
		"groups": [], "bindings": [], "tokens": [], "flags": ["unused"]}`)
	wantJSON(t, "bob's groups", rep.Users[1].Groups, `[{"name": "dev", "expiresAt": "2026-11-18T12:00:00Z"},
		{"name": "ops", "expiresAt": "2026-11-18T11:00:00Z"}]`)
	wantJSON(t, "bob's bindings, through ops", rep.Users[1].Bindings, `[{"id": 2, "principal": "group:ops", "role": "writer",
		"repoMask": "raw/*", "createdAt": "2026-11-08T10:00:00Z", "via": "group:ops"}]`)
	wantJSON(t, "alice's token", rep.Users[0].Tokens, `[{"id": 10, "kind": "pat", "user": "alice", "name": "laptop", "scopes": [],
		"expiresAt": "2026-12-18T10:00:00Z", "lastUsedAt": "2026-05-02T10:00:00Z", "createdAt": "2026-01-22T10:00:00Z",
		"revokedAt": null, "hashPrefix": "aaaaaaaa", "flags": ["unused"]}]`)
	wantJSON(t, "robot", struct {
		Owner, OwnerName string
		Flags            []string
		TokenFlags       []string
	}{rep.Users[2].Owner, rep.Users[2].OwnerName, rep.Users[2].Flags, rep.Users[2].Tokens[0].Flags},
		`{"Owner": "user:`+accessUID(1)+`", "OwnerName": "alice", "Flags": ["admin", "adminWithoutEffect"],
		"TokenFlags": ["noExpiry", "beyondMaxTTL"]}`)
	for _, tt := range []struct {
		what string
		got  any
		want string
	}{
		{"the users, by name", mapped(rep.Users, func(u AccessUser) string { return u.Name }), `["alice", "bob", "robot", "zed"]`},
		{"the flags of the users", [][]string{rep.Users[0].Flags, rep.Users[1].Flags},
			`[["admin", "allRepos", "adminWithoutEffect"], ["unused", "inactive"]]`},
		{"the groups with a binding", *rep.Groups, `[{"principal": "group:nobody", "bindings": [{"id": 6, "principal": "group:nobody",
			"role": "reader", "repoMask": "raw/app", "createdAt": "2026-11-08T10:00:00Z"}], "members": [], "flags": []},
			{"principal": "group:ops", "bindings": [{"id": 2, "principal": "group:ops", "role": "writer", "repoMask": "raw/*",
			"createdAt": "2026-11-08T10:00:00Z"}], "members": [{"principal": "user:` + accessUID(2) + `", "name": "bob",
			"expiresAt": "2026-11-18T11:00:00Z"}], "flags": []}]`},
		{"the workloads, by principal", mapped(*rep.Workloads, func(w AccessWorkload) string {
			return w.Principal + " " + strings.Join(w.Flags, ",") + map[bool]string{true: " pattern"}[w.Pattern]
		}), `["cert:spiffe://corp/ci/*  pattern", "k8s:prod/build/runner "]`},
		{"the deploy tokens", mapped(*rep.DeployTokens, func(d AccessToken) string {
			return d.Principal + " " + d.Repo + d.RepoMask + " " + strings.Join(d.Flags, ",")
		}), `["token:12 raw/app noExpiry,beyondMaxTTL", "token:13 */* allRepos"]`},
		{"the CI trust policies, by name", mapped(*rep.CIPolicies, func(p AccessCIPolicy) string {
			return p.Name + " " + p.Source + " " + strings.Join(p.Flags, ",")
		}), `["app-release api ", "mirror config allRepos,noExpiry"]`},
		{"anonymous", rep.Anonymous, `{"repos": ["raw/public"]}`},
		{"the settings", rep.Settings, `{"adminMFA": true, "anonymousRead": true, "disableInactiveUsersAfter": 7776000,
			"deployTokensMaxTTL": 2592000, "serviceAccountsMaxTokenTTL": 7776000}`},
	} {
		wantJSON(t, tt.what, tt.got, tt.want)
	}
}

// mapped returns f of each of list.
func mapped[T any](list []T, f func(T) string) []string {
	out := []string{}
	for _, v := range list {
		out = append(out, f(v))
	}
	return out
}

// TestAccessReportFlags: unused counts unusedDays before generatedAt, from the last use or the
// creation; adminWithoutEffect only under the admin mode; inactive and ownerDisabled as the users
// API answers them; beyondMaxTTL by the bound of the configuration; the admin mode off, an admin
// binding has its effect.
func TestAccessReportFlags(t *testing.T) {
	a := accessRows()
	a.Users[0].Disabled = true // alice: robot's owner disabled
	a.Users[1].LastSeenAt = nil
	rep := NewAccessReport(a, AccessConfig{InactiveAfter: 24 * time.Hour}, AccessQuery{Scope: auth.AccessAll, UnusedDays: 250})
	wantJSON(t, "the flags of the users at 250 days, without the admin mode", [][]string{rep.Users[0].Flags, rep.Users[1].Flags,
		rep.Users[2].Flags, rep.Users[3].Flags, rep.Users[0].Tokens[0].Flags, rep.Users[2].Tokens[0].Flags},
		`[["admin", "allRepos"], ["unused", "inactive"], ["admin", "ownerDisabled"], ["unused"], [], ["noExpiry"]]`)
}

// TestAccessReportScopes: people keeps the users of kind local and oidc and the groups; systems
// the service accounts, the workloads, the deploy tokens and the CI trust policies; all keeps
// everything; anonymous is in every scope.
func TestAccessReportScopes(t *testing.T) {
	for _, tt := range []struct {
		scope, users, sections string
	}{
		{auth.AccessAll, "alice bob robot zed", "anonymous ciPolicies deployTokens generatedAt groups scope settings unusedDays users workloads"},
		{auth.AccessPeople, "alice bob zed", "anonymous generatedAt groups scope settings unusedDays users"},
		{auth.AccessSystems, "robot", "anonymous ciPolicies deployTokens generatedAt scope settings unusedDays users workloads"},
	} {
		rep := NewAccessReport(accessRows(), accessConfig, AccessQuery{Scope: tt.scope, UnusedDays: 90})
		doc, err := rep.JSON()
		must(t, err)
		var top map[string]json.RawMessage
		must(t, json.Unmarshal(doc, &top))
		keys := mapped(sortedKeys(top), func(k string) string { return k })
		if got := strings.Join(keys, " "); got != tt.sections {
			t.Errorf("scope %s: the sections %s, want %s", tt.scope, got, tt.sections)
		}
		if got := strings.Join(mapped(rep.Users, func(u AccessUser) string { return u.Name }), " "); got != tt.users {
			t.Errorf("scope %s: the users %s, want %s", tt.scope, got, tt.users)
		}
	}
}

// sortedKeys returns the keys of m in byte order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// accessFixture returns a fixture over an index with the rows of every kind the report reads, made
// through the services with real secrets, with the configuration accessConfig: the local admin
// root with a password and a personal access token, a deploy token of raw/app and one over */*, a
// webhook whose URL has a path and a query, and the user's hashes; and the secrets, the hashes
// and the parts of the URL that no report may hold.
func accessFixture(t *testing.T, o Options) (*fixture, []string) {
	t.Helper()
	o.Access = accessConfig
	f := newFixture(t, o)
	ctx := context.Background()
	f.create(t, "raw", "app")
	const password = "Corr3ct-Horse-Battery-Staple"
	users := &auth.Users{Index: f.st}
	_, err := users.Create(ctx, "root", password, true, auth.Origin{})
	must(t, err)
	must(t, users.SetPassword(ctx, "root", password+"-2", auth.Origin{}))
	tokens := &auth.Tokens{Index: f.st}
	_, pat, err := tokens.CreatePAT(ctx, "root", "laptop", nil, auth.Expiry{}, auth.Origin{})
	must(t, err)
	_, deploy, err := tokens.CreateDeploy(ctx, "raw/app", "node", []string{"read"}, auth.Expiry{}, auth.Origin{})
	must(t, err)
	_, mask, err := tokens.CreateMaskDeploy(ctx, "*/*", "fleet", []string{"read"}, auth.Expiry{}, auth.Origin{})
	must(t, err)
	must(t, f.st.CreateWebhook(ctx, &index.Webhook{UID: index.NewUID(time.Now()), Mask: "*/*", Active: true, CreatedBy: "user:x",
		URL: "https://hooks.example.com/hook-path-x7/deliver?key=query-secret-q9", SecretEnc: []byte("webhook-secret-w3")}))
	row, err := f.st.GetUser(ctx, "root")
	must(t, err)
	secrets := []string{password, password + "-2", row.PasswordHash, "$argon2id$", "hook-path-x7", "query-secret-q9", "webhook-secret-w3"}
	secrets = append(secrets, row.PasswordHistory...)
	for _, s := range []string{pat, deploy, mask} {
		secrets = append(secrets, s, auth.HashSecret(s), auth.HashSecret(s)[auth.HashPrefixLen:])
	}
	return f, secrets
}

// TestAccessReportRoute: GET /api/v1/access-report answers the document of the index's rows at
// the database's time, as it is built, with the default scope and unusedDays; it holds none of
// the secrets, hashes and webhook URLs of the index, but each token's hashPrefix; and it writes
// no audit row.
func TestAccessReportRoute(t *testing.T) {
	f, secrets := accessFixture(t, Options{})
	before := len(auditOf(t, f.st, ""))
	rr := f.do(http.MethodGet, "/api/v1/access-report", "", admin)
	wantStatus(t, rr, http.StatusOK)
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	body := rr.Body.Bytes()
	if !bytes.Equal(body, canonicalOf(t, body)) {
		t.Errorf("the answer is not in the canonical form:\n%s", body)
	}
	for _, s := range secrets {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("the report holds %q:\n%s", s, body)
		}
	}
	var rep AccessReport
	must(t, json.Unmarshal(body, &rep))
	if rep.Scope != auth.AccessAll || rep.UnusedDays != DefaultUnusedDays || time.Since(rep.GeneratedAt) > time.Minute {
		t.Errorf("scope %s, unusedDays %d, generatedAt %s: want all, 90 and the database's time now", rep.Scope, rep.UnusedDays,
			rep.GeneratedAt)
	}
	if len(rep.Users) != 1 || len(rep.Users[0].Tokens) != 1 || len(rep.Users[0].Tokens[0].HashPrefix) != auth.HashPrefixLen ||
		len(*rep.DeployTokens) != 2 || rep.Users[0].Flags[0] != FlagAdmin {
		t.Errorf("the report of root, its token and two deploy tokens:\n%s", body)
	}
	if after := len(auditOf(t, f.st, "")); after != before {
		t.Errorf("the report wrote %d audit rows", after-before)
	}
}

// TestAccessReportQuery: ?scope= and ?unusedDays= out of their bounds, or given twice, are 400
// naming them, and the bounds themselves are taken.
func TestAccessReportQuery(t *testing.T) {
	f, _ := accessFixture(t, Options{})
	for _, tt := range []struct {
		query, msg string
	}{
		{"scope=everyone", `scope: "everyone" is not all, people or systems`},
		{"scope=", `scope: "" is not all, people or systems`},
		{"scope=all&scope=people", "scope: once in the query"},
		{"unusedDays=0", "unusedDays: 0 is not a number of days from 1 to 3650"},
		{"unusedDays=3651", "unusedDays: 3651 is not a number of days from 1 to 3650"},
		{"unusedDays=90d", `unusedDays: "90d" is not a number of days from 1 to 3650`},
		{"unusedDays=1&unusedDays=2", "unusedDays: once in the query"},
	} {
		wantError(t, f.do(http.MethodGet, "/api/v1/access-report?"+tt.query, "", admin), http.StatusBadRequest, codeInvalid, tt.msg)
	}
	for _, query := range []string{"scope=people&unusedDays=1", "scope=systems&unusedDays=3650"} {
		wantStatus(t, f.do(http.MethodGet, "/api/v1/access-report?"+query, "", admin), http.StatusOK)
	}
}

// TestAccessReportAdminOnly: only the admin role reads the report, checked before the query: a
// user without it, a maintainer on */* among them, is 403, whatever its query; under the admin
// mode an admin binding without its effect is 403 for a password and 401 for a sign-in without a
// second factor, and a sign-in with one reads it.
func TestAccessReportAdminOnly(t *testing.T) {
	f := rbacFixture(t, [3]string{"root", auth.RoleAdmin, "*/*"}, [3]string{"ops", auth.RoleMaintainer, "*/*"},
		[3]string{"alice", auth.RoleReader, "raw/*"})
	for _, p := range []string{"ops", "alice"} {
		wantError(t, f.do(http.MethodGet, "/api/v1/access-report?scope=bad", "", auth.Principal{Kind: auth.KindUser, Name: p}),
			http.StatusForbidden, codeForbidden)
	}
	wantStatus(t, f.do(http.MethodGet, "/api/v1/access-report", "", auth.Principal{Kind: auth.KindUser, Name: "root"}), http.StatusOK)

	m := adminModeFixture(t)
	root := auth.Principal{Kind: auth.KindUser, Name: "root"}
	wantError(t, m.doAs(http.MethodGet, "/api/v1/access-report", "", root, auth.Credential{Kind: auth.CredPassword}),
		http.StatusForbidden, codeForbidden, "auth.admin.requireMFA")
	oneFactor := auth.Credential{Kind: auth.CredPAT, TokenID: 6, SignIn: &auth.SignIn{At: time.Now(), AMR: []string{"pwd"}}}
	wantError(t, m.doAs(http.MethodGet, "/api/v1/access-report", "", root, oneFactor), http.StatusUnauthorized, codeUnauthenticated)
	mfa := auth.Credential{Kind: auth.CredPAT, TokenID: 5, SignIn: &auth.SignIn{At: time.Now(), AMR: []string{"pwd", "otp"}}}
	wantStatus(t, m.doAs(http.MethodGet, "/api/v1/access-report", "", root, mfa), http.StatusOK)
}
