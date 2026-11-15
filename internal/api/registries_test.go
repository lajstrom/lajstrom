package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// fakeRegistries answers its registries as they are.
type fakeRegistries []Registry

func (f fakeRegistries) Registries(context.Context) (string, []Registry) {
	return "lajstrom-a 7", append([]Registry(nil), f...)
}

// TestRegistries: GET /api/v1/registries answers the reporter's registries by name, each with
// the repos on it by format and name, a group on none; and none without a reporter.
func TestRegistries(t *testing.T) {
	f := rbacFixture(t, [3]string{"root", auth.RoleAdmin, "*/*"})
	f.create(t, "raw", "scratch")
	f.create(t, "pypi", "libs")
	f.create(t, "raw", "assets")
	if _, err := f.repos.Create(context.Background(), CreateRequest{Format: "raw", Name: "elsewhere", Registry: "other"}, Origin{}); err != nil {
		t.Fatal(err)
	}
	caps := CapabilitiesOf(provider.Capabilities{Referrers: true, MaxNameLength: 246, Consistency: provider.ConsistencyStrong})
	f.h = recordStatuses(New(Options{Repos: f.repos, Log: f.repos.Log, Auth: auth.NewRBAC(auth.RBACOptions{Index: f.st, Log: f.repos.Log}),
		Registries: fakeRegistries{
			{Name: "other", Type: "generic", URL: "https://registry.example", Status: RegistryStatus{State: RegistryDown, Breaker: provider.BreakerOpen}},
			{Name: "local", Type: "generic", URL: "http://zot:5000", Prefix: "lajstrom/", Capabilities: caps,
				Status: RegistryStatus{State: RegistryOK, Breaker: provider.BreakerClosed}},
			{Name: "unused", Type: "ecr", URL: "https://1.dkr.ecr.eu-west-1.amazonaws.com", Status: RegistryStatus{State: RegistryOK, Breaker: provider.BreakerClosed}},
		}}))
	root := auth.Principal{Kind: auth.KindUser, Name: "root"}
	var got map[string]any
	decodeJSON(t, f.doAs(http.MethodGet, "/api/v1/registries", "", root, rootPassword), http.StatusOK, &got)
	wantJSON(t, "the registries", got, `{"replica": "lajstrom-a 7", "registries": [
		{"name": "local", "type": "generic", "url": "http://zot:5000", "prefix": "lajstrom/",
		 "capabilities": {"createOnPush": false, "referrers": true, "catalog": false, "deleteManifest": false, "immutableTags": false,
			"blobRedirect": false, "referrersAlwaysTag": false, "maxBlobSize": 0, "consistency": "strong", "consistencyBound": "0s",
			"maxNameDepth": 0, "maxNameLength": 246, "tagsFoldCase": false, "buildRoutes": false},
		 "repos": ["pypi/libs", "raw/assets", "raw/scratch"],
		 "status": {"state": "ok", "checkedAt": null, "answeredAt": null, "latencyMs": null, "message": null, "breaker": "closed"}},
		{"name": "other", "type": "generic", "url": "https://registry.example", "prefix": "", "capabilities": null,
		 "repos": ["raw/elsewhere"],
		 "status": {"state": "down", "checkedAt": null, "answeredAt": null, "latencyMs": null, "message": null, "breaker": "open"}},
		{"name": "unused", "type": "ecr", "url": "https://1.dkr.ecr.eu-west-1.amazonaws.com", "prefix": "", "capabilities": null,
		 "repos": [],
		 "status": {"state": "ok", "checkedAt": null, "answeredAt": null, "latencyMs": null, "message": null, "breaker": "closed"}}]}`)

	bare := newFixture(t, Options{})
	decodeJSON(t, bare.do(http.MethodGet, "/api/v1/registries", "", admin), http.StatusOK, &got)
	wantJSON(t, "the registries without a reporter", got, `{"replica": "", "registries": []}`)
}

// TestRegistriesAuthorization: the registries are the admin's alone, with any credential that
// reaches the role; a maintainer on */*, a token with scopes and a deploy token are 403,
// anonymous 401.
func TestRegistriesAuthorization(t *testing.T) {
	f := rbacFixture(t, [3]string{"root", auth.RoleAdmin, "*/*"}, [3]string{"erin", auth.RoleMaintainer, "*/*"})
	user := func(name string) auth.Principal { return auth.Principal{Kind: auth.KindUser, Name: name} }
	for _, tt := range []struct {
		name   string
		p      auth.Principal
		c      auth.Credential
		status int
	}{
		{"admin", user("root"), rootPassword, http.StatusOK},
		{"admin's personal access token", user("root"), auth.Credential{Kind: auth.CredPAT, TokenID: 1}, http.StatusOK},
		{"admin's token with scopes", user("root"), auth.Credential{Kind: auth.CredPAT, TokenID: 1, Scopes: []string{"read"}}, http.StatusForbidden},
		{"maintainer on */*", user("erin"), rootPassword, http.StatusForbidden},
		{"a deploy token over */*", auth.Principal{Kind: auth.KindToken, Name: "3"},
			auth.Credential{Kind: auth.CredDeploy, TokenID: 3, Mask: "*/*", Scopes: []string{"read"}}, http.StatusForbidden},
		{"anonymous", auth.Anonymous, auth.Credential{}, http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := f.doAs(http.MethodGet, "/api/v1/registries", "", tt.p, tt.c)
			wantStatus(t, rr, tt.status)
			wantChallenge(t, rr)
		})
	}
}
