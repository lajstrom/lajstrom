package ci

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
)

// testCI is the configuration of the tests: a GitHub and a GitLab issuer, a release policy that
// writes on tags of an environment, a policy that reads on every ref, and a GitLab policy on
// protected refs.
var testCI = config.CI{
	Issuers: []config.CIIssuer{
		{Name: "github", Platform: config.CIPlatformGitHub, Issuer: "https://token.actions.example"},
		{Name: "gitlab", Platform: config.CIPlatformGitLab, Issuer: "https://gitlab.example"},
	},
	Policies: []config.CIPolicy{
		{Name: "app-release", Issuer: "github", Claims: map[string]config.CIValues{
			"repository_id": {"123"}, "repository": {"acme/app"}, "ref": {"refs/tags/v*"}, "environment": {"release", "release-**"},
		}, Grants: []config.CIGrant{{Role: "writer", Mask: "pypi/internal"}}},
		{Name: "acme-read", Issuer: "github", Claims: map[string]config.CIValues{"repository_owner_id": {"9"}},
			Grants: []config.CIGrant{{Role: "reader", Mask: "*/*"}}},
		{Name: "platform", Issuer: "gitlab", Claims: map[string]config.CIValues{
			"namespace_id": {"42"}, "ref_protected": {"true"}, "ref_path": {"refs/heads/main", "refs/tags/**"},
		}, Grants: []config.CIGrant{{Role: "maintainer", Mask: "oci/*"}}},
	},
}

// github returns the claims of a GitHub Actions token of acme/app (repository 123 of owner 9),
// with over its keys replaced; a nil value removes the claim.
func github(over map[string]any) map[string]any {
	c := map[string]any{
		"iss": "https://token.actions.example", "aud": "https://art.example.com",
		"sub": "repo:acme/app:environment:release", "repository": "acme/app", "repository_id": "123",
		"repository_owner": "acme", "repository_owner_id": "9", "ref": "refs/tags/v1.2.3", "ref_type": "tag",
		"environment": "release", "event_name": "push", "workflow_ref": "acme/app/.github/workflows/release.yml@refs/tags/v1.2.3",
		"job_workflow_ref": "acme/app/.github/workflows/release.yml@refs/tags/v1.2.3", "run_id": "777", "run_attempt": "1",
		"sha": "0123abcd", "actor": "alice", "actor_id": "55", "runner_environment": "github-hosted",
	}
	return with(c, over)
}

// gitlab returns the claims of a GitLab CI token of acme/platform/app (project 7 of namespace
// 42), with over.
func gitlab(over map[string]any) map[string]any {
	c := map[string]any{
		"iss": "https://gitlab.example", "aud": "https://art.example.com", "sub": "project_path:acme/platform/app:ref_type:branch:ref:main",
		"project_id": "7", "project_path": "acme/platform/app", "namespace_id": "42", "namespace_path": "acme/platform",
		"ref": "main", "ref_type": "branch", "ref_path": "refs/heads/main", "ref_protected": "true", "pipeline_source": "push",
		"pipeline_id": "1001", "job_id": "2002", "sha": "fedc", "user_login": "bob", "user_email": "bob@example.com",
	}
	return with(c, over)
}

func with(c, over map[string]any) map[string]any {
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// TestMatch: the policies of an issuer match by every claim they name, wildcards as
// docs/spec/rbac.md §11 has them, numbers and booleans as their text; the token's grants are
// the union of those that match, and a policy that writes never matches a pull request.
func TestMatch(t *testing.T) {
	ps, err := New(testCI)
	if err != nil {
		t.Fatal(err)
	}
	gh, gl := ps.IssuerOf("https://token.actions.example"), ps.IssuerOf("https://gitlab.example")
	if gh == nil || gl == nil || gh.Name != "github" || ps.IssuerOf("https://other.example") != nil {
		t.Fatalf("IssuerOf: %v %v", gh, gl)
	}
	for _, tt := range []struct {
		name     string
		i        *Issuer
		claims   map[string]any
		policies []string
	}{
		{"a release", gh, github(nil), []string{"app-release", "acme-read"}},
		{"ids as numbers", gh, github(map[string]any{"repository_id": float64(123), "repository_owner_id": float64(9)}), []string{"app-release", "acme-read"}},
		{"an environment by **", gh, github(map[string]any{"environment": "release-eu/west"}), []string{"app-release", "acme-read"}},
		{"* stops at /", gh, github(map[string]any{"ref": "refs/tags/v1/rc"}), []string{"acme-read"}},
		{"a branch", gh, github(map[string]any{"ref": "refs/heads/main"}), []string{"acme-read"}},
		{"no environment", gh, github(map[string]any{"environment": nil}), []string{"acme-read"}},
		{"another repository of the owner", gh, github(map[string]any{"repository": "acme/lib", "repository_id": "124"}), []string{"acme-read"}},
		{"the name alone is not the id", gh, github(map[string]any{"repository_id": "999"}), []string{"acme-read"}},
		{"a pull_request_target reads", gh, github(map[string]any{"event_name": "pull_request_target"}), []string{"acme-read"}},
		{"a pull request reads", gh, github(map[string]any{"event_name": "pull_request", "ref": "refs/pull/5/merge"}), []string{"acme-read"}},
		{"gitlab on main", gl, gitlab(nil), []string{"platform"}},
		{"gitlab ref_protected as a boolean", gl, gitlab(map[string]any{"ref_protected": true}), []string{"platform"}},
		{"gitlab on a tag below /", gl, gitlab(map[string]any{"ref_path": "refs/tags/release/1.0", "ref_type": "tag"}), []string{"platform"}},
	} {
		m, err := tt.i.Match(tt.claims)
		if err != nil {
			t.Errorf("%s: Match = %v", tt.name, err)
			continue
		}
		if !slices.Equal(m.Policies, tt.policies) {
			t.Errorf("%s: policies %v, want %v", tt.name, m.Policies, tt.policies)
		}
	}
	m, _ := gh.Match(github(nil))
	if m.Principal != (auth.Principal{Kind: auth.KindCI, Name: "github/acme/app"}) || m.Issuer != "github" ||
		!slices.Equal(m.Grants, []auth.Grant{{Role: "writer", Mask: "pypi/internal"}, {Role: "reader", Mask: "*/*"}}) {
		t.Errorf("Match = %+v", m)
	}
	m, _ = gl.Match(gitlab(nil))
	if m.Principal.String() != "ci:gitlab/acme/platform/app" {
		t.Errorf("gitlab principal = %s", m.Principal)
	}
}

// TestMatchRefused: a token without its project, a GitHub project that is not <owner>/<name>, and
// a token that no policy matches are refused, the last naming the token's claims and no policy;
// a merge request pipeline never matches a policy that writes.
func TestMatchRefused(t *testing.T) {
	ps, _ := New(testCI)
	gh, gl := ps.IssuerOf("https://token.actions.example"), ps.IssuerOf("https://gitlab.example")
	for _, tt := range []struct {
		name   string
		i      *Issuer
		claims map[string]any
		msg    string
	}{
		{"no repository", gh, github(map[string]any{"repository": nil}), "the token's repository is not <owner>/<name>"},
		{"three segments", gh, github(map[string]any{"repository": "acme/app/x"}), "is not <owner>/<name>"},
		{"a dot segment", gl, gitlab(map[string]any{"project_path": "acme/../app"}), "the token's project_path is not a project"},
		{"another owner", gh, github(map[string]any{"repository_owner_id": "10", "repository_id": "5", "repository": "evil/app"}),
			"no trust policy matches the token: issuer github, repository evil/app, ref refs/tags/v1.2.3, environment release, event_name push"},
		{"a merge request", gl, gitlab(map[string]any{"pipeline_source": "merge_request_event", "ref_path": "refs/merge-requests/3/head"}),
			"pipeline_source merge_request_event"},
		{"an unprotected ref", gl, gitlab(map[string]any{"ref_protected": "false"}), "no trust policy matches"},
	} {
		_, err := tt.i.Match(tt.claims)
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: Match = %v, want ErrRefused with %q", tt.name, err, tt.msg)
		}
		if err != nil && (strings.Contains(err.Error(), "app-release") || strings.Contains(err.Error(), "platform ")) {
			t.Errorf("%s: the refusal names a policy: %v", tt.name, err)
		}
	}
	// A merge request pipeline on a protected ref that the policy names: still refused.
	if _, err := gl.Match(gitlab(map[string]any{"pipeline_source": "merge_request_event"})); !errors.Is(err, ErrRefused) {
		t.Errorf("a merge request pipeline of main: %v", err)
	}
}

// TestMatchAuditClaims: the audit claims are those of the platform's list and of its run, as
// text cut at 256 bytes; never sub, aud nor GitLab's user_email.
func TestMatchAuditClaims(t *testing.T) {
	ps, _ := New(testCI)
	m, err := ps.IssuerOf("https://gitlab.example").Match(gitlab(map[string]any{"ref_path": "refs/heads/main", "ci_config_ref_uri": strings.Repeat("x", 300)}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"project_id", "project_path", "namespace_id", "ref", "ref_path", "ref_protected", "pipeline_source",
		"pipeline_id", "job_id", "sha", "user_login"} {
		if m.Claims[c] == "" {
			t.Errorf("no %s in %v", c, m.Claims)
		}
	}
	for _, c := range []string{"sub", "aud", "iss", "user_email"} {
		if _, ok := m.Claims[c]; ok {
			t.Errorf("%s is in the audit claims", c)
		}
	}
	if len(m.Claims["ci_config_ref_uri"]) != 256 {
		t.Errorf("a long claim is %d bytes", len(m.Claims["ci_config_ref_uri"]))
	}
	m, _ = ps.IssuerOf("https://token.actions.example").Match(github(nil))
	if m.Claims["run_id"] != "777" || m.Claims["job_workflow_ref"] == "" || m.Claims["actor_id"] != "55" {
		t.Errorf("github audit claims = %v", m.Claims)
	}
}

// TestPatterns: * matches any run of characters but /, ** any run, and nothing else is special.
func TestPatterns(t *testing.T) {
	for _, tt := range []struct {
		pattern, s string
		want       bool
	}{
		{"refs/tags/v*", "refs/tags/v1.2.3", true},
		{"refs/tags/v*", "refs/tags/v", true},
		{"refs/tags/v*", "refs/tags/v1/2", false},
		{"refs/tags/v*", "refs/tags/x1", false},
		{"refs/heads/**", "refs/heads/a/b/c", true},
		{"refs/heads/**", "refs/heads", false},
		{"v1.*", "v1x2", false},
		{"v1.*", "v1.2", true},
		{"release-*-eu", "release-1-eu", true},
		{"release-*-eu", "release-1/2-eu", false},
		{"a+b?(c)", "a+b?(c)", true},
		{"a+b?(c)*", "aab", false},
	} {
		if got := compile(config.ClaimPattern, tt.pattern).matches(tt.s); got != tt.want {
			t.Errorf("%q matches %q: %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
	if compile(config.ClaimName, "acme/*").matches("acme/app") {
		t.Error("a name is a pattern")
	}
}

// TestGrants: the grants of a CI token's policies are the configuration's now; a name it does not
// have grants nothing, and none known is not ok.
func TestGrants(t *testing.T) {
	ps, _ := New(testCI)
	g, ok := ps.Grants([]string{"gone", "acme-read"})
	if !ok || !slices.Equal(g, []auth.Grant{{Role: "reader", Mask: "*/*"}}) {
		t.Errorf("Grants = %v, %v", g, ok)
	}
	if g, ok := ps.Grants([]string{"gone"}); ok || g != nil {
		t.Errorf("Grants of an unknown policy = %v, %v", g, ok)
	}
	if _, err := New(config.CI{Policies: []config.CIPolicy{{Name: "a", Issuer: "nobody"}}}); err == nil {
		t.Error("New with a policy of no issuer")
	}
}
