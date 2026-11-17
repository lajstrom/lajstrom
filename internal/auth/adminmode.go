package auth

import (
	"context"
	"errors"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The admin mode of docs/spec/rbac.md §12 (auth.admin.requireMFA, decided on #1276): an admin
// binding, a user's own or a group's, counts only for the token of a web UI's sign-in through
// OIDC, sent with a proof of its key, whose sign-in shows a second factor (SignIn.SecondFactor).
// For every other credential the evaluation of §3 leaves the admin bindings out
// (RBACOptions.AdminMFA), and the principal's other bindings count.

// AdminMode is an Authorizer with the admin mode: RBAC.
type AdminMode interface {
	// AdminLeftOut reports whether p has an admin binding, its own or one of a group it is in,
	// that the admin mode leaves out for the credential of ctx: false while the mode is off.
	AdminLeftOut(ctx context.Context, p Principal) (bool, error)
}

// adminRule is what the admin role needs under the admin mode, for the messages of its refusals.
const adminRule = "the admin role counts only for a web UI's sign-in through OIDC with a second factor (auth.admin.requireMFA): "

// AdminMFARefusal is the answer of a route of the admin role to a principal whose admin binding
// the admin mode leaves out for cred (docs/spec/rbac.md §12): the token of a web UI's sign-in
// through OIDC without a second factor is asked for more, a *StepUpError (the API's 401 with
// insufficient_user_authentication, without max_age: the role needs no fresh sign-in); any other
// credential gets an error wrapping ErrScope (403) that names auth.admin.requireMFA and the kind
// of the credential.
func AdminMFARefusal(cred Credential, now time.Time) error {
	err := CheckSignIn(cred, now, Need{SecondFactor: true})
	var stepUp *StepUpError
	switch {
	case errors.As(err, &stepUp):
		return stepUp.withPrefix(adminRule)
	case err == nil:
		return &authError{msg: adminRule + "the request's credential does not reach it", err: ErrScope}
	}
	return &authError{msg: adminRule + err.Error(), err: ErrScope}
}

// AdminBindingsUnderMFA counts, for serve's warnings at start under the admin mode
// (docs/spec/rbac.md §12), the admin bindings that the mode leaves without effect, those of
// service accounts and local users, who never sign in through OIDC, and reports whether a
// request can reach the role at all: whether an enabled OIDC user or a group has an admin
// binding. A binding of a user that no longer exists counts for neither.
func AdminBindingsUnderMFA(ctx context.Context, r index.Reader) (idle int, reachable bool, err error) {
	bindings, err := r.ListBindings(ctx, "")
	if err != nil {
		return 0, false, err
	}
	for _, b := range bindings {
		p, perr := ParsePrincipal(b.Principal)
		switch {
		case b.Role != RoleAdmin || perr != nil:
			continue
		case p.Kind == KindGroup:
			reachable = true
			continue
		}
		u, err := LookupUser(ctx, r, p)
		switch {
		case errors.Is(err, index.ErrNotFound):
		case err != nil:
			return 0, false, err
		case u.Kind == KindOIDC:
			reachable = reachable || !u.Disabled
		default:
			idle++
		}
	}
	return idle, reachable, nil
}
