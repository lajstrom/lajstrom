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
	// KindCert is a client certificate that a configured CA signed (docs/spec/rbac.md §9):
	// cert:spiffe://<trust domain>/<path> or cert:<bundle>/<name>.
	KindCert Kind = "cert"
	// KindKubernetes is a Kubernetes service account, by its token (docs/spec/rbac.md §9):
	// k8s:<cluster>/<namespace>/<service account>.
	KindKubernetes Kind = "k8s"
	// KindCI is a CI job, by the CI token a trust policy minted for its platform's OIDC token
	// (docs/spec/rbac.md §11): ci:<issuer>/<project>. It has no bindings.
	KindCI Kind = "ci"
)

// Principal is who performs a request. Its canonical string form, used in bindings, sessions,
// the audit log and the sub of a bearer token, is user:<uid>, token:<id>, group:<name>,
// anonymous (docs/spec/rbac.md §1), cert:<name> or k8s:<name> (§9), or ci:<name> (§11). The
// zero value is not a valid principal.
type Principal struct {
	Kind Kind
	// Name is a user's uid (index.NewUID), a group's name, the decimal id of a deploy token, the
	// identity of a certificate or a service account (docs/spec/rbac.md §9), or a CI job's
	// <issuer>/<project> (§11); empty for
	// anonymous. Where a person names a user, user:<name> parses too, with the name
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

// PrincipalKind is the principalKind of an audit row's principal (docs/spec/rbac.md §5):
// KindService for the user:<uid> of a service account, live or deleted, and "" for every other
// principal, a user that does not exist among them. Only a user's principal is read from r. The
// answers of GET /api/v1/audit and the log lines of the rows (audit.Lines) both take it from here.
func PrincipalKind(ctx context.Context, r index.Reader, principal string) (string, error) {
	uid, ok := strings.CutPrefix(principal, string(KindUser)+":")
	if !ok || !index.IsUID(uid) {
		return "", nil
	}
	u, err := r.GetUserByUID(ctx, uid)
	switch {
	case err == nil && u.Kind == KindService:
		return KindService, nil
	case err == nil, errors.Is(err, index.ErrNotFound):
		return "", nil
	}
	return "", err
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

// ParsePrincipal parses the canonical form produced by Principal.String; a cert: or k8s:
// principal must keep the grammar of docs/spec/rbac.md §9, and may be a pattern
// (Principal.IsPattern); a ci: principal that of §11, never a pattern. Errors wrap
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
	case KindCert, KindKubernetes:
		if err := checkWorkloadName(k, name); err != nil {
			return Principal{}, fmt.Errorf("%w: %q: %w", ErrInvalidPrincipal, s, err)
		}
		return Principal{Kind: k, Name: name}, nil
	case KindCI:
		if err := checkCIName(name); err != nil {
			return Principal{}, fmt.Errorf("%w: %q: %w", ErrInvalidPrincipal, s, err)
		}
		return Principal{Kind: k, Name: name}, nil
	case KindAnonymous:
		return Principal{}, fmt.Errorf("%w: %q: anonymous takes no name", ErrInvalidPrincipal, s)
	default:
		return Principal{}, fmt.Errorf("%w: %q: unknown kind %q", ErrInvalidPrincipal, s, kind)
	}
}
