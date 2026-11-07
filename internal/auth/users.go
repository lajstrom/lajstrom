package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/index"
)

// KindLocal is the kind of a user whose password Lajstrom keeps (users.kind); oidc comes after
// the MVP.
const KindLocal = "local"

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
)

// User is a local user as the CLI prints it and the users API (#47) answers it. It never
// carries the password hash.
type User struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"createdAt"`
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
}

// Create checks name and password and creates the local user with one user.create audit event,
// whose details say whether it is an admin. With admin, the binding (user:<name>, admin, */*)
// is created in the same transaction: the bootstrap of docs/spec/rbac.md §6.
func (s *Users) Create(ctx context.Context, name, password string, admin bool, o Origin) (*User, error) {
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
		if err = w.CreateUser(ctx, &row); err != nil {
			return err
		}
		if admin {
			b := index.Binding{Principal: Principal{Kind: KindUser, Name: name}.String(), Role: RoleAdmin, RepoMask: MaskAll, CreatedAt: row.CreatedAt}
			if err = w.CreateBinding(ctx, &b); err != nil {
				return err
			}
		}
		ev, err = s.record(ctx, w, ActionUserCreate, name, o, map[string]any{"admin": admin})
		return err
	})
	if err != nil {
		return nil, userError("create", name, err)
	}
	s.writeAudit(ctx, ev)
	u := resource(&row)
	return &u, nil
}

// Get returns the user called name.
func (s *Users) Get(ctx context.Context, name string) (*User, error) {
	row, err := s.Index.GetUser(ctx, name)
	if err != nil {
		return nil, userError("get", name, err)
	}
	u := resource(row)
	return &u, nil
}

// List returns every user, ordered by name.
func (s *Users) List(ctx context.Context) ([]User, error) {
	rows, err := s.Index.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	users := make([]User, len(rows))
	for i := range rows {
		users[i] = resource(&rows[i])
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
	return s.update(ctx, name, ActionUserPassword, o, func(u *index.User) bool {
		u.PasswordHash = hash
		return true
	})
}

// Disable disables the user, with one user.disable audit event: its credentials are refused
// from the next request on. A user that is disabled already is left as it is, without an event.
func (s *Users) Disable(ctx context.Context, name string, o Origin) error {
	return s.update(ctx, name, ActionUserDisable, o, func(u *index.User) bool {
		changed := !u.Disabled
		u.Disabled = true
		return changed
	})
}

// Enable enables a disabled user, with one user.enable audit event. A user that is enabled
// already is left as it is, without an event.
func (s *Users) Enable(ctx context.Context, name string, o Origin) error {
	return s.update(ctx, name, ActionUserEnable, o, func(u *index.User) bool {
		changed := u.Disabled
		u.Disabled = false
		return changed
	})
}

// update applies change to the user called name in one transaction with one audit event of
// action, when change reports that it changed something.
func (s *Users) update(ctx context.Context, name, action string, o Origin, change func(*index.User) bool) error {
	var ev *index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		row, err := w.GetUser(ctx, name)
		if err != nil {
			return err
		}
		if !change(row) {
			return nil
		}
		if err := w.UpdateUser(ctx, row); err != nil {
			return err
		}
		e, err := s.record(ctx, w, action, name, o, map[string]any{})
		ev = &e
		return err
	})
	if err != nil {
		return userError(action, name, err)
	}
	if ev != nil {
		s.writeAudit(ctx, *ev)
	}
	return nil
}

// record builds the audit event of a change of the user called name, with details and the
// origin, and appends it on w, in the transaction of the change, when there is no
// audit.Writer. The caller passes the event to writeAudit after the commit.
func (s *Users) record(ctx context.Context, w index.Writer, action, name string, o Origin, details map[string]any) (index.AuditEvent, error) {
	return recordAudit(ctx, w, s.Audit, index.AuditEvent{At: s.now(), Action: action, Subject: name}, o, details)
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
	switch {
	case errors.Is(err, index.ErrNotFound):
		return &authError{msg: fmt.Sprintf("user %s does not exist", name), err: err}
	case errors.Is(err, index.ErrConflict):
		return &authError{msg: fmt.Sprintf("user %s already exists", name), err: err}
	}
	return fmt.Errorf("auth: %s user %s: %w", op, name, err)
}

// resource returns the User of a users row, with a UTC time.
func resource(u *index.User) User {
	return User{Name: u.Name, Kind: u.Kind, Disabled: u.Disabled, CreatedAt: u.CreatedAt.UTC()}
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
