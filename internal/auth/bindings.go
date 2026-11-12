package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Audit actions of the binding changes (docs/spec/rbac.md §5).
const (
	ActionBindingCreate = "binding.create"
	ActionBindingDelete = "binding.delete"
)

// Binding is a binding as the CLI prints it and the bindings API (#47) answers it.
type Binding struct {
	ID int64 `json:"id"`
	// Principal is user:<uid> or group:<name>; Name is a user's name now (the name it had, for a
	// user since deleted), for people, and empty for a group.
	Principal string    `json:"principal"`
	Name      string    `json:"name,omitempty"`
	Role      string    `json:"role"`
	RepoMask  string    `json:"repoMask"`
	CreatedAt time.Time `json:"createdAt"`
}

// Bindings manages the bindings of the control plane (docs/spec/rbac.md §3). lajstrom admin
// binding goes through it, and the bindings API (#47) will, so that the rules and the audit
// events exist once. Every change is one audit event by PrincipalFrom(ctx), with the binding's
// principal as its subject. Create it as a struct literal; its methods may be called
// concurrently.
//
// Errors for the caller wrap ErrInvalid (a field that breaks its rule; the message names it),
// index.ErrNotFound (no such binding) or index.ErrConflict (the same binding exists, or the last
// admin binding of an enabled user would go); any other error is a failure of the index.
type Bindings struct {
	// Index is the index database; required.
	Index index.Store
	// Formats reports whether a format of that name is served (a compiled-in plugin, or oci):
	// the format of a mask is one of them or "*". nil accepts every name.
	Formats func(name string) bool
	// Audit records the events; nil means each event goes to the index in the transaction of
	// its change.
	Audit audit.Writer
	// Now is the clock of createdAt and of the audit events; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// Create checks the binding (principal, role, mask) by docs/spec/rbac.md §3 and creates it,
// with one binding.create audit event. principal is user:<name> or user:<uid> of a live user,
// stored as its user:<uid>, or group:<name> with a name of CheckGroupName, which need not have
// members (docs/spec/rbac.md §8). It counts from the next request on, on every replica.
func (s *Bindings) Create(ctx context.Context, principal, role, mask string, o Origin) (*Binding, error) {
	p, err := s.check(principal, role, mask)
	if err != nil {
		return nil, err
	}
	row := index.Binding{Role: role, RepoMask: mask, CreatedAt: s.now()}
	name := ""
	var ev index.AuditEvent
	err = s.Index.Tx(ctx, func(w index.Writer) (err error) {
		row.Principal = p.String() // a group need not have members (docs/spec/rbac.md §3)
		if p.Kind == KindUser {
			u, err := LookupUser(ctx, w, p)
			if errors.Is(err, index.ErrNotFound) {
				return invalidf("principal: user %s does not exist", p.Name)
			} else if err != nil {
				return err
			}
			row.Principal, name = UserPrincipal(u).String(), u.Name
		}
		if err = w.CreateBinding(ctx, &row); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, ActionBindingCreate, &row, o)
		return err
	})
	if err != nil {
		return nil, bindingError("create", fmt.Sprintf("%s %s on %s", principal, role, mask), err)
	}
	s.writeAudit(ctx, ev)
	b := bindingResource(&row, name)
	return &b, nil
}

// List returns the bindings of principal, user:<name> or user:<uid> of a live user, or every
// binding when principal is "", ordered by id, each with its user's name.
func (s *Bindings) List(ctx context.Context, principal string) ([]Binding, error) {
	if principal != "" {
		p, err := ParsePrincipal(principal)
		if err != nil {
			return nil, invalidf("principal: %s", err.Error())
		}
		if p.Kind == KindUser {
			u, err := LookupUser(ctx, s.Index, p)
			switch {
			case errors.Is(err, index.ErrNotFound):
				return []Binding{}, nil
			case err != nil:
				return nil, fmt.Errorf("auth: list bindings: %w", err)
			}
			principal = UserPrincipal(u).String()
		}
	}
	rows, err := s.Index.ListBindings(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("auth: list bindings: %w", err)
	}
	list := make([]Binding, len(rows))
	for i := range rows {
		name, err := principalName(ctx, s.Index, rows[i].Principal)
		if err != nil {
			return nil, fmt.Errorf("auth: list bindings: %w", err)
		}
		list[i] = bindingResource(&rows[i], name)
	}
	return list, nil
}

// principalName returns the name of the user of principal user:<uid>, the name it had for a user
// since deleted, and "" for any other principal, or a user that does not exist.
func principalName(ctx context.Context, r index.Reader, principal string) (string, error) {
	p, err := ParsePrincipal(principal)
	if err != nil || p.Kind != KindUser || !index.IsUID(p.Name) {
		return "", nil //nolint:nilerr // a principal that is not a user's has no name
	}
	u, err := r.GetUserByUID(ctx, p.Name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return "", nil
	case err != nil:
		return "", err
	case u.DeletedAt != nil:
		return u.DeletedName, nil
	}
	return u.Name, nil
}

// Delete removes the binding id, with one binding.delete audit event. The last admin binding
// of an enabled user is refused (index.ErrConflict): nobody could manage the server over HTTP
// after it. The check comes after the delete, in its transaction, so that two deletes at once
// cannot each leave the other's binding as the last one.
func (s *Bindings) Delete(ctx context.Context, id int64, o Origin) error {
	var ev index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		all, err := w.ListBindings(ctx, "")
		if err != nil {
			return err
		}
		i := slices.IndexFunc(all, func(b index.Binding) bool { return b.ID == id })
		if i < 0 {
			return fmt.Errorf("%w: binding %d", index.ErrNotFound, id)
		}
		row := all[i]
		if err := w.DeleteBinding(ctx, id); err != nil {
			return err
		}
		if row.Role == RoleAdmin {
			if err := lastAdmin(ctx, w, row); err != nil {
				return err
			}
		}
		ev, err = s.record(ctx, w, ActionBindingDelete, &row, o)
		return err
	})
	if err != nil {
		return bindingError("delete", fmt.Sprintf("%d", id), err)
	}
	s.writeAudit(ctx, ev)
	return nil
}

// lastAdmin returns an error wrapping index.ErrConflict when the admin binding gone, deleted on
// w, was one of a live, enabled user and no such user has an admin binding left (anyAdmin).
func lastAdmin(ctx context.Context, w index.Writer, gone index.Binding) error {
	p, perr := ParsePrincipal(gone.Principal)
	if perr != nil {
		return nil //nolint:nilerr // a binding whose principal is not one granted nothing
	}
	u, err := LookupUser(ctx, w, p)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil
	case err != nil:
		return err
	case u.Disabled:
		return nil
	}
	ok, err := anyAdmin(ctx, w)
	if err != nil || ok {
		return err
	}
	return &authError{
		msg: fmt.Sprintf("binding %d is the last admin binding of an enabled user: create another first "+
			"(lajstrom admin binding create, or admin user create --admin)", gone.ID),
		err: index.ErrConflict,
	}
}

// check returns the principal of a binding that keeps docs/spec/rbac.md §3, or an error wrapping
// ErrInvalid that names the field. Whether the user exists is checked in the transaction.
func (s *Bindings) check(principal, role, mask string) (Principal, error) {
	p, err := bindable(principal)
	if err != nil {
		return Principal{}, err
	}
	if !IsRole(role) {
		return Principal{}, invalidf("role: %q is not reader, writer, maintainer or admin", role)
	}
	format, repo, err := ParseMask(mask)
	if err != nil {
		return Principal{}, invalidf("repoMask: %s", err.Error())
	}
	if format != "*" && s.Formats != nil && !s.Formats(format) {
		return Principal{}, invalidf("repoMask: %q is not a format of this server, nor *", format)
	}
	if repo != "*" && (len(repo) > config.MaxNameLen || !config.RepoNamePattern.MatchString(repo)) {
		return Principal{}, invalidf("repoMask: %q is not a repo name (docs/spec/manifest-profile.md §4.1) or *", repo)
	}
	if role == RoleAdmin && mask != MaskAll {
		return Principal{}, invalidf("repoMask: an admin binding is on %s: the admin role is every repo and the system", MaskAll)
	}
	return p, nil
}

// bindable returns the principal of a binding, user:<name>, user:<uid> or group:<name> with a
// name of CheckGroupName, or an error wrapping ErrInvalid that names the field.
func bindable(principal string) (Principal, error) {
	p, err := ParsePrincipal(principal)
	if err != nil {
		return Principal{}, invalidf("principal: %s", err.Error())
	}
	switch p.Kind {
	case KindUser:
	case KindGroup:
		if err := CheckGroupName(p.Name); err != nil {
			return Principal{}, invalidf("principal: %s", err.Error())
		}
	case KindToken, KindAnonymous:
		return Principal{}, invalidf("principal: %s cannot be bound: a binding is for user:<name> or group:<name>; "+
			"anonymous reads through a repo's anonymousRead, a deploy token gets its repo and scopes", principal)
	}
	return p, nil
}

// record builds the audit event of a change of the binding b, its id, role and mask in the
// details, and appends it on w when there is no audit.Writer (recordAudit).
func (s *Bindings) record(ctx context.Context, w index.Writer, action string, b *index.Binding, o Origin) (index.AuditEvent, error) {
	return recordAudit(ctx, w, s.Audit, index.AuditEvent{At: s.now(), Action: action, Subject: b.Principal}, o,
		map[string]any{"id": b.ID, "role": b.Role, "repoMask": b.RepoMask})
}

// writeAudit passes ev to the audit.Writer, after the commit of its change (writeAuditEvent).
func (s *Bindings) writeAudit(ctx context.Context, ev index.AuditEvent) {
	writeAuditEvent(ctx, s.Audit, s.log(), ev, "auth: audit event of a binding change not written", "principal")
}

// bindingError turns an error about the binding what into the one the callers answer: an
// *authError as it is, index.ErrNotFound and index.ErrConflict with a message for the person,
// anything else wrapped with op, for the log.
func bindingError(op, what string, err error) error {
	var ae *authError
	switch {
	case errors.As(err, &ae):
		return err
	case errors.Is(err, index.ErrNotFound):
		return &authError{msg: fmt.Sprintf("binding %s does not exist", what), err: err}
	case errors.Is(err, index.ErrConflict):
		return &authError{msg: fmt.Sprintf("binding %s exists already", what), err: err}
	}
	return fmt.Errorf("auth: %s binding %s: %w", op, what, err)
}

// bindingResource returns the Binding of a bindings row, with its user's name and a UTC time.
func bindingResource(b *index.Binding, name string) Binding {
	return Binding{ID: b.ID, Principal: b.Principal, Name: name, Role: b.Role, RepoMask: b.RepoMask, CreatedAt: b.CreatedAt.UTC()}
}

func (s *Bindings) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Bindings) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}
