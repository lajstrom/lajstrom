package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth/dpop"
	"github.com/lajstrom/lajstrom/internal/index"
)

// KindOIDC is the kind of a user made by its first OIDC login, without a password
// (docs/spec/rbac.md §8).
const KindOIDC = "oidc"

// ActionUserLogin is the audit action of an OIDC login (docs/spec/rbac.md §8).
const ActionUserLogin = "user.login"

// SourceOIDC is the source of the audit events of an OIDC login.
const SourceOIDC = "oidc"

// ErrLoginRefused means an OIDC login was refused for who it is: a user name claim that is
// missing or not a name, a name a live user has already, a disabled or inactive user. The message says
// which, for the person, and names no secret.
var ErrLoginRefused = errors.New("auth: login refused")

// OIDCLogin is a login whose ID token verified (internal/auth/oidc): its identity, its user name
// claim, its groups, and the lifetime of its token, which its groups count for.
type OIDCLogin struct {
	Issuer, Subject string
	// Username is the claim auth.oidc.usernameClaim; it names the user of a first login only.
	Username string
	// Groups are the names of the groups claim (CheckGroupName).
	Groups []string
	// Lifetime is auth.oidc.tokenLifetime.
	Lifetime time.Duration
}

// LoginOIDC records the verified login l (docs/spec/rbac.md §8) in one transaction and returns
// its user: the identity's user, or, at its first login, a new user of kind oidc named by
// l.Username, with its identity; then the identity's groups replaced, counting until l.Lifetime
// from now, which moves auth_version. The audit rows are user.create for a new user and
// user.login, both by the user, with the source oidc, the issuer, the subject and the groups.
//
// The login is the user's last activity, recorded in its transaction (docs/spec/rbac.md §1,
// Inactive users). The login of a user inactive beyond InactiveAfter disables it
// (DisableInactive) and is refused, without a user.login row, unless the user is the last
// enabled one with an admin binding, which logs in as any.
//
// An identity whose user was deleted is let go, and the login makes a new user. A missing or
// malformed name, a name a live user has, a disabled user and an inactive one are errors
// wrapping ErrLoginRefused; any other error is a failure of the index.
func (s *Users) LoginOIDC(ctx context.Context, l OIDCLogin, o Origin) (*index.User, error) {
	u, err := s.loginOIDC(ctx, l, o, false)
	var in *inactiveError
	if !errors.As(err, &in) {
		return u, err
	}
	refused, err := s.DisableInactive(ctx, in.user.ID, o)
	if err != nil {
		s.log().WarnContext(ctx, "an inactive user's OIDC login is refused, and the user is not disabled",
			"user", UserPrincipal(in.user).String(), "error", err.Error())
	}
	if refused {
		return nil, &authError{msg: fmt.Sprintf("the user %s has had no activity for more than %s, and is disabled: an admin "+
			"can enable it", in.user.Name, shortDuration(s.InactiveAfter)), err: ErrLoginRefused}
	}
	return s.loginOIDC(ctx, l, o, true)
}

// inactiveError is the rollback of the login of an inactive user, which LoginOIDC disables in a
// transaction of its own.
type inactiveError struct{ user *index.User }

func (e *inactiveError) Error() string { return "auth: the user " + e.user.Name + " is inactive" }

// loginOIDC is LoginOIDC's transaction; a user inactive beyond InactiveAfter is an
// *inactiveError, unless inactive is true: the last enabled admin, which DisableInactive kept.
func (s *Users) loginOIDC(ctx context.Context, l OIDCLogin, o Origin, inactive bool) (*index.User, error) {
	o.Source = SourceOIDC
	now := s.now()
	var (
		u      *index.User
		events []index.AuditEvent
	)
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		events = events[:0]
		id, created, err := s.identityUser(ctx, w, l, now, &u)
		if err != nil {
			return err
		}
		if !inactive && Inactive(u, s.InactiveAfter, now) {
			return &inactiveError{user: u}
		}
		if err := w.TouchUser(ctx, u.ID, now); err != nil {
			return err
		}
		u.LastSeenAt = &now
		uctx := WithPrincipal(ctx, UserPrincipal(u))
		if created {
			ev, err := s.record(uctx, w, ActionUserCreate, u, o,
				map[string]any{"admin": false, "issuer": l.Issuer, "subject": l.Subject})
			if err != nil {
				return err
			}
			events = append(events, ev)
		}
		if err := w.SetIdentityGroups(ctx, id.ID, l.Groups, now, now.Add(l.Lifetime)); err != nil {
			return err
		}
		ev, err := s.record(uctx, w, ActionUserLogin, u, o,
			map[string]any{"issuer": l.Issuer, "subject": l.Subject, "groups": l.Groups})
		events = append(events, ev)
		return err
	})
	if err != nil {
		var (
			ae *authError
			ie *inactiveError
		)
		if errors.As(err, &ae) || errors.As(err, &ie) {
			return nil, err
		}
		return nil, fmt.Errorf("auth: oidc login of %s at %s: %w", l.Subject, l.Issuer, err)
	}
	for _, ev := range events {
		s.writeAudit(ctx, ev)
	}
	return u, nil
}

// identityUser finds the user of l's identity on w, or creates both, and stores the user in *u;
// created is true for a new user.
func (s *Users) identityUser(ctx context.Context, w index.Writer, l OIDCLogin, now time.Time, u **index.User) (*index.Identity, bool, error) {
	id, err := w.GetIdentity(ctx, l.Issuer, l.Subject)
	switch {
	case errors.Is(err, index.ErrNotFound):
		id = nil
	case err != nil:
		return nil, false, err
	}
	if id != nil {
		user, err := w.GetUserByID(ctx, id.UserID)
		switch {
		case err != nil && !errors.Is(err, index.ErrNotFound):
			return nil, false, err
		case err == nil && user.DeletedAt == nil:
			if user.Disabled {
				return nil, false, &authError{msg: fmt.Sprintf("the user %s is disabled", user.Name), err: ErrLoginRefused}
			}
			*u = user
			return id, false, nil
		}
		if err := w.DeleteIdentity(ctx, id.ID); err != nil { // its user is deleted: a new user
			return nil, false, err
		}
	}
	if err := CheckName(l.Username); err != nil {
		return nil, false, &authError{msg: fmt.Sprintf("the user name claim is missing or not a user name: %s", err), err: ErrLoginRefused}
	}
	switch _, err := w.GetUser(ctx, l.Username); {
	case err == nil:
		return nil, false, &authError{msg: fmt.Sprintf("the name %s is taken by another user: an admin renames or deletes it "+
			"(an identity is never joined to a user by its name)", l.Username), err: ErrLoginRefused}
	case !errors.Is(err, index.ErrNotFound):
		return nil, false, err
	}
	user := index.User{Name: l.Username, Kind: KindOIDC, CreatedAt: now}
	if err := w.CreateUser(ctx, &user); err != nil {
		return nil, false, err
	}
	id = &index.Identity{UserID: user.ID, Issuer: l.Issuer, Subject: l.Subject, CreatedAt: now, LoginAt: now}
	if err := w.CreateIdentity(ctx, id); err != nil {
		return nil, false, err
	}
	*u = &user
	return id, true, nil
}

// LoginToken mints the personal access token of an OIDC login for the user u
// (docs/spec/rbac.md §8): without scopes, expiring after lifetime, named oidc <time> <4 hex>, with
// its token.create event by the user and createdBy oidc. It returns the token and its secret.
func (s *Tokens) LoginToken(ctx context.Context, u *index.User, lifetime time.Duration, o Origin) (*Token, string, error) {
	return s.loginToken(ctx, u, lifetime, "", nil, o)
}

// BoundLoginToken mints the token of a web UI's OIDC sign-in (docs/spec/rbac.md §10): the token of
// LoginToken, bound to the DPoP key whose JWK SHA-256 thumbprint is jkt (CreateBoundPAT), which
// keeps in, the sign-in at the IdP of the login's ID token, when it is not nil.
func (s *Tokens) BoundLoginToken(ctx context.Context, u *index.User, lifetime time.Duration, jkt string, in *SignIn,
	o Origin) (*Token, string, error) {
	if !dpop.ValidThumbprint(jkt) {
		return nil, "", invalidf("dpopJkt: must be a JWK SHA-256 thumbprint, 43 characters of base64url")
	}
	return s.loginToken(ctx, u, lifetime, jkt, in, o)
}

// loginToken is LoginToken, the token bound to the DPoP key of the thumbprint jkt unless it is "",
// with the sign-in in.
func (s *Tokens) loginToken(ctx context.Context, u *index.User, lifetime time.Duration, jkt string, in *SignIn, o Origin) (*Token, string, error) {
	o.Source = SourceOIDC
	b := make([]byte, 2)
	_, _ = rand.Read(b) // crypto/rand.Read does not return errors; it crashes the program instead
	name := "oidc " + s.now().Format(time.RFC3339) + " " + hex.EncodeToString(b)
	ctx = WithCredential(WithPrincipal(ctx, UserPrincipal(u)), Credential{Kind: CredOIDC})
	return s.createPAT(ctx, u.Name, name, nil, binding{jkt: jkt, signIn: in}, Expiry{In: lifetime}, o)
}
