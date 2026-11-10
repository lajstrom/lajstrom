package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// dave maintains the group npm/all: manage on it, not the admin role.
var dave = auth.Principal{Kind: auth.KindUser, Name: "dave"}

// newGroupFixture is newProxyFixture with group repos of npm, the authorization of the tests
// and dave's grants, the hosted npm/internal and npm/private and the proxies npm/npmjs and
// npm/mirror created.
func newGroupFixture(t *testing.T) *fixture {
	t.Helper()
	g := map[string]bool{"user:dave manage npm/all": true, "user:dave read npm/all": true}
	for k, v := range grants {
		g[k] = v
	}
	f := newFixture(t, Options{Auth: fakeAuth{grants: g}})
	withProxies(f)
	f.repos.Groupers = oneOf("npm")
	for _, body := range []string{
		`{"format": "npm", "name": "internal", "registry": "local"}`,
		`{"format": "npm", "name": "private", "registry": "local"}`,
		`{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs"}}`,
		`{"format": "npm", "name": "mirror", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs2"}}`,
	} {
		wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	}
	return f
}

// TestCreateGroup: a group is created with its members in order, its resource has its
// settings and neither a registry nor a policy, and the members are rows of the index
// (docs/spec/proxy-and-group.md §2.3, §2.4).
func TestCreateGroup(t *testing.T) {
	f := newGroupFixture(t)
	body := `{"format": "npm", "name": "all", "kind": "group", "anonymousRead": true, "group": {"members": ["internal", "npmjs"]}}`
	var got map[string]any
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated, &got)
	want := `{"format": "npm", "name": "all", "kind": "group", "anonymousRead": true,
		"group": {"members": ["internal", "npmjs"], "merge": []},
		"createdAt": "2026-01-02T12:00:00Z", "updatedAt": "2026-01-02T12:00:00Z"}`
	wantJSON(t, "the created group", got, want)
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/npm/all", "", admin), http.StatusOK, &got)
	wantJSON(t, "the group read back", got, want)
	ev := auditOf(t, f.st, "repo.create")[0] // the newest
	if g, _ := ev.details["repo"].(map[string]any)["group"].(map[string]any); fmt.Sprint(g["members"]) != "[internal npmjs]" {
		t.Errorf("audit details %v, want the members in the resource", ev.details)
	}
	row, err := f.st.GetRepo(context.Background(), "npm", "all")
	must(t, err)
	members, err := f.st.ListGroupMembers(context.Background(), row.ID)
	must(t, err)
	if len(members) != 2 || members[0].Name != "internal" || members[1].Name != "npmjs" || row.Registry != "" || row.Policy != nil {
		t.Errorf("the group in the index: %+v, members %+v", row, members)
	}
	var list RepoList
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos?format=npm", "", admin), http.StatusOK, &list)
	if len(list.Repos) != 5 || list.Repos[0].Name != "all" || string(list.Repos[0].Group) == "" {
		t.Errorf("the list: %+v", list.Repos)
	}
}

func TestCreateGroupInvalid(t *testing.T) {
	const group = `{"format": "npm", "name": "all", "kind": "group"`
	many := `"m0"`
	for i := 1; i < 17; i++ {
		many += fmt.Sprintf(`, "m%d"`, i)
	}
	tests := []struct{ name, body, want string }{
		{"no group", group + `}`, "group: a group repo must name its members"},
		{"no members", group + `, "group": {"merge": []}}`, "group.members: must name 1 to 16 repos"},
		{"17 members", group + `, "group": {"members": [` + many + `]}}`, "group.members: has 17 repos, at most 16"},
		{"a member twice", group + `, "group": {"members": ["npmjs", "npmjs"]}}`, `group.members[1]: "npmjs" is already group.members[0]`},
		{"a member that does not exist", group + `, "group": {"members": ["internal", "verdaccio"]}}`,
			`group.members[1]: "verdaccio" is not a repo of format npm`},
		{"a member of another format", group + `, "group": {"members": ["scratch"]}}`, `group.members[0]: "scratch" is not a repo of format npm`},
		{"a group as a member", group + `, "group": {"members": ["other"]}}`, `group.members[0]: "other" is a group`},
		{"two hosted members", group + `, "group": {"members": ["internal", "npmjs", "private"]}}`,
			`group.members[2]: "private" is a second hosted member, after group.members[0]`},
		{"a registry", group + `, "registry": "local", "group": {"members": ["internal"]}}`, "registry: a group has none"},
		{"a policy", group + `, "policy": {"maxFileSize": 1}, "group": {"members": ["internal"]}}`, "policy: a group has none"},
		{"an upstream", group + `, "upstream": {"name": "npmjs"}, "group": {"members": ["internal"]}}`,
			"upstream: only a proxy repo has one, not a group repo"},
		{"an unknown key", group + `, "group": {"members": ["internal"], "order": "first"}}`, `group: "order" is not a key`},
		{"a merge pattern with a space", group + `, "group": {"members": ["internal"], "merge": ["@a b/*"]}}`, "group.merge[0]: "},
		{"a group on a hosted repo", `{"format": "npm", "name": "x", "registry": "local", "group": {"members": ["internal"]}}`,
			"group: only a group repo has one, not a hosted repo"},
		{"a format without group repos", `{"format": "pypi", "name": "all", "kind": "group", "group": {"members": ["a"]}}`,
			"kind: format pypi has no group repos"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGroupFixture(t)
			f.create(t, "raw", "scratch")
			wantStatus(t, f.do(http.MethodPost, "/api/v1/repos",
				`{"format": "npm", "name": "other", "kind": "group", "group": {"members": ["mirror"]}}`, admin), http.StatusCreated)
			wantError(t, f.do(http.MethodPost, "/api/v1/repos", tt.body, admin), http.StatusBadRequest, codeInvalid, tt.want)
		})
	}
}

// TestPatchGroup: manage changes a group's merge patterns; only the admin role changes its
// members, which are replaced whole and checked as on a create; a group takes no policy, and
// updatedAt moves with the members (docs/spec/proxy-and-group.md §2.3, rbac.md §7).
func TestPatchGroup(t *testing.T) {
	f := newGroupFixture(t)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos",
		`{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["internal", "npmjs"]}}`, admin), http.StatusCreated)
	var got Repo
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/npm/all", `{"group": {"merge": ["@types/*"]}}`, dave), http.StatusOK, &got)
	wantJSON(t, "group after dave's PATCH", got.Group, `{"members": ["internal", "npmjs"], "merge": ["@types/*"]}`)
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/all", `{"group": {"members": ["npmjs"]}}`, dave),
		http.StatusForbidden, codeForbidden, "group.members: only the admin role")
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/all", `{"group": {"members": ["internal", "private"]}}`, admin),
		http.StatusBadRequest, codeInvalid, `group.members[1]: "private" is a second hosted member`)
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/all", `{"policy": {"maxFileSize": 1}}`, admin),
		http.StatusBadRequest, codeInvalid, "policy: a group has none")
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/all", `{"group": null}`, admin),
		http.StatusBadRequest, codeInvalid, "group: a group repo must have one")
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/npm/internal", `{"group": {"merge": []}}`, admin),
		http.StatusBadRequest, codeInvalid, "group: only a group repo has one, not a hosted repo")

	before := got.UpdatedAt
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/npm/all", `{"group": {"members": ["mirror", "npmjs", "private"]}}`, admin),
		http.StatusOK, &got)
	wantJSON(t, "group after the admin's PATCH", got.Group, `{"members": ["mirror", "npmjs", "private"], "merge": ["@types/*"]}`)
	if !got.UpdatedAt.After(before) {
		t.Errorf("updatedAt %v did not move with the members", got.UpdatedAt)
	}
	ev := auditOf(t, f.st, "repo.update")
	if len(ev) != 2 {
		t.Fatalf("%d repo.update events, want 2", len(ev))
	}
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/npm/all", `{"group": {"members": ["mirror", "npmjs", "private"]}}`, admin),
		http.StatusOK, &got)
	if n := len(auditOf(t, f.st, "repo.update")); n != 2 {
		t.Errorf("a PATCH that changes nothing wrote an event: %d", n)
	}
}

// TestDeleteMember: a repo that is a member of a group is not deleted, force or not, and the
// 409 names the groups; once the group is gone it is (docs/spec/proxy-and-group.md §2.5).
func TestDeleteMember(t *testing.T) {
	f := newGroupFixture(t)
	for _, body := range []string{
		`{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["internal", "npmjs"]}}`,
		`{"format": "npm", "name": "build", "kind": "group", "group": {"members": ["npmjs"]}}`,
	} {
		wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	}
	for _, target := range []string{"/api/v1/repos/npm/npmjs", "/api/v1/repos/npm/npmjs?force=true"} {
		wantError(t, f.do(http.MethodDelete, target, "", admin), http.StatusConflict, codeConflict,
			"repo npm/npmjs is a member of the group(s) npm/all, npm/build")
	}
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/npm/all", "", admin), http.StatusNoContent)
	wantStatus(t, f.do(http.MethodDelete, "/api/v1/repos/npm/internal", "", admin), http.StatusNoContent)
	wantError(t, f.do(http.MethodDelete, "/api/v1/repos/npm/npmjs", "", admin), http.StatusConflict, codeConflict, "npm/build")
}

// TestBootstrapGroup: the bootstrap creates a configured group after the repos it lists,
// wherever the configuration lists it, and a group whose settings differ is a warning.
func TestBootstrapGroup(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	cfg := []config.Repo{
		{Name: "all", Format: "npm", Kind: config.KindGroup, Group: map[string]any{"members": []any{"team", "npmjs"}}},
		{Name: "team", Format: "npm", Registry: "local"},
	}
	if created, err := f.repos.Bootstrap(ctx, cfg); err != nil || created != 2 {
		t.Fatalf("Bootstrap = %d, %v; want 2 created", created, err)
	}
	r, err := f.repos.Get(ctx, "npm", "all")
	must(t, err)
	wantJSON(t, "the bootstrapped group", r.Group, `{"members": ["team", "npmjs"], "merge": []}`)
	f.logs.Reset()
	if _, err := f.repos.Bootstrap(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.logs.String(), "differs") {
		t.Errorf("a warning for the same settings:\n%s", f.logs)
	}
	cfg[0].Group = map[string]any{"members": []any{"npmjs"}, "merge": []any{"@types/*"}}
	if _, err := f.repos.Bootstrap(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	wantLogs(t, f.logs, `"repo":"npm/all","field":"group"`)

	bad := []config.Repo{{Name: "two", Format: "npm", Kind: config.KindGroup, Group: map[string]any{"members": []any{"internal", "private"}}}}
	if _, err := f.repos.Bootstrap(ctx, bad); err == nil || !strings.Contains(err.Error(), `repos[0]: group.members[1]: "private" is a second hosted member`) {
		t.Errorf("Bootstrap of a bad group = %v", err)
	}
}

// TestGroupResourceKeys: the JSON of a group has no registry and no policy key; a hosted repo's
// still has both.
func TestGroupResourceKeys(t *testing.T) {
	for _, tt := range []struct {
		r    Repo
		keys string
	}{
		{Repo{Format: "npm", Name: "all", Kind: index.KindGroup, Group: json.RawMessage(`{"members":["a"],"merge":[]}`)},
			"anonymousRead createdAt format group kind name updatedAt"},
		{Repo{Format: "npm", Name: "a", Kind: index.KindHosted, Registry: "local"},
			"anonymousRead createdAt format kind name policy registry updatedAt"},
	} {
		b, err := json.Marshal(tt.r)
		must(t, err)
		var m map[string]any
		must(t, json.Unmarshal(b, &m))
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if got := strings.Join(keys, " "); got != tt.keys {
			t.Errorf("keys of a %s repo: %s, want %s", tt.r.Kind, got, tt.keys)
		}
	}
}
