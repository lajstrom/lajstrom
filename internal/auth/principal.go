package auth

import (
	"errors"
	"fmt"
	"strings"
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
	// KindGroup is a group from OIDC claims (after the MVP).
	KindGroup Kind = "group"
	// KindAnonymous is a caller without credentials.
	KindAnonymous Kind = "anonymous"
)

// Principal is who performs a request. Its canonical string form, used in bindings, sessions
// and the audit log, is user:<name>, token:<id>, group:<name> or anonymous. The zero value is
// not a valid principal.
type Principal struct {
	Kind Kind
	// Name is the user or group name, or the decimal id of a deploy token; empty for anonymous.
	Name string
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
