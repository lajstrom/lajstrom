package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/index"
)

// ErrInvalidPrincipal means a string is not a principal in canonical form.
var ErrInvalidPrincipal = errors.New("auth: invalid principal")

// Kind is the kind of a principal (docs/spec/rbac.md §1).
type Kind string

// Principal kinds.
const (
	// KindUser is a local or OIDC user; a personal access token acts as its user.
	KindUser Kind = "user"
	// KindToken is a deploy token, a principal of its own bound to one repo.
	KindToken Kind = "token"
	// KindGroup is a group from the claims of an OIDC login (docs/spec/rbac.md §8): a principal
	// of bindings, never of a request; it reaches a repo through its members.
	KindGroup Kind = "group"
	// KindAnonymous is a caller without credentials.
	KindAnonymous Kind = "anonymous"
)

// Principal is who performs a request. Its canonical string form, used in bindings, sessions,
// the audit log and the sub of a bearer token, is user:<uid>, token:<id>, group:<name> or
// anonymous (docs/spec/rbac.md §1). The zero value is not a valid principal.
type Principal struct {
	Kind Kind
	// Name is a user's uid (index.NewUID), a group's name or the decimal id of a deploy token;
	// empty for anonymous. Where a person names a user, user:<name> parses too, with the name
	// here; LookupUser finds the user either way.
	Name string
}

// UserPrincipal returns the principal of the user u: user:<uid>.
func UserPrincipal(u *index.User) Principal { return Principal{Kind: KindUser, Name: u.UID} }

// LookupUser returns the live user that p names: by uid when p.Name has the form of one
// (index.IsUID), by name otherwise, which at any moment is one live user's. A principal that is
// not a user's, and a deleted user, is index.ErrNotFound.
func LookupUser(ctx context.Context, r index.Reader, p Principal) (*index.User, error) {
	if p.Kind != KindUser {
		return nil, fmt.Errorf("%w: %s is not a user", index.ErrNotFound, p)
	}
	if !index.IsUID(p.Name) {
		return r.GetUser(ctx, p.Name)
	}
	u, err := r.GetUserByUID(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	if u.DeletedAt != nil {
		return nil, fmt.Errorf("%w: user %s is deleted", index.ErrNotFound, p.Name)
	}
	return u, nil
}

// MaxGroupNameLen bounds the name of a group, in bytes (docs/spec/rbac.md §8).
const MaxGroupNameLen = 256

// CheckGroupName returns nil when name is the name of a group by docs/spec/rbac.md §8: 1 to
// MaxGroupNameLen bytes of printable UTF-8, without a space at either end. Groups come from an
// IdP as it spells them (GitHub's org:team, Keycloak's /path/group, an Azure object id); a name
// outside the rule is dropped from a login and refused in a binding. Errors wrap
// ErrInvalidPrincipal and do not quote the name, which may be long.
func CheckGroupName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: a group name must not be empty", ErrInvalidPrincipal)
	case len(name) > MaxGroupNameLen:
		return fmt.Errorf("%w: a group name is at most %d bytes", ErrInvalidPrincipal, MaxGroupNameLen)
	case !utf8.ValidString(name) || strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
		return fmt.Errorf("%w: a group name is printable UTF-8", ErrInvalidPrincipal)
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("%w: a group name has no space at either end", ErrInvalidPrincipal)
	}
	return nil
}

// Anonymous is the principal of requests without credentials.
var Anonymous = Principal{Kind: KindAnonymous}

// String returns the canonical form of p.
func (p Principal) String() string {
	if p.Kind == KindAnonymous {
		return string(KindAnonymous)
	}
	return string(p.Kind) + ":" + p.Name
}

// ParsePrincipal parses the canonical form produced by Principal.String. Errors wrap
// ErrInvalidPrincipal.
func ParsePrincipal(s string) (Principal, error) {
	if s == string(KindAnonymous) {
		return Anonymous, nil
	}
	kind, name, ok := strings.Cut(s, ":")
	if !ok || name == "" {
		return Principal{}, fmt.Errorf("%w: %q: want <kind>:<name> or anonymous", ErrInvalidPrincipal, s)
	}
	switch k := Kind(kind); k {
	case KindUser, KindGroup:
		return Principal{Kind: k, Name: name}, nil
	case KindToken:
		if strings.Trim(name, "0123456789") != "" {
			return Principal{}, fmt.Errorf("%w: %q: token id must be decimal", ErrInvalidPrincipal, s)
		}
		return Principal{Kind: k, Name: name}, nil
	case KindAnonymous:
		return Principal{}, fmt.Errorf("%w: %q: anonymous takes no name", ErrInvalidPrincipal, s)
	default:
		return Principal{}, fmt.Errorf("%w: %q: unknown kind %q", ErrInvalidPrincipal, s, kind)
	}
}
