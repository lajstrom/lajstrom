// Package ci is token exchange for CI (docs/spec/rbac.md §11): the trust policies of auth.ci
// matched against the claims of a CI platform's OIDC token, the exchange at POST /-/oidc/token
// that mints a CI token, and the CI token as a credential.
//
// No token it reads or mints is logged or put in an error: an error names the issuer, the
// project and the rule that failed.
package ci

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
)

// ErrRefused means a token is refused: its project claim is missing or breaks the grammar of a
// ci: principal, or no policy of its issuer matches it. The message is the client's: it names
// the token's own claims and nothing of the policies.
var ErrRefused = errors.New("ci: refused")

// maxAuditClaim bounds a claim's value in the audit row of an exchange, in bytes.
const maxAuditClaim = 256

// Policies are the issuers and the trust policies of auth.ci, compiled. Create them with New;
// their methods may be called concurrently.
type Policies struct {
	issuers  []*Issuer
	policies map[string]*policy // by name
}

// Issuer is an issuer of auth.ci.issuers.
type Issuer struct {
	// Name is the issuer's name, in the principal ci:<name>/<project>.
	Name string
	// Issuer is the exact iss of its tokens.
	Issuer string
	// Platform is config.CIPlatformGitHub or config.CIPlatformGitLab.
	Platform string

	pf       config.CIPlatform
	policies []*policy
}

// policy is a trust policy, its values compiled.
type policy struct {
	name   string
	claims map[string][]value
	grants []auth.Grant
	writes bool
}

// value is a value of a claim of a policy: a string compared exactly, or the regular expression
// of a pattern.
type value struct {
	exact   string
	pattern *regexp.Regexp
}

func (v value) matches(s string) bool {
	if v.pattern != nil {
		return v.pattern.MatchString(s)
	}
	return v.exact == s
}

// New compiles c, which config.Validate has checked: an issuer of an unknown platform and a
// policy of an unknown issuer are errors, as they are there.
func New(c config.CI) (*Policies, error) {
	ps := &Policies{policies: map[string]*policy{}}
	byName := map[string]*Issuer{}
	for _, is := range c.Issuers {
		pf, ok := config.CIPlatforms[is.Platform]
		if !ok {
			return nil, fmt.Errorf("ci: issuer %s: unknown platform %q", is.Name, is.Platform)
		}
		i := &Issuer{Name: is.Name, Issuer: is.Issuer, Platform: is.Platform, pf: pf}
		ps.issuers = append(ps.issuers, i)
		byName[is.Name] = i
	}
	for _, cp := range c.Policies {
		i, ok := byName[cp.Issuer]
		if !ok {
			return nil, fmt.Errorf("ci: policy %s: unknown issuer %q", cp.Name, cp.Issuer)
		}
		p := &policy{name: cp.Name, claims: map[string][]value{}}
		for claim, values := range cp.Claims {
			for _, v := range values {
				p.claims[claim] = append(p.claims[claim], compile(i.pf.Claims[claim], v))
			}
		}
		for _, g := range cp.Grants {
			p.grants = append(p.grants, auth.Grant{Role: g.Role, Mask: g.Mask})
			p.writes = p.writes || config.CIWrites(g.Role)
		}
		i.policies = append(i.policies, p)
		ps.policies[p.name] = p
	}
	return ps, nil
}

// compile returns the value v of a claim of kind: a pattern's * matches any run of characters but
// /, and ** any run; every other value is exact.
func compile(kind config.ClaimKind, v string) value {
	if kind != config.ClaimPattern || !strings.Contains(v, "*") {
		return value{exact: v}
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(v); {
		switch {
		case strings.HasPrefix(v[i:], "**"):
			b.WriteString(".*")
			i += 2
		case v[i] == '*':
			b.WriteString("[^/]*")
			i++
		default:
			j := i + strings.IndexByte(v[i:]+"*", '*')
			b.WriteString(regexp.QuoteMeta(v[i:j]))
			i = j
		}
	}
	b.WriteString("$")
	return value{pattern: regexp.MustCompile(b.String())}
}

// Issuers returns the issuers, in the order of the configuration.
func (ps *Policies) Issuers() []*Issuer { return ps.issuers }

// IssuerOf returns the issuer whose iss is iss, or nil.
func (ps *Policies) IssuerOf(iss string) *Issuer {
	for _, i := range ps.issuers {
		if i.Issuer == iss {
			return i
		}
	}
	return nil
}

// Match is a token a policy matched: its principal, the policies that matched and their grants,
// and the claims its audit row records.
type Match struct {
	Principal auth.Principal
	// Issuer is the issuer's name.
	Issuer string
	// Policies are the names of the policies that matched, in the order of the configuration.
	Policies []string
	// Grants are their grants.
	Grants []auth.Grant
	// Claims are the claims of the platform's list and of its run that the token has, as text of
	// at most maxAuditClaim bytes.
	Claims map[string]string
}

// Match matches the verified claims of a token of i against i's policies (docs/spec/rbac.md §11):
// the project names the principal, and every policy whose claims all match it counts, but that a
// policy that writes never matches a token of a pull request. A project claim that is missing or
// breaks the grammar, and a token that no policy matches, are errors wrapping ErrRefused.
func (i *Issuer) Match(claims map[string]any) (*Match, error) {
	project, _ := claims[i.pf.Project].(string)
	if i.pf.ProjectSegments > 0 && strings.Count(project, "/") != i.pf.ProjectSegments-1 {
		return nil, fmt.Errorf("%w: the token's %s is not <owner>/<name>", ErrRefused, i.pf.Project)
	}
	p, err := auth.CIPrincipal(i.Name, project)
	if err != nil {
		return nil, fmt.Errorf("%w: the token's %s is not a project: %w", ErrRefused, i.pf.Project, err)
	}
	text := map[string]string{}
	for c := range claims {
		if s, ok := claimText(claims[c]); ok {
			text[c] = s
		}
	}
	pr := slices.Contains(i.pf.PullRequests, text[i.pf.Event])
	m := &Match{Principal: p, Issuer: i.Name, Claims: map[string]string{}}
	for _, pol := range i.policies {
		if (pol.writes && pr) || !pol.matches(text) {
			continue
		}
		m.Policies = append(m.Policies, pol.name)
		m.Grants = append(m.Grants, pol.grants...)
	}
	if len(m.Policies) == 0 {
		return nil, fmt.Errorf("%w: no trust policy matches the token: issuer %s, %s %s, ref %s, environment %s, %s %s",
			ErrRefused, i.Name, i.pf.Project, project, orNone(text["ref"]), orNone(text["environment"]), i.pf.Event, orNone(text[i.pf.Event]))
	}
	for c, s := range text {
		if _, ok := i.pf.Claims[c]; ok || slices.Contains(i.pf.Run, c) {
			m.Claims[c] = s[:min(len(s), maxAuditClaim)]
		}
	}
	return m, nil
}

// matches reports whether every claim of p has a value that matches the token's claim of that
// name, in text.
func (p *policy) matches(text map[string]string) bool {
	for claim, values := range p.claims {
		s, ok := text[claim]
		if !ok || !slices.ContainsFunc(values, func(v value) bool { return v.matches(s) }) {
			return false
		}
	}
	return true
}

// Grants returns the grants of the policies named, as the configuration has them now: a name it
// does not have grants nothing. ok is false when it has none of them.
func (ps *Policies) Grants(names []string) (grants []auth.Grant, ok bool) {
	for _, n := range names {
		if p := ps.policies[n]; p != nil {
			grants, ok = append(grants, p.grants...), true
		}
	}
	return grants, ok
}

// claimText returns a claim's value as a policy compares it: a string as it is, a number or a
// boolean as its JSON text; false for any other value.
func claimText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'g', -1, 64), true
	}
	return "", false
}

// orNone is s, or "none" when it is empty.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
