package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCI: auth.ci (docs/spec/rbac.md §11) loads as written, a claim's value is a scalar or a list,
// ids and booleans unquoted alike, the audience defaults to server.externalURL and the lifetime
// to 15 minutes.
func TestCI(t *testing.T) {
	c, err := Load(writeConfig(t, minimal+`auth:
  ci:
    issuers:
      - {name: github, platform: github, issuer: "https://token.actions.githubusercontent.com"}
      - {name: gitlab, platform: gitlab, issuer: "https://gitlab.example", audience: lajstrom, caFile: /ca/gitlab.pem}
    policies:
      - name: app-release
        issuer: github
        claims:
          repository_id: 123456789
          repository: acme/app
          ref: refs/tags/v*
          environment: release
          event_name: [push, workflow_dispatch]
        grants:
          - {role: writer, mask: pypi/internal}
          - {role: reader, mask: "*/*"}
      - name: platform-read
        issuer: gitlab
        claims: {namespace_id: "42", ref_protected: true}
        grants: [{role: reader, mask: "oci/*"}]
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	ci := c.Auth.CI
	if ci.TokenLifetime != 15*time.Minute {
		t.Errorf("tokenLifetime = %v", ci.TokenLifetime)
	}
	if len(ci.Issuers) != 2 || ci.Issuers[1] != (CIIssuer{Name: "gitlab", Platform: "gitlab", Issuer: "https://gitlab.example",
		Audience: "lajstrom", CAFile: "/ca/gitlab.pem"}) {
		t.Errorf("issuers = %+v", ci.Issuers)
	}
	if a := ci.Issuers[0].AudienceOr("https://art.example.com/"); a != "https://art.example.com" {
		t.Errorf("the default audience is %q", a)
	}
	p := ci.Policies[0]
	if p.Name != "app-release" || p.Issuer != "github" || len(p.Grants) != 2 || p.Grants[1] != (CIGrant{Role: "reader", Mask: "*/*"}) {
		t.Errorf("policy = %+v", p)
	}
	for claim, want := range map[string][]string{"repository_id": {"123456789"}, "ref": {"refs/tags/v*"},
		"event_name": {"push", "workflow_dispatch"}} {
		if !slices.Equal(p.Claims[claim], want) {
			t.Errorf("claims.%s = %q, want %q", claim, p.Claims[claim], want)
		}
	}
	if got := ci.Policies[1].Claims["ref_protected"]; !slices.Equal(got, []string{"true"}) {
		t.Errorf("an unquoted boolean is %q", got)
	}
}

// TestCIRefusals: each rule of auth.ci names its key, the safe defaults of docs/spec/rbac.md §11
// among them.
func TestCIRefusals(t *testing.T) {
	_, err := Load(writeConfig(t, minimal+`auth:
  ci:
    tokenLifetime: 30m
    issuers:
      - {name: GitHub, platform: github, issuer: "https://a.example"}
      - {name: gh, platform: jenkins, issuer: "http://b.example"}
      - {name: gh, platform: gitlab, issuer: "https://a.example"}
    policies:
      - {name: no-issuer, issuer: nobody, claims: {repository_id: "1"}, grants: [{role: reader, mask: "*/*"}]}
      - name: no-id
        issuer: gh
        claims: {project_path: acme/app, ref: main}
        grants: [{role: writer, mask: pypi/app}]
      - name: no-ref
        issuer: gh
        claims: {project_id: "7"}
        grants: [{role: maintainer, mask: npm/app}]
      - name: bad-values
        issuer: gh
        claims: {project_id: "7x", namespace_id: "", project_path: "acme/*", ref: "**", sub: x, environment: []}
        grants: [{role: reader, mask: npm/app}]
      - name: prs
        issuer: gh
        claims: {project_id: "7", ref: main, pipeline_source: [merge_request_event, external_pull_request_event]}
        grants: [{role: writer, mask: "npm/*"}]
      - name: grants
        issuer: gh
        claims: {project_id: "7"}
        grants: [{role: admin, mask: "*/*"}, {role: owner, mask: "a/b/c"}, {role: reader, mask: "Apt/x"}, {role: reader, mask: "apt/X"}]
      - {name: nothing, issuer: gh, claims: {project_id: "7"}}
      - {name: nothing, issuer: gh, claims: {project_id: "7"}, grants: [{role: reader, mask: "*/*"}]}
`), nil)
	for _, w := range []string{
		"auth.ci.tokenLifetime: must be from 1m to 20m",
		`auth.ci.issuers[0].name: "GitHub" is not lowercase`,
		`auth.ci.issuers[1].platform: "jenkins" is not github or gitlab`,
		"auth.ci.issuers[1].issuer: must be https",
		`auth.ci.issuers[2].name: "gh" names another issuer too`,
		`auth.ci.issuers[2].issuer: "https://a.example" is another issuer's`,
		`auth.ci.policies[0].issuer: "nobody" is no issuer of auth.ci.issuers`,
		"auth.ci.policies[1].claims: must name the project by an id, project_id or namespace_id",
		"auth.ci.policies[2].claims: a policy that grants writer or maintainer needs ref, ref_path, environment",
		`auth.ci.policies[3].claims.project_id: "7x" is not an id`,
		`auth.ci.policies[3].claims.namespace_id: "" is empty`,
		`auth.ci.policies[3].claims.project_path: "acme/*" has *, a wildcard in ref, environment and workflow claims only`,
		`auth.ci.policies[3].claims.ref: "**" is only *`,
		"auth.ci.policies[3].claims.sub: is not a claim a policy of gitlab names",
		"auth.ci.policies[3].claims.environment: must have a value",
		"auth.ci.policies[4].claims.pipeline_source: names only pull requests, which never write",
		"auth.ci.policies[5].grants[0].role: admin is not for a CI token",
		`auth.ci.policies[5].grants[1].role: "owner" is not reader, writer or maintainer`,
		`auth.ci.policies[5].grants[1].mask: "a/b/c" is not <format>/<repo>`,
		`auth.ci.policies[5].grants[2].mask: "Apt/x": the format is * or a format's name`,
		`auth.ci.policies[5].grants[3].mask: "apt/X": the repo is * or a repo's name`,
		"auth.ci.policies[6].grants: must grant a role on a mask",
		`auth.ci.policies[7].name: "nothing" names another policy too`,
	} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("Load = %v\nwant %q", err, w)
		}
	}
	if err != nil && strings.Contains(err.Error(), "policies[5].claims") {
		t.Errorf("a policy that reads only needs no ref: %v", err)
	}

	_, err = Load(writeConfig(t, listenOnly+"  insecureHTTP: true\nauth: {ci: {tokenLifetime: 30s, issuers: [{name: a, platform: github, issuer: \"http://a.example\"}]}}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "auth.ci.issuers[0].audience: must be set when server.externalURL is empty") ||
		!strings.Contains(err.Error(), "auth.ci.tokenLifetime") || strings.Contains(err.Error(), "issuers[0].issuer") {
		t.Errorf("Load without externalURL = %v", err)
	}
	_, err = Load(writeConfig(t, minimal+"auth: {ci: {policies: [{name: a, issuer: b, claims: {x: {y: z}}}]}}\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "a claim's value is a string, a number or a boolean") {
		t.Errorf("Load with a map for a value = %v", err)
	}
}

// TestCIWriteConditions: a policy that writes may name its ref or its environment, and an event
// list that holds one event of a pull request among others is taken: the pull request is
// refused at the exchange (docs/spec/rbac.md §11).
func TestCIWriteConditions(t *testing.T) {
	_, err := Load(writeConfig(t, minimal+`auth:
  ci:
    issuers: [{name: github, platform: github, issuer: "https://token.actions.githubusercontent.com"}]
    policies:
      - {name: by-env, issuer: github, claims: {repository_owner_id: "9", environment: prod}, grants: [{role: writer, mask: "oci/*"}]}
      - {name: by-ref, issuer: github, claims: {repository_id: "9", ref: "refs/heads/**", event_name: [push, pull_request]}, grants: [{role: maintainer, mask: "oci/app"}]}
`), nil)
	if err != nil {
		t.Errorf("Load = %v", err)
	}
}
