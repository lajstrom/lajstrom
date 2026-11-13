package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
)

// newOCIGroupFixture is newGroupFixture with the hosted oci repos internal and team and the oci
// proxies hub and mirror; dave manages oci/all.
func newOCIGroupFixture(t *testing.T) *fixture {
	t.Helper()
	g := map[string]bool{"user:dave manage oci/all": true, "user:dave read oci/all": true}
	for k, v := range grants {
		g[k] = v
	}
	f := newFixture(t, Options{Auth: fakeAuth{grants: g}})
	withProxies(f)
	f.repos.Groupers = oneOf("npm")
	for _, body := range []string{
		`{"format": "oci", "name": "internal", "registry": "local"}`,
		`{"format": "oci", "name": "team", "registry": "local"}`,
		`{"format": "oci", "name": "hub", "registry": "local", "kind": "proxy", "upstream": {"name": "dockerhub"}}`,
		`{"format": "oci", "name": "mirror", "registry": "local", "kind": "proxy", "upstream": {"name": "dockerhub"}}`,
	} {
		wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated)
	}
	return f
}

// TestCreateOCIGroup: a group of oci repos needs no plugin, may have several hosted members,
// and its resource shows its members and routes, without merge; repos.group keeps the routes
// (docs/spec/proxy-and-group.md §12.1).
func TestCreateOCIGroup(t *testing.T) {
	f := newOCIGroupFixture(t)
	body := `{"format": "oci", "name": "all", "kind": "group",
		"group": {"members": ["internal", "team", "hub"], "paths": {"internal": ["acme/*"], "team": ["team/*", "acme/team/*"], "hub": ["*"]}}}`
	var got map[string]any
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos", body, admin), http.StatusCreated, &got)
	want := `{"format": "oci", "name": "all", "kind": "group", "anonymousRead": false,
		"group": {"members": ["internal", "team", "hub"], "paths": {"hub": ["*"], "internal": ["acme/*"], "team": ["team/*", "acme/team/*"]}},
		"createdAt": "2026-01-02T12:00:00Z", "updatedAt": "2026-01-02T12:00:00Z"}`
	wantJSON(t, "the created group", got, want)
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/oci/all", "", admin), http.StatusOK, &got)
	wantJSON(t, "the group read back", got, want)
	row, err := f.st.GetRepo(context.Background(), "oci", "all")
	must(t, err)
	if string(row.Group) != `{"paths":{"hub":["*"],"internal":["acme/*"],"team":["team/*","acme/team/*"]}}` {
		t.Errorf("repos.group = %s", row.Group)
	}
}

func TestCreateOCIGroupInvalid(t *testing.T) {
	const group = `{"format": "oci", "name": "all", "kind": "group"`
	for _, tt := range []struct{ name, body, want string }{
		{"the same pattern on two members", group + `, "group": {"members": ["internal", "hub", "mirror"], "paths": {"internal": ["acme/*"], "hub": ["*"], "mirror": ["*"]}}}`,
			`group.paths.mirror[0]: "*" is also group.paths.hub[0]`},
		{"merge", group + `, "group": {"members": ["internal"], "merge": ["*"], "paths": {"internal": ["*"]}}}`, "group.merge: "},
		{"no routes", group + `, "group": {"members": ["internal"]}}`, "group.paths: must give each member"},
		{"a member without routes", group + `, "group": {"members": ["internal", "hub"], "paths": {"internal": ["*"]}}}`,
			"group.paths.hub: must name at least one pattern"},
		{"not a route", group + `, "group": {"members": ["internal"], "paths": {"internal": ["acme*"]}}}`, `group.paths.internal[0]: "acme*" is not a route`},
		{"a member of another format", group + `, "group": {"members": ["scratch"], "paths": {"scratch": ["*"]}}}`,
			`group.members[0]: "scratch" is not a repo of format oci`},
		{"a registry", group + `, "registry": "local", "group": {"members": ["internal"], "paths": {"internal": ["*"]}}}`, "registry: a group has none"},
		{"paths on an npm group", `{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["x"], "paths": {"x": ["*"]}}}`,
			"group.paths: only a group of oci repos has routes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newOCIGroupFixture(t)
			f.create(t, "raw", "scratch")
			wantError(t, f.do(http.MethodPost, "/api/v1/repos", tt.body, admin), http.StatusBadRequest, codeInvalid, tt.want)
		})
	}
}

// TestPatchOCIGroup: only the admin role changes the routes, as the members; paths merge as
// RFC 7396, each list replaced whole and a null key removed; the result is checked whole, so
// members and the routes that go with them are sent together (§12.1, rbac.md §7).
func TestPatchOCIGroup(t *testing.T) {
	f := newOCIGroupFixture(t)
	wantStatus(t, f.do(http.MethodPost, "/api/v1/repos", `{"format": "oci", "name": "all", "kind": "group",
		"group": {"members": ["internal", "hub"], "paths": {"internal": ["acme/*"], "hub": ["*"]}}}`, admin), http.StatusCreated)
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all", `{"group": {"paths": {"internal": ["acme/*", "acme"]}}}`, dave),
		http.StatusForbidden, codeForbidden, "group.paths: only the admin role")
	var got Repo
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all", `{"anonymousRead": true, "group": {"members": ["internal", "hub"], "paths": {"hub": ["*"], "internal": ["acme/*"]}}}`, dave),
		http.StatusOK, &got)
	if !got.AnonymousRead {
		t.Errorf("dave's PATCH that sends the group back: %+v", got)
	}
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all", `{"group": {"paths": {"internal": ["acme/*", "acme"]}}}`, admin), http.StatusOK, &got)
	wantJSON(t, "the routes after the admin's PATCH", got.Group, `{"members": ["internal", "hub"], "paths": {"hub": ["*"], "internal": ["acme/*", "acme"]}}`)

	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all", `{"group": {"members": ["internal", "hub", "team"]}}`, admin),
		http.StatusBadRequest, codeInvalid, "group.paths.team: must name at least one pattern")
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all", `{"group": {"members": ["internal"]}}`, admin),
		http.StatusBadRequest, codeInvalid, `group.paths.hub: "hub" is not one of group.members`)
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all", `{"group": {"paths": {"internal": ["*"]}}}`, admin),
		http.StatusBadRequest, codeInvalid, `"*" is also group.paths.`)
	decodeJSON(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all",
		`{"group": {"members": ["internal", "team"], "paths": {"hub": null, "team": ["*"]}}}`, admin), http.StatusOK, &got)
	wantJSON(t, "two hosted members", got.Group, `{"members": ["internal", "team"], "paths": {"internal": ["acme/*", "acme"], "team": ["*"]}}`)
	wantError(t, f.do(http.MethodPatch, "/api/v1/repos/oci/all", `{"group": {"merge": []}}`, admin),
		http.StatusBadRequest, codeInvalid, "group.merge: ")
}

// TestBootstrapOCIGroup: the bootstrap creates a configured group of oci repos, and one whose
// routes overlap stops it naming the entry (§12.1).
func TestBootstrapOCIGroup(t *testing.T) {
	f := newOCIGroupFixture(t)
	ctx := context.Background()
	routes := map[string]any{"internal": []any{"acme/*"}, "hub": []any{"*"}}
	cfg := []config.Repo{{Name: "all", Format: "oci", Kind: config.KindGroup,
		Group: map[string]any{"members": []any{"internal", "hub"}, "paths": routes}}}
	if created, err := f.repos.Bootstrap(ctx, cfg); err != nil || created != 1 {
		t.Fatalf("Bootstrap = %d, %v; want 1 created", created, err)
	}
	f.logs.Reset()
	if _, err := f.repos.Bootstrap(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.logs.String(), "differs") {
		t.Errorf("a warning for the same settings:\n%s", f.logs)
	}
	bad := []config.Repo{{Name: "two", Format: "oci", Kind: config.KindGroup, Group: map[string]any{"members": []any{"internal", "hub"},
		"paths": map[string]any{"internal": []any{"*"}, "hub": []any{"*"}}}}}
	if _, err := f.repos.Bootstrap(ctx, bad); err == nil || !strings.Contains(err.Error(), `repos[0]: group.paths.hub[0]: "*" is also group.paths.internal[0]`) {
		t.Errorf("Bootstrap of overlapping routes = %v", err)
	}
}
