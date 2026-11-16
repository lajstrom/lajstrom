package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The principal of token exchange for CI (docs/spec/rbac.md §11): ci:<issuer>/<project>, the job
// of a CI platform whose OIDC token a trust policy took, <issuer> the name of the issuer in
// auth.ci and <project> GitHub's repository or GitLab's project_path. It has no row and no
// bindings: its rights are the grants of the policies its CI token names.

// CIPrefix begins the secret of a CI token: ljs_ci_ and a JWT signed with the token key.
const CIPrefix = "ljs_ci_"

// MaxCIProject bounds the project of a ci: principal, in bytes.
const MaxCIProject = 255

var ciSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Grant is a role on the repos of a mask: what a trust policy grants a CI token
// (docs/spec/rbac.md §11). Its role is reader, writer or maintainer.
type Grant struct {
	Role string
	Mask string
}

// CIPrincipal returns the principal ci:<issuer>/<project>, or an error wrapping
// ErrInvalidPrincipal when issuer is not the name of an issuer (config.CheckIdentitySource) or
// project breaks CheckCIProject.
func CIPrincipal(issuer, project string) (Principal, error) {
	name := issuer + "/" + project
	if err := checkCIName(name); err != nil {
		return Principal{}, fmt.Errorf("%w: ci:%s: %w", ErrInvalidPrincipal, name, err)
	}
	return Principal{Kind: KindCI, Name: name}, nil
}

// CheckCIProject returns nil when project is the project of a ci: principal: one or more segments
// of letters, digits, ".", "-" and "_" separated by "/", none "." or "..", at most MaxCIProject
// bytes.
func CheckCIProject(project string) error {
	if project == "" || len(project) > MaxCIProject {
		return fmt.Errorf("a project is 1 to %d bytes", MaxCIProject)
	}
	for s := range strings.SplitSeq(project, "/") {
		if s == "." || s == ".." || !ciSegmentPattern.MatchString(s) {
			return errors.New("a segment of a project is letters, digits, ., - and _, and is not . or .. alone")
		}
	}
	return nil
}

// checkCIName checks <issuer>/<project>.
func checkCIName(name string) error {
	issuer, project, ok := strings.Cut(name, "/")
	if !ok {
		return errors.New("want <issuer>/<project>")
	}
	if err := config.CheckIdentitySource(issuer); err != nil {
		return fmt.Errorf("the issuer: %w", err)
	}
	return CheckCIProject(project)
}

// ciTokenKey is the context key of the id of the CI token of a request that a bearer of /v2
// carries (WithCIToken).
type ciTokenKey struct{}

// WithCIToken returns a copy of ctx that carries id, the CI token's id that the bearer of the
// request names (its claim ci): the facade stores it, as it stores the bearer's principal.
func WithCIToken(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ciTokenKey{}, id)
}

// CITokenFrom returns the id of the CI token of the request of ctx: that of its CredCI, or that
// of its bearer (WithCIToken); "" for a request without one. The audit rows of its writes carry
// it as details.ciToken (docs/spec/rbac.md §11).
func CITokenFrom(ctx context.Context) string {
	if c := CredentialFrom(ctx); c.Kind == CredCI {
		return c.CIToken
	}
	id, _ := ctx.Value(ciTokenKey{}).(string)
	return id
}

// ciLimit is Credential.Limit for a CI token: the actions of the roles of the grants whose mask
// matches repo.
func (c Credential) ciLimit(a Action, format, name string) error {
	var g actionSet
	matched := false
	for _, gr := range c.Grants {
		if gr.Role == RoleAdmin || !MaskMatches(gr.Mask, format, name) {
			continue
		}
		matched = true
		g |= roleActions[gr.Role]
	}
	switch {
	case !matched:
		return fmt.Errorf("%w: repository %s/%s", index.ErrNotFound, format, name)
	case g&actionBit(a) == 0:
		return fmt.Errorf("%w: the CI token's policies do not grant %s here", ErrScope, a)
	}
	return nil
}
