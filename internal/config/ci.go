package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CI configures token exchange for CI (docs/spec/rbac.md §11, #937): the CI platforms whose OIDC
// tokens POST /-/oidc/token exchanges for a CI token, and the trust policies that decide which
// tokens are exchanged and what their CI token may do. Off while it has no issuer.
type CI struct {
	// TokenLifetime is the lifetime of a CI token: DefaultCITokenLifetime when the file leaves it
	// out, from MinCITokenLifetime to MaxCITokenLifetime.
	TokenLifetime time.Duration `yaml:"tokenLifetime"`
	// Issuers are the CI platforms whose tokens are exchanged. Empty exchanges none.
	Issuers []CIIssuer `yaml:"issuers"`
	// Policies are the trust policies; a token is exchanged when one or more policies of its
	// issuer match it.
	Policies []CIPolicy `yaml:"policies"`
}

// Bounds of auth.ci.tokenLifetime.
const (
	DefaultCITokenLifetime = 15 * time.Minute
	MinCITokenLifetime     = time.Minute
	// MaxCITokenLifetime is the lifetime of a bearer of /v2 (keys.TokenLifetime), so that a token
	// key retired by a rotation verifies every CI token it signed until its end (keys.TokenGrace).
	MaxCITokenLifetime = 20 * time.Minute
)

// CIIssuer is an issuer of auth.ci.issuers.
type CIIssuer struct {
	// Name is the issuer's name, in the principal ci:<name>/<project> (CheckIdentitySource).
	Name string `yaml:"name"`
	// Platform is CIPlatformGitHub or CIPlatformGitLab: the claims a policy may name and the claim
	// of the project (CIPlatforms).
	Platform string `yaml:"platform"`
	// Issuer is the exact iss of the platform's tokens; https, http only with
	// server.insecureHTTP.
	Issuer string `yaml:"issuer"`
	// Audience is what a token's aud must contain; empty is server.externalURL without a trailing
	// slash (CIIssuer.AudienceOr).
	Audience string `yaml:"audience"`
	// CAFile is a PEM file of the CAs of the issuer's TLS certificate, read by serve at start;
	// empty uses the system's roots.
	CAFile string `yaml:"caFile"`
}

// AudienceOr returns the issuer's audience, or externalURL without a trailing slash when it has
// none.
func (i CIIssuer) AudienceOr(externalURL string) string {
	if i.Audience != "" {
		return i.Audience
	}
	return strings.TrimSuffix(externalURL, "/")
}

// CIPolicy is a trust policy of auth.ci.policies.
type CIPolicy struct {
	// Name is the policy's name, in the CI token and its audit row (CheckIdentitySource).
	Name string `yaml:"name"`
	// Issuer is the name of an issuer of auth.ci.issuers.
	Issuer string `yaml:"issuer"`
	// Claims maps each claim the policy names to its values, any of which matches; every claim
	// of the map must match.
	Claims map[string]CIValues `yaml:"claims"`
	// Grants are what the CI token of a token the policy matches may do.
	Grants []CIGrant `yaml:"grants"`
}

// CIGrant is a role on the repos of a mask <format>/<repo>, as a binding's
// (docs/spec/rbac.md §3).
type CIGrant struct {
	Role string `yaml:"role"`
	Mask string `yaml:"mask"`
}

// CIValues are the values of a claim of a policy: in the file, one scalar or a list of scalars,
// each kept as it is written (123 and "123" alike).
type CIValues []string

// UnmarshalYAML reads a scalar or a sequence of scalars.
func (v *CIValues) UnmarshalYAML(n *yaml.Node) error {
	scalar := func(n *yaml.Node) (string, error) {
		switch {
		case n.Kind != yaml.ScalarNode:
			return "", errors.New("a claim's value is a string, a number or a boolean")
		case n.Tag == "!!null":
			return "", nil
		}
		return n.Value, nil
	}
	if n.Kind != yaml.SequenceNode {
		s, err := scalar(n)
		*v = CIValues{s}
		return err
	}
	out := make(CIValues, 0, len(n.Content))
	for _, c := range n.Content {
		s, err := scalar(c)
		if err != nil {
			return err
		}
		out = append(out, s)
	}
	*v = out
	return nil
}

// The CI platforms of auth.ci.issuers[].platform.
const (
	CIPlatformGitHub = "github"
	CIPlatformGitLab = "gitlab"
)

// ClaimKind is how a trust policy compares a claim (docs/spec/rbac.md §11).
type ClaimKind int

// Claim kinds.
const (
	// ClaimID is a numeric id: decimal digits, compared exactly.
	ClaimID ClaimKind = iota + 1
	// ClaimName is a name that may be registered again: compared exactly.
	ClaimName
	// ClaimPattern is a ref, an environment or a workflow: * matches any run of characters but
	// /, ** any run.
	ClaimPattern
	// ClaimValue is one of a few values the platform defines: compared exactly.
	ClaimValue
)

// CIPlatform describes the tokens of a CI platform (docs/spec/rbac.md §11).
type CIPlatform struct {
	// Claims are the claims a policy may name, and how each is compared.
	Claims map[string]ClaimKind
	// Project is the claim that names the project in the principal ci:<issuer>/<project>, of
	// ProjectSegments segments (0: one or more).
	Project         string
	ProjectSegments int
	// IDs are the claims of which every policy names one: the project by a numeric id.
	IDs []string
	// WriteConditions are the claims of which a policy that writes names one.
	WriteConditions []string
	// Event is the claim of the event of the job, and PullRequests its values of a pull request,
	// which no policy that writes matches.
	Event        string
	PullRequests []string
	// Run are the claims of the run that the audit row of an exchange records besides Claims.
	Run []string
}

// CIPlatforms are the platforms by name. Treat it as read-only.
var CIPlatforms = map[string]CIPlatform{
	CIPlatformGitHub: {
		Claims: map[string]ClaimKind{
			"repository_id": ClaimID, "repository_owner_id": ClaimID, "actor_id": ClaimID,
			"repository": ClaimName, "repository_owner": ClaimName, "actor": ClaimName,
			"ref": ClaimPattern, "environment": ClaimPattern, "workflow_ref": ClaimPattern, "job_workflow_ref": ClaimPattern,
			"ref_type": ClaimValue, "ref_protected": ClaimValue, "event_name": ClaimValue, "runner_environment": ClaimValue,
			"repository_visibility": ClaimValue,
		},
		Project: "repository", ProjectSegments: 2,
		IDs:             []string{"repository_id", "repository_owner_id"},
		WriteConditions: []string{"ref", "environment"},
		Event:           "event_name",
		PullRequests:    []string{"pull_request", "pull_request_target", "pull_request_review", "pull_request_review_comment"},
		Run:             []string{"run_id", "run_attempt", "sha"},
	},
	CIPlatformGitLab: {
		Claims: map[string]ClaimKind{
			"project_id": ClaimID, "namespace_id": ClaimID, "user_id": ClaimID,
			"project_path": ClaimName, "namespace_path": ClaimName, "user_login": ClaimName,
			"ref": ClaimPattern, "ref_path": ClaimPattern, "environment": ClaimPattern, "ci_config_ref_uri": ClaimPattern,
			"ref_type": ClaimValue, "ref_protected": ClaimValue, "environment_protected": ClaimValue, "pipeline_source": ClaimValue,
			"runner_environment": ClaimValue, "deployment_tier": ClaimValue, "project_visibility": ClaimValue,
		},
		Project:         "project_path",
		IDs:             []string{"project_id", "namespace_id"},
		WriteConditions: []string{"ref", "ref_path", "environment"},
		Event:           "pipeline_source",
		PullRequests:    []string{"merge_request_event", "external_pull_request_event"},
		Run:             []string{"pipeline_id", "job_id", "sha"},
	},
}

// The roles a grant may have: those of docs/spec/rbac.md §3 but admin.
var ciRoles = []string{"reader", "writer", "maintainer"}

// CIWrites reports whether role grants more than read.
func CIWrites(role string) bool { return role == "writer" || role == "maintainer" }

// validate checks auth.ci (docs/spec/rbac.md §11): the lifetime, the issuers
// (validateIssuers), and each policy's unique name, its issuer, and validatePolicy.
func (c CI) validate(p *problems, s Server) {
	if c.TokenLifetime < MinCITokenLifetime || c.TokenLifetime > MaxCITokenLifetime {
		p.add("auth.ci.tokenLifetime", "must be from 1m to 20m, the lifetime of a bearer of /v2")
	}
	platforms := c.validateIssuers(p, s)
	names := map[string]bool{}
	for i, pol := range c.Policies {
		key := fmt.Sprintf("auth.ci.policies[%d]", i)
		if err := CheckIdentitySource(pol.Name); err != nil {
			p.add(key+".name", "%s", err)
		} else if names[pol.Name] {
			p.add(key+".name", "%q names another policy too", pol.Name)
		}
		names[pol.Name] = true
		platform, ok := platforms[pol.Issuer]
		switch {
		case !ok:
			p.add(key+".issuer", "%q is no issuer of auth.ci.issuers", pol.Issuer)
		case CIPlatforms[platform].Project != "":
			validatePolicy(p, key, pol, platform)
		}
	}
}

// validateIssuers checks each issuer's unique name, its platform, its issuer as
// auth.kubernetes's and unique, and an audience, and returns the platform of each name.
func (c CI) validateIssuers(p *problems, s Server) map[string]string {
	platforms, issuers := map[string]string{}, map[string]bool{}
	for i, is := range c.Issuers {
		key := fmt.Sprintf("auth.ci.issuers[%d]", i)
		if err := CheckIdentitySource(is.Name); err != nil {
			p.add(key+".name", "%s", err)
		} else if _, ok := platforms[is.Name]; ok {
			p.add(key+".name", "%q names another issuer too", is.Name)
		}
		if _, ok := CIPlatforms[is.Platform]; !ok {
			p.add(key+".platform", "%q is not github or gitlab", is.Platform)
		}
		platforms[is.Name] = is.Platform
		u, err := url.Parse(is.Issuer)
		switch {
		case err != nil || !isHTTP(u) || u.RawQuery != "" || u.Fragment != "":
			p.add(key+".issuer", "must be an absolute http(s) URL without a query")
		case u.Scheme != "https" && !s.InsecureHTTP:
			p.add(key+".issuer", "must be https (http only with server.insecureHTTP)")
		case issuers[is.Issuer]:
			p.add(key+".issuer", "%q is another issuer's", is.Issuer)
		}
		issuers[is.Issuer] = true
		if is.AudienceOr(s.ExternalURL) == "" {
			p.add(key+".audience", "must be set when server.externalURL is empty")
		}
	}
	return platforms
}

// validatePolicy checks the policy pol of the platform under key with the safe defaults of
// docs/spec/rbac.md §11: its claims (validateClaims), the project named by an id, its grants
// (validateGrants), and for a policy that writes a ref or an environment, and an event condition
// that names more than pull requests.
func validatePolicy(p *problems, key string, pol CIPolicy, platform string) {
	pf := CIPlatforms[platform]
	validateClaims(p, key, pol, platform)
	if !slices.ContainsFunc(pf.IDs, func(c string) bool { return pol.Claims[c] != nil }) {
		p.add(key+".claims", "must name the project by an id, %s: a name can be registered again by anyone", strings.Join(pf.IDs, " or "))
	}
	if !validateGrants(p, key, pol.Grants) {
		return
	}
	if !slices.ContainsFunc(pf.WriteConditions, func(c string) bool { return pol.Claims[c] != nil }) {
		p.add(key+".claims", "a policy that grants writer or maintainer needs %s: without it every branch and pull request publishes",
			strings.Join(pf.WriteConditions, ", "))
	}
	if events, ok := pol.Claims[pf.Event]; ok && len(events) > 0 &&
		!slices.ContainsFunc(events, func(e string) bool { return !slices.Contains(pf.PullRequests, e) }) {
		p.add(key+".claims."+pf.Event, "names only pull requests, which never write")
	}
}

// validateClaims checks that each claim of pol is one of the platform's, with values that fit
// its kind.
func validateClaims(p *problems, key string, pol CIPolicy, platform string) {
	pf := CIPlatforms[platform]
	for _, c := range slices.Sorted(maps.Keys(pol.Claims)) {
		kind, ok := pf.Claims[c]
		if !ok {
			p.add(key+".claims."+c, "is not a claim a policy of %s names", platform)
			continue
		}
		if len(pol.Claims[c]) == 0 {
			p.add(key+".claims."+c, "must have a value")
		}
		for _, v := range pol.Claims[c] {
			if msg := checkClaimValue(kind, v); msg != "" {
				p.add(key+".claims."+c, "%q %s", v, msg)
			}
		}
	}
}

// validateGrants checks one or more grants, each with a role but admin and a mask, and reports
// whether one of them writes.
func validateGrants(p *problems, key string, grants []CIGrant) (writes bool) {
	if len(grants) == 0 {
		p.add(key+".grants", "must grant a role on a mask")
	}
	for j, g := range grants {
		gk := fmt.Sprintf("%s.grants[%d]", key, j)
		switch {
		case g.Role == "admin":
			p.add(gk+".role", "admin is not for a CI token: reader, writer or maintainer")
		case !slices.Contains(ciRoles, g.Role):
			p.add(gk+".role", "%q is not reader, writer or maintainer", g.Role)
		}
		writes = writes || CIWrites(g.Role)
		if err := checkMask(g.Mask); err != nil {
			p.add(gk+".mask", "%s", err)
		}
	}
	return writes
}

// checkClaimValue returns why v cannot be a value of a claim of kind, or "".
func checkClaimValue(kind ClaimKind, v string) string {
	switch {
	case v == "":
		return "is empty"
	case strings.Trim(v, "*") == "":
		return "is only *, which is no condition: leave the claim out"
	case kind == ClaimID && strings.Trim(v, "0123456789") != "":
		return "is not an id, decimal digits"
	case kind != ClaimPattern && strings.Contains(v, "*"):
		return "has *, a wildcard in ref, environment and workflow claims only"
	}
	return ""
}

// checkMask returns nil when mask is <format>/<repo>, each segment * or a name: a format's name
// (IdentitySourcePattern, the rule of format names) and a repo's (RepoNamePattern).
func checkMask(mask string) error {
	format, repo, ok := strings.Cut(mask, "/")
	switch {
	case !ok || strings.Contains(repo, "/"):
		return fmt.Errorf("%q is not <format>/<repo>", mask)
	case format != "*" && !IdentitySourcePattern.MatchString(format):
		return fmt.Errorf("%q: the format is * or a format's name", mask)
	case repo != "*" && (len(repo) > MaxNameLen || !RepoNamePattern.MatchString(repo)):
		return fmt.Errorf("%q: the repo is * or a repo's name", mask)
	}
	return nil
}
