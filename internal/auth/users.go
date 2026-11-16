package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Kinds of users (users.kind). KindOIDC, a user that its first OIDC login makes, is with the
// login.
const (
	// KindLocal is the kind of a user whose password Lajstrom keeps.
	KindLocal = "local"
	// KindService is the kind of a service account (docs/spec/rbac.md §1): a user for a system,
	// without a password, with an owner, authenticated by its personal access tokens only.
	KindService = "service"
)

// RoleAdmin and MaskAll make the admin binding of docs/spec/rbac.md §3 and §6: everything on
// every repo, plus the system and the audit log.
const (
	RoleAdmin = "admin"
	MaskAll   = "*/*"
)

// Audit actions of the user changes (docs/spec/rbac.md §5).
const (
	ActionUserCreate   = "user.create"
	ActionUserPassword = "user.password"
	ActionUserDisable  = "user.disable"
	ActionUserEnable   = "user.enable"
	ActionUserRename   = "user.rename"
	ActionUserDelete   = "user.delete"
	ActionUserOwner    = "user.owner"
)

// User is a user as the CLI prints it and the users API (#47) answers it. It never carries the
// password hash.
type User struct {
	// UID is the user's identity (docs/spec/rbac.md §1); principals refer to it, user:<uid>.
	UID       string    `json:"uid"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"createdAt"`
	// Owner is a service account's owner, user:<uid> or group:<name>; OwnerName the name of a
	// user owner, and OwnerDisabled whether that user is disabled. All three are empty for every
	// other kind.
	Owner         string `json:"owner,omitempty"`
	OwnerName     string `json:"ownerName,omitempty"`
	OwnerDisabled bool   `json:"ownerDisabled,omitempty"`
	// ID and Revision are the row's, which the API's ETag names (docs/api.md, Revisions); not
	// shown.
	ID       int64 `json:"-"`
	Revision int64 `json:"-"`
}

// UserChange is what Users.Change changes, each field when it is set.
type UserChange struct {
	Disabled *bool
	// Owner is a service account's new owner, as for CreateService (ownerOf).
	Owner *string
	Name  *string
}

// Origin says where a change comes from, for its audit event; the principal is
// PrincipalFrom(ctx).
type Origin struct {
	// Source is cli, api or config; the audit details carry it as "source".
	Source string
	// IP is the client address without the port; empty outside HTTP.
	IP string
	// RequestID is the id of the HTTP request; the audit details carry it as "requestId" when
	// it is set.
	RequestID string
	// Host is the host name of the machine a CLI change ran on; the audit details carry it as
	// "host" when it is set.
	Host string
}

// Users manages the local users of the control plane (docs/spec/index-schema.md, users).
// lajstrom admin user goes through it, and the users API (#47) will, so that the validation and
// the audit events exist once. Every change is one audit event by PrincipalFrom(ctx), with the
// user as its subject. Create it as a struct literal; its methods may be called concurrently.
//
// Errors for the caller wrap ErrInvalid (a name or a password that breaks its rule),
// index.ErrNotFound (no such user) or index.ErrConflict (the name is taken), with a message that
// names the user or the field; any other error is a failure of the index.
type Users struct {
	// Index is the index database; required.
	Index index.Store
	// Audit records the events; nil means each event goes to the index in the transaction of
	// its change.
	Audit audit.Writer
	// Now is the clock of createdAt and of the audit events; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
	// DisableWithOwner disables the enabled service accounts of a user in the transaction that
	// disables the user, their owner: auth.serviceAccounts.onOwnerDisabled disable
	// (docs/spec/rbac.md §1). False flags them only, ownerDisabled. Enabling the owner again
	// enables none of them.
	DisableWithOwner bool
}

// Create checks name and password and creates the local user, with a new uid, and one
// user.create audit event, whose details say whether it is an admin. With admin, the binding
// (user:<uid>, admin, */*) is created in the same transaction: the bootstrap of
// docs/spec/rbac.md §6.
func (s *Users) Create(ctx context.Context, name, password string, admin bool, o Origin) (*User, error) {
	return s.create(ctx, name, password, admin, o, nil)
}

// create is Create, with before, when it is not nil, run first in the transaction: an error of
// it rolls the transaction back and is returned as it is.
func (s *Users) create(ctx context.Context, name, password string, admin bool, o Origin, before func(index.Writer) error) (*User, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	row := index.User{Name: name, PasswordHash: hash, Kind: KindLocal, CreatedAt: s.now()}
	var ev index.AuditEvent
	err = s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if before != nil {
			if err = before(w); err != nil {
				return err
			}
		}
		if err = w.CreateUser(ctx, &row); err != nil {
			return err
		}
		if admin {
			b := index.Binding{Principal: UserPrincipal(&row).String(), Role: RoleAdmin, RepoMask: MaskAll, CreatedAt: row.CreatedAt}
			if err = w.CreateBinding(ctx, &b); err != nil {
				return err
			}
		}
		ev, err = s.record(ctx, w, ActionUserCreate, &row, o, map[string]any{"admin": admin})
		return err
	})
	if err != nil {
		return nil, userError("create", name, err)
	}
	s.writeAudit(ctx, ev)
	return s.resource(ctx, s.Index, &row, nil)
}

// CreateService checks name and owner and creates the service account (docs/spec/rbac.md §1),
// with a new uid, no password and one user.create audit event whose details have its kind and
// owner. owner is user:<name> or user:<uid> of a live user that is not a service account, kept as
// user:<uid>, or group:<name> with a name of CheckGroupName (ownerOf).
func (s *Users) CreateService(ctx context.Context, name, owner string, o Origin) (*User, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	row := index.User{Name: name, Kind: KindService, CreatedAt: s.now()}
	var ev index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if row.Owner, err = ownerOf(ctx, w, owner); err != nil {
			return err
		}
		if err = w.CreateUser(ctx, &row); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, ActionUserCreate, &row, o, map[string]any{"kind": KindService, "owner": row.Owner})
		return err
	})
	if err != nil {
		return nil, userError("create", name, err)
	}
	s.writeAudit(ctx, ev)
	return s.resource(ctx, s.Index, &row, nil)
}

// SetOwner makes owner, as for CreateService, the owner of the service account called name,
// with one user.owner audit event whose details have the owner it had (from) and the new one. An
// owner it has already is left as it is, without an event. A user that is not a service account
// has no owner (ErrInvalid).
func (s *Users) SetOwner(ctx context.Context, name, owner string, o Origin) error {
	var ev *index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		row, err := w.GetUser(ctx, name)
		if err != nil {
			return err
		}
		if row.Kind != KindService {
			return invalidf("owner: user %s is not a service account, and only a service account has an owner", name)
		}
		to, err := ownerOf(ctx, w, owner)
		if err != nil || to == row.Owner {
			return err
		}
		from := row.Owner
		row.Owner = to
		if err := w.UpdateUser(ctx, row); err != nil {
			return err
		}
		e, err := s.record(ctx, w, ActionUserOwner, row, o, map[string]any{"from": from, "owner": to})
		ev = &e
		return err
	})
	if err != nil {
		return userError(ActionUserOwner, name, err)
	}
	if ev != nil {
		s.writeAudit(ctx, *ev)
	}
	return nil
}

// ownerOf returns the owner of a service account that owner names, as it is kept: user:<uid> of
// the live user of user:<name> or user:<uid>, which must not be a service account, or
// group:<name> with a name of CheckGroupName, which need not have members. Errors wrap
// ErrInvalid and name the field owner.
func ownerOf(ctx context.Context, r index.Reader, owner string) (string, error) {
	p, err := ParsePrincipal(owner)
	if err != nil {
		return "", invalidf("owner: %s", err.Error())
	}
	switch p.Kind {
	case KindGroup:
		if err := CheckGroupName(p.Name); err != nil {
			return "", invalidf("owner: %s", err.Error())
		}
		return p.String(), nil
	case KindUser:
		u, err := LookupUser(ctx, r, p)
		switch {
		case errors.Is(err, index.ErrNotFound):
			return "", invalidf("owner: user %s does not exist", p.Name)
		case err != nil:
			return "", err
		case u.Kind == KindService:
			return "", invalidf("owner: %s is a service account, which owns nothing: name a person or a group", u.Name)
		}
		return UserPrincipal(u).String(), nil
	case KindToken, KindAnonymous, KindCert, KindKubernetes, KindCI:
	}
	return "", invalidf("owner: %q is neither user:<name> nor group:<name>", owner)
}

// Get returns the user called name.
func (s *Users) Get(ctx context.Context, name string) (*User, error) {
	row, err := s.Index.GetUser(ctx, name)
	if err != nil {
		return nil, userError("get", name, err)
	}
	return s.resource(ctx, s.Index, row, nil)
}

// List returns the users of kind, local, oidc or service, or every user when kind is "", ordered
// by name. Any other kind is ErrInvalid.
func (s *Users) List(ctx context.Context, kind string) ([]User, error) {
	switch kind {
	case "", KindLocal, KindOIDC, KindService:
	default:
		return nil, invalidf("kind: %q is not local, oidc or service", kind)
	}
	rows, err := s.Index.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	byUID := make(map[string]*index.User, len(rows))
	for i := range rows {
		byUID[rows[i].UID] = &rows[i]
	}
	users := make([]User, 0, len(rows))
	for i := range rows {
		if kind != "" && rows[i].Kind != kind {
			continue
		}
		u, err := s.resource(ctx, s.Index, &rows[i], byUID)
		if err != nil {
			return nil, fmt.Errorf("auth: list users: %w", err)
		}
		users = append(users, *u)
	}
	return users, nil
}

// SetPassword checks password and makes it the user's, with one user.password audit event. The
// old password stops working at the next request (the Authenticator compares the stored hash).
func (s *Users) SetPassword(ctx context.Context, name, password string, o Origin) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.update(ctx, name, ActionUserPassword, o, func(u *index.User) (bool, error) {
		if u.Kind == KindService {
			return false, invalidf("password: user %s is a service account, which has no password: it authenticates with its tokens", name)
		}
		u.PasswordHash = hash
		return true, nil
	})
}

// Disable disables the user, with one user.disable audit event: its credentials are refused
// from the next request on. A user that is disabled already is left as it is, without an event.
// The last enabled user with an admin binding is not disabled (index.ErrConflict). With
// DisableWithOwner, the enabled service accounts the user owns are disabled in the same
// transaction, each with a user.disable event of its own (disableOwned).
func (s *Users) Disable(ctx context.Context, name string, o Origin) error {
	return s.update(ctx, name, ActionUserDisable, o, func(u *index.User) (bool, error) {
		changed := !u.Disabled
		u.Disabled = true
		return changed, nil
	})
}

// Rename gives the user called name the name to, with one user.rename audit event: its uid,
// and so its bindings, tokens and sessions, stay. A name another live user has is
// index.ErrConflict.
func (s *Users) Rename(ctx context.Context, name, to string, o Origin) error {
	if err := CheckName(to); err != nil {
		return err
	}
	err := s.update(ctx, name, ActionUserRename, o, func(u *index.User) (bool, error) {
		changed := u.Name != to
		u.Name = to
		return changed, nil
	})
	if errors.Is(err, index.ErrConflict) {
		return &authError{msg: fmt.Sprintf("user %s already exists", to), err: err}
	}
	return err
}

// Delete deletes the user called name (docs/spec/rbac.md §1), with one user.delete audit event:
// in one transaction it revokes the user's live tokens, deletes its bindings, disables it and
// marks it deleted, its name becoming ~<uid> so that the name is free, the row kept so that its
// uid resolves to who it was. The last enabled user with an admin binding is not deleted, and
// neither is the owner of a live service account (index.ErrConflict). A deletion is not undone.
// The condition of ctx (index.WithCondition) is checked on the row the transaction read.
func (s *Users) Delete(ctx context.Context, name string, o Origin) error {
	var ev index.AuditEvent
	err := s.Index.Tx(index.ConditionContext(ctx), func(w index.Writer) error {
		u, err := w.GetUser(ctx, name)
		if err != nil {
			return err
		}
		if err := index.CheckCondition(ctx, u.ID, u.Revision); err != nil {
			return err
		}
		if err := ownsNone(ctx, w, u); err != nil {
			return err
		}
		now := s.now()
		revoked, err := revokeAll(ctx, w, u, now)
		if err != nil {
			return err
		}
		bindings, err := w.ListBindings(ctx, UserPrincipal(u).String())
		if err != nil {
			return err
		}
		admin := false
		for _, b := range bindings {
			admin = admin || b.Role == RoleAdmin
			if err := w.DeleteBinding(ctx, b.ID); err != nil {
				return err
			}
		}
		u.Disabled, u.DeletedAt, u.DeletedName, u.Name = true, &now, u.Name, "~"+u.UID
		if err := w.UpdateUser(ctx, u); err != nil {
			return err
		}
		if admin && u.Kind != KindService {
			if err := adminsLeft(ctx, w, u.DeletedName); err != nil {
				return err
			}
		}
		ev, err = s.record(ctx, w, ActionUserDelete, u, o, map[string]any{"tokensRevoked": revoked, "bindingsDeleted": len(bindings)})
		return err
	})
	if err != nil {
		return userError("delete", name, err)
	}
	s.writeAudit(ctx, ev)
	return nil
}

// Enable enables a disabled user, with one user.enable audit event. A user that is enabled
// already is left as it is, without an event.
func (s *Users) Enable(ctx context.Context, name string, o Origin) error {
	return s.update(ctx, name, ActionUserEnable, o, func(u *index.User) (bool, error) {
		changed := u.Disabled
		u.Disabled = false
		return changed, nil
	})
}

// Change applies ch to the user called name in one transaction, Disabled first, then a service
// account's Owner, then Name, and returns the user as it is then: the PATCH of
// /api/v1/users/{user}. Each change is an audit event of its own, user.disable or user.enable,
// under the old name, then user.owner, then user.rename, and a value the user has already is
// none. A disabling that leaves no enabled user with an admin binding (a service account is not
// such a user: anyAdmin) and a name another live user has are index.ErrConflict, an owner of a
// user that is not a service account is ErrInvalid, and nothing changes. A disabling with
// DisableWithOwner disables the enabled service accounts the user owns too, each with a
// user.disable event after the user's (disableOwned). The condition of ctx (index.WithCondition)
// is checked on the row the transaction read.
func (s *Users) Change(ctx context.Context, name string, ch UserChange, o Origin) (*User, error) {
	var (
		row *index.User
		evs []index.AuditEvent
	)
	err := s.Index.Tx(index.ConditionContext(ctx), func(w index.Writer) (err error) {
		evs = nil
		if row, err = w.GetUser(ctx, name); err != nil {
			return err
		}
		if err := index.CheckCondition(ctx, row.ID, row.Revision); err != nil {
			return err
		}
		changes, err := applyChange(ctx, w, row, ch)
		if err != nil || len(changes) == 0 {
			return err
		}
		if err := w.UpdateUser(ctx, row); err != nil {
			if errors.Is(err, index.ErrConflict) {
				return &authError{msg: fmt.Sprintf("user %s already exists", row.Name), err: err}
			}
			return err
		}
		disabling := changes[0].action == ActionUserDisable && row.Kind != KindService
		if disabling {
			if err := keepAdmin(ctx, w, row, name); err != nil {
				return err
			}
		}
		evs, err = s.recordChanges(ctx, w, row, changes, disabling, o)
		return err
	})
	if err != nil {
		return nil, userError("change", name, err)
	}
	for _, ev := range evs {
		s.writeAudit(ctx, ev)
	}
	return s.resource(ctx, s.Index, row, nil)
}

// recordChanges records on w the events of changes, in order, of the user row; after a
// disabling's, with DisableWithOwner, the events of the service accounts it disables
// (disableOwned).
func (s *Users) recordChanges(ctx context.Context, w index.Writer, row *index.User, changes []userEvent, disabling bool, o Origin) ([]index.AuditEvent, error) {
	var evs []index.AuditEvent
	for i, c := range changes {
		ev, err := s.record(ctx, w, c.action, &c.user, o, c.details)
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
		if i == 0 && disabling && s.DisableWithOwner {
			owned, err := s.disableOwned(ctx, w, row, o)
			if err != nil {
				return nil, err
			}
			evs = append(evs, owned...)
		}
	}
	return evs, nil
}

// userEvent is an audit event of Users.Change: its action, the user as the change left it, and
// its details.
type userEvent struct {
	action  string
	user    index.User
	details map[string]any
}

// applyChange applies ch to row, Disabled first, then Owner, then Name, and returns the event of
// each change, in order; a value row has already is none. A name that breaks its rule, an owner
// that ownerOf refuses on r, and an owner of a user that is not a service account wrap
// ErrInvalid.
func applyChange(ctx context.Context, r index.Reader, row *index.User, ch UserChange) ([]userEvent, error) {
	var changes []userEvent
	if ch.Disabled != nil && *ch.Disabled != row.Disabled {
		row.Disabled = *ch.Disabled
		action := ActionUserEnable
		if row.Disabled {
			action = ActionUserDisable
		}
		changes = append(changes, userEvent{action, *row, map[string]any{}})
	}
	if ch.Owner != nil {
		if row.Kind != KindService {
			return nil, invalidf("owner: user %s is not a service account, and only a service account has an owner", row.Name)
		}
		to, err := ownerOf(ctx, r, *ch.Owner)
		if err != nil {
			return nil, err
		}
		if to != row.Owner {
			from := row.Owner
			row.Owner = to
			changes = append(changes, userEvent{ActionUserOwner, *row, map[string]any{"from": from, "owner": to}})
		}
	}
	if ch.Name != nil && *ch.Name != row.Name {
		if err := CheckName(*ch.Name); err != nil {
			return nil, err
		}
		from := row.Name
		row.Name = *ch.Name
		changes = append(changes, userEvent{ActionUserRename, *row, map[string]any{"from": from}})
	}
	return changes, nil
}

// keepAdmin returns the error of adminsLeft, about the user called name, when u, just disabled
// on w, has an admin binding and no enabled user with one is left.
func keepAdmin(ctx context.Context, w index.Writer, u *index.User, name string) error {
	admin, err := hasAdminBinding(ctx, w, u)
	if err != nil || !admin {
		return err
	}
	return adminsLeft(ctx, w, name)
}

// update applies change to the user called name in one transaction with one audit event of
// action, when change reports that it changed something; an error of change is returned as it
// is. A change that leaves no enabled user with an admin binding is index.ErrConflict; a service
// account is not such a user (anyAdmin). A disabling with DisableWithOwner adds the events of
// the service accounts it disables (updated). The condition of ctx (index.WithCondition) is
// checked on the row the transaction read.
func (s *Users) update(ctx context.Context, name, action string, o Origin, change func(*index.User) (bool, error)) error {
	var evs []index.AuditEvent
	err := s.Index.Tx(index.ConditionContext(ctx), func(w index.Writer) error {
		evs = nil
		row, err := w.GetUser(ctx, name)
		if err != nil {
			return err
		}
		if err := index.CheckCondition(ctx, row.ID, row.Revision); err != nil {
			return err
		}
		if changed, err := change(row); err != nil || !changed {
			return err
		}
		if err := w.UpdateUser(ctx, row); err != nil {
			return err
		}
		evs, err = s.updated(ctx, w, name, action, row, o)
		return err
	})
	if err != nil {
		return userError(action, name, err)
	}
	for _, ev := range evs {
		s.writeAudit(ctx, ev)
	}
	return nil
}

// updated finishes on w the change of action that update wrote to row, the user called name
// before it: it refuses a disabling that leaves no enabled user with an admin binding, records
// the change's audit event, and with DisableWithOwner disables the service accounts that a
// disabled row owns (disableOwned). It returns the events, the change's first.
func (s *Users) updated(ctx context.Context, w index.Writer, name, action string, row *index.User, o Origin) ([]index.AuditEvent, error) {
	disabling := action == ActionUserDisable && row.Kind != KindService
	if disabling {
		admin, err := hasAdminBinding(ctx, w, row)
		if err != nil {
			return nil, err
		}
		if admin {
			if err := adminsLeft(ctx, w, name); err != nil {
				return nil, err
			}
		}
	}
	details := map[string]any{}
	if action == ActionUserRename {
		details["from"] = name
	}
	ev, err := s.record(ctx, w, action, row, o, details)
	if err != nil || !disabling || !s.DisableWithOwner {
		return []index.AuditEvent{ev}, err
	}
	owned, err := s.disableOwned(ctx, w, row, o)
	return append([]index.AuditEvent{ev}, owned...), err
}

// disableOwned disables the enabled service accounts whose owner is the user u, on w, and
// returns their user.disable events, each with u, user:<uid>, as owner in its details
// (docs/spec/rbac.md §1, §5). A deleted service account is disabled already.
func (s *Users) disableOwned(ctx context.Context, w index.Writer, u *index.User, o Origin) ([]index.AuditEvent, error) {
	rows, err := w.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	owner := UserPrincipal(u).String()
	var evs []index.AuditEvent
	for i := range rows {
		a := &rows[i]
		if a.Kind != KindService || a.Owner != owner || a.Disabled {
			continue
		}
		a.Disabled = true
		if err := w.UpdateUser(ctx, a); err != nil {
			return nil, err
		}
		ev, err := s.record(ctx, w, ActionUserDisable, a, o, map[string]any{"owner": owner})
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// record builds the audit event of a change of the user u, its subject user:<uid> and its name in
// the details (the name it had, for a deleted user), with details and the origin, and appends it
// on w, in the transaction of the change, when there is no audit.Writer. The caller passes the
// event to writeAudit after the commit.
func (s *Users) record(ctx context.Context, w index.Writer, action string, u *index.User, o Origin, details map[string]any) (index.AuditEvent, error) {
	details["name"] = u.Name
	if u.DeletedAt != nil {
		details["name"] = u.DeletedName
	}
	return recordAudit(ctx, w, s.Audit, index.AuditEvent{At: s.now(), Action: action, Subject: UserPrincipal(u).String()}, o, details)
}

// hasAdminBinding reports whether u has an admin binding.
func hasAdminBinding(ctx context.Context, r index.Reader, u *index.User) (bool, error) {
	bindings, err := r.ListBindings(ctx, UserPrincipal(u).String())
	if err != nil {
		return false, err
	}
	for _, b := range bindings {
		if b.Role == RoleAdmin {
			return true, nil
		}
	}
	return false, nil
}

// adminsLeft returns an error wrapping index.ErrConflict, about the user called name, when no
// enabled user with an admin binding is left on w: nobody could manage the server over HTTP.
func adminsLeft(ctx context.Context, w index.Writer, name string) error {
	ok, err := anyAdmin(ctx, w)
	if err != nil || ok {
		return err
	}
	return &authError{
		msg: fmt.Sprintf("user %s is the last enabled user with an admin binding: create another admin first "+
			"(lajstrom admin user create --admin, or admin binding create)", name),
		err: index.ErrConflict,
	}
}

// anyAdmin reports whether an enabled, live user that is not a service account has an admin
// binding on r. A binding whose principal is no live user grants nothing; a service account
// cannot make a token once its own expire (docs/spec/rbac.md §1), so it does not count.
func anyAdmin(ctx context.Context, r index.Reader) (bool, error) {
	bindings, err := r.ListBindings(ctx, "")
	if err != nil {
		return false, err
	}
	for _, b := range bindings {
		if b.Role != RoleAdmin {
			continue
		}
		p, perr := ParsePrincipal(b.Principal)
		if perr != nil {
			continue
		}
		u, err := LookupUser(ctx, r, p)
		switch {
		case errors.Is(err, index.ErrNotFound):
			continue
		case err != nil:
			return false, err
		}
		if !u.Disabled && u.Kind != KindService {
			return true, nil
		}
	}
	return false, nil
}

// ownsNone returns an error wrapping index.ErrConflict, naming them, when u owns a live service
// account: the owner of a service account is a live user or a group (docs/spec/rbac.md §1).
func ownsNone(ctx context.Context, r index.Reader, u *index.User) error {
	if u.Kind == KindService {
		return nil
	}
	rows, err := r.ListUsers(ctx)
	if err != nil {
		return err
	}
	owner := UserPrincipal(u).String()
	var owned []string
	for _, a := range rows {
		if a.Kind == KindService && a.Owner == owner {
			owned = append(owned, a.Name)
		}
	}
	if len(owned) == 0 {
		return nil
	}
	return &authError{
		msg: fmt.Sprintf("user %s owns the service accounts %s: give them another owner, or delete them, first",
			u.Name, strings.Join(owned, ", ")),
		err: index.ErrConflict,
	}
}

// revokeAll revokes the live tokens of the user u at now, on w, and returns how many it revoked.
func revokeAll(ctx context.Context, w index.Writer, u *index.User, now time.Time) (int, error) {
	tokens, err := w.ListTokens(ctx, &u.ID, nil)
	if err != nil {
		return 0, err
	}
	revoked := 0
	for _, t := range tokens {
		if t.RevokedAt == nil {
			if err := w.RevokeToken(ctx, t.ID, now); err != nil {
				return 0, err
			}
			revoked++
		}
	}
	return revoked, nil
}

// writeAudit passes ev to the audit.Writer, after the commit of its change (writeAuditEvent).
func (s *Users) writeAudit(ctx context.Context, ev index.AuditEvent) {
	writeAuditEvent(ctx, s.Audit, s.log(), ev, "auth: audit event of a user change not written", "user")
}

// recordAudit completes ev, an event of a change with its time, action, subject and repo: the
// principal is PrincipalFrom(ctx), the IP the origin's, the details details with the origin's
// source and request id. It appends ev on w, in the transaction of the change, when aw is nil;
// the caller passes the event to writeAuditEvent after the commit.
func recordAudit(ctx context.Context, w index.Writer, aw audit.Writer, ev index.AuditEvent, o Origin, details map[string]any) (index.AuditEvent, error) {
	if o.Source != "" {
		details["source"] = o.Source
	}
	if o.RequestID != "" {
		details["requestId"] = o.RequestID
	}
	if o.Host != "" {
		details["host"] = o.Host
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return index.AuditEvent{}, fmt.Errorf("auth: audit details of %s: %w", ev.Action, err)
	}
	ev.Principal, ev.IP, ev.Details = PrincipalFrom(ctx).String(), o.IP, raw
	if aw == nil {
		if err := w.AppendAudit(ctx, &ev); err != nil {
			return index.AuditEvent{}, err
		}
	}
	return ev, nil
}

// writeAuditEvent passes ev to aw, after the commit of its change; nothing when aw is nil. A
// failure is logged with msg and the subject under the key subject: the change is made, and is
// not undone for its event.
func writeAuditEvent(ctx context.Context, aw audit.Writer, log *slog.Logger, ev index.AuditEvent, msg, subject string) {
	if aw == nil {
		return
	}
	if err := aw.Write(ctx, ev); err != nil {
		log.ErrorContext(ctx, msg, "action", ev.Action, subject, ev.Subject, "error", err.Error())
	}
}

// userError turns an error about the user called name into the one the callers answer:
// index.ErrNotFound and index.ErrConflict get a message for the person, anything else is
// wrapped with op, for the log.
func userError(op, name string, err error) error {
	var ae *authError
	switch {
	case errors.As(err, &ae):
		return err
	case errors.Is(err, index.ErrNotFound):
		return &authError{msg: fmt.Sprintf("user %s does not exist", name), err: err}
	case errors.Is(err, index.ErrConflict):
		return &authError{msg: fmt.Sprintf("user %s already exists", name), err: err}
	}
	return fmt.Errorf("auth: %s user %s: %w", op, name, err)
}

// resource returns the User of a users row, with a UTC time, and for a service account its
// owner, with the name of a user owner and whether it is disabled: from byUID, the users by their
// uid, when it has the owner, else read on r.
func (s *Users) resource(ctx context.Context, r index.Reader, u *index.User, byUID map[string]*index.User) (*User, error) {
	out := &User{UID: u.UID, Name: u.Name, Kind: u.Kind, Disabled: u.Disabled, CreatedAt: u.CreatedAt.UTC(), Owner: u.Owner,
		ID: u.ID, Revision: u.Revision}
	p, err := ParsePrincipal(u.Owner)
	if err != nil || p.Kind != KindUser {
		return out, nil //nolint:nilerr // no owner, or a group: no name of a user
	}
	owner := byUID[p.Name]
	if owner == nil {
		switch owner, err = r.GetUserByUID(ctx, p.Name); {
		case errors.Is(err, index.ErrNotFound):
			return out, nil
		case err != nil:
			return nil, err
		}
	}
	out.OwnerName, out.OwnerDisabled = owner.Name, owner.Disabled
	if owner.DeletedAt != nil {
		out.OwnerName, out.OwnerDisabled = owner.DeletedName, true
	}
	return out, nil
}

func (s *Users) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Users) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}
