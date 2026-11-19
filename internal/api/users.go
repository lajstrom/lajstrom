package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// UserList is the answer of GET /api/v1/users: a page of the live users ordered by name, of the
// kind of ?kind when it is given, and in Next the cursor of the page after it, absent on the last
// page.
type UserList struct {
	Users []auth.User `json:"users"`
	Next  string      `json:"next,omitempty"`
}

// UserRequest is the body of POST /api/v1/users. Admin also binds the user to the admin role
// on */* (docs/spec/rbac.md §6); under auth.approval second-admin it is refused, and that
// binding is requested with POST /api/v1/bindings (§13). Kind is local, the default, or service:
// a service account (docs/spec/rbac.md §1), which has Owner, user:<name>, user:<uid> or
// group:<name>, and neither a password nor Admin.
type UserRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
	Kind     string `json:"kind"`
	Owner    string `json:"owner"`
	Reason
}

// UserPatch is the body of PATCH /api/v1/users/{user}: the fields to change, at least one. Owner
// is a service account's only; MustChange a user's with a password (docs/spec/rbac.md §1, Change
// at first use).
type UserPatch struct {
	Name       *string `json:"name"`
	Disabled   *bool   `json:"disabled"`
	Owner      *string `json:"owner"`
	MustChange *bool   `json:"mustChange"`
	Reason
}

// PasswordRequest is the body of POST /api/v1/users/{user}/password: Password alone is an admin's
// new password; with CurrentPassword, the user's own change, whose credential is CurrentPassword
// (docs/spec/rbac.md §1, A user's own password).
type PasswordRequest struct {
	Password        string  `json:"password"`
	CurrentPassword *string `json:"currentPassword,omitempty"`
	Reason
}

// Bounds of ?limit on the lists of the API.
const (
	defaultLimit = 100
	maxLimit     = 1000
)

// allowAdmin decides whether the request may manage the system's resources, users and
// bindings: the admin role (manage on the system, auth.System; docs/spec/http-routing.md §4).
func (h *handler) allowAdmin(r *http.Request) error {
	return h.allow(r, auth.ActionManage, auth.System, auth.System)
}

// userName returns the name of the live user that the path segment {user} names: a name, or
// a uid (index.IsUID), which a name never looks like. A uid of no user, or of a deleted one,
// is index.ErrNotFound.
func (h *handler) userName(r *http.Request) (string, error) {
	s := r.PathValue("user")
	if !index.IsUID(s) {
		return s, nil
	}
	u, err := h.users.Index.GetUserByUID(r.Context(), s)
	switch {
	case errors.Is(err, index.ErrNotFound) || err == nil && u.DeletedAt != nil:
		return "", &clientError{msg: fmt.Sprintf("user %s does not exist", s), err: index.ErrNotFound}
	case err != nil:
		return "", err
	}
	return u.Name, nil
}

// listUsers answers a page of the live users of ?kind, local, oidc or service, or of every
// kind, ordered by name: ?limit of them (100 when absent, 1000 at most), after the user of
// ?cursor, the Next of the page before.
func (h *handler) listUsers(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	users, err := h.users.List(r.Context(), r.URL.Query().Get("kind"))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	slices.SortFunc(users, func(a, b auth.User) int { return strings.Compare(a.Name, b.Name) })
	i, _ := slices.BinarySearchFunc(users, after, func(u auth.User, name string) int {
		if strings.Compare(u.Name, name) <= 0 {
			return -1
		}
		return 1
	})
	list := UserList{Users: users[i:min(i+limit, len(users))]}
	if i+limit < len(users) {
		list.Next = cursor(list.Users[len(list.Users)-1].Name)
	}
	return http.StatusOK, list, nil
}

// page returns ?limit and the key that ?cursor holds ("" for the first page).
func page(r *http.Request) (limit int, after string, err error) {
	limit = defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > maxLimit {
			return 0, "", invalid("limit: %q is not a number from 1 to %d", v, maxLimit)
		}
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(b) == 0 {
			return 0, "", invalid("cursor: %q is not the next of a page of this list", v)
		}
		after = string(b)
	}
	return limit, after, nil
}

// cursor returns the cursor of the page after the one that ends at key: opaque to the client.
func cursor(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

// createUser creates a local user or a service account from the UserRequest of the body: 201
// with the user and its Location. Under auth.approval second-admin, Admin is 400 naming the
// setting, and nothing is made: the admin binding is made with POST /api/v1/bindings, which waits
// for a second admin (docs/spec/rbac.md §13), so that the users route is no way around it.
func (h *handler) createUser(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	var req UserRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	o, err := h.changeOrigin(r, req.ChangeReason)
	if err != nil {
		return 0, nil, err
	}
	var u *auth.User
	switch req.Kind {
	case "", auth.KindLocal:
		switch {
		case req.Owner != "":
			return 0, nil, invalid("owner: only a service account has an owner (kind service)")
		case req.Admin && h.secondAdmin:
			return 0, nil, invalid("admin: under auth.approval second-admin, a user is bound with POST /api/v1/bindings, " +
				"which waits for a second admin, not made an admin here")
		}
		u, err = h.users.Create(r.Context(), req.Name, req.Password, req.Admin, o)
	case auth.KindService:
		switch {
		case req.Password != "":
			return 0, nil, invalid("password: a service account has no password: it authenticates with its tokens")
		case req.Admin:
			return 0, nil, invalid("admin: a service account is bound with POST /api/v1/bindings, not made an admin here")
		}
		u, err = h.users.CreateService(r.Context(), req.Name, req.Owner, o)
	default:
		return 0, nil, invalid("kind: %q is not local or service; an oidc user is made by its first login", req.Kind)
	}
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("Location", "/api/v1/users/"+u.Name)
	w.Header().Set("ETag", userETag(u))
	return http.StatusCreated, u, nil
}

// userETag is the entity tag of u (ETag).
func userETag(u *auth.User) string { return ETag(TableUsers, u.ID, u.Revision) }

// getUser answers one live user, with its ETag; 304 for an If-None-Match that has it.
func (h *handler) getUser(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	u, err := h.users.Get(r.Context(), name)
	if err != nil {
		return 0, nil, err
	}
	return tagged(w, r, userETag(u), u)
}

// updateUser applies the UserPatch of the body in one transaction (auth.Users.Change) and
// answers the user as it is then, with its ETag: disabled first, then a service account's owner,
// under the old name, then the name, then mustChange; each change is an audit event of its own
// (user.disable or user.enable, user.owner, user.rename, user.mustchange), and a change to what
// the user already has is no event. With If-Match, the change is made only while the user has a
// revision it names.
func (h *handler) updateUser(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	var patch UserPatch
	if err := decodeBody(w, r, &patch); err != nil {
		return 0, nil, err
	}
	if patch.Name == nil && patch.Disabled == nil && patch.Owner == nil && patch.MustChange == nil {
		return 0, nil, invalid("body: nothing to change; the fields are name, disabled, owner and mustChange")
	}
	o, err := h.changeOrigin(r, patch.ChangeReason)
	if err != nil {
		return 0, nil, err
	}
	if r, err = conditional(r, TableUsers, "user "+name); err != nil {
		return 0, nil, err
	}
	u, err := h.users.Change(r.Context(), name, auth.UserChange{Disabled: patch.Disabled, Owner: patch.Owner, Name: patch.Name,
		MustChange: patch.MustChange}, o)
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("ETag", userETag(u))
	return http.StatusOK, u, nil
}

// deleteUser deletes a user (auth.Users.Delete): 204. With If-Match, only while the user has a
// revision it names.
func (h *handler) deleteUser(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	o, err := h.changeOrigin(r, nil)
	if err != nil {
		return 0, nil, err
	}
	if r, err = conditional(r, TableUsers, "user "+name); err != nil {
		return 0, nil, err
	}
	if err := h.users.Delete(r.Context(), name, o); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// unlockUser ends the lock of the lockout of a user and sets its count of wrong passwords to 0
// (auth.Users.Unlock; docs/spec/rbac.md §1, Local passwords): 204, with a user.unlock audit row
// when a lock was in force. It takes no body: its reason is ?changeReason=.
func (h *handler) unlockUser(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	o, err := h.changeOrigin(r, nil)
	if err != nil {
		return 0, nil, err
	}
	if err := h.users.Unlock(r.Context(), name, o); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// setPassword sets the password of a user from the PasswordRequest of the body: 204. With
// currentPassword it is the user's own change (ownPassword), which takes no role and ignores the
// request's Authorization; without, an admin's new password: the authentication of the request,
// which the server's middleware leaves to this handler (Options.Authenticate), then the admin
// role (docs/spec/rbac.md §1, A user's own password; §3). Under auth.approval second-admin, an
// admin's new password for a user who holds admin is 400 naming the setting, whoever asks
// (adminPassword).
func (h *handler) setPassword(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var req PasswordRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	if req.CurrentPassword != nil {
		return h.ownPassword(w, r, *req.CurrentPassword, req.Password, req.ChangeReason)
	}
	if h.authenticate != nil {
		var ok bool
		if r, ok = h.authenticate(w, r); !ok {
			return 0, nil, nil // answered: the credentials were refused
		}
	}
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	if h.secondAdmin {
		if err := h.adminPassword(r, name); err != nil {
			return 0, nil, err
		}
	}
	o, err := h.changeOrigin(r, req.ChangeReason)
	if err != nil {
		return 0, nil, err
	}
	if err := h.users.SetPassword(r.Context(), name, req.Password, o); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// adminPassword returns a 400 naming auth.approval second-admin when the user called name holds
// admin (auth.HoldsAdmin: an admin binding of its own, or of a group whose membership has not
// lapsed; enabled or disabled, whatever the admin mode), and nil otherwise, with a user that
// does not exist left to SetPassword to answer (docs/spec/rbac.md §13, Another admin's password;
// #1393). An admin who knew another's password would sign in as that admin and approve its own
// requests: such a user changes its own password with currentPassword, or lajstrom admin user
// set-password sets it. The refusal holds whoever asks, the user itself included, so that a token
// made for the user sets none either; it writes no row.
func (h *handler) adminPassword(r *http.Request, name string) error {
	u, err := h.users.Index.GetUser(r.Context(), name)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	admin, err := auth.HoldsAdmin(r.Context(), h.users.Index, u, h.repos.now())
	switch {
	case err != nil:
		return err
	case admin:
		return invalid("password: under auth.approval second-admin, the API sets no new password for user %s, who holds admin: "+
			"the user changes its own with currentPassword, or lajstrom admin user set-password sets it", name)
	}
	return nil
}

// ownPassword is a user's own change of its password, to password, from current, with reason when
// the body has one, which it never needs (ownOrigin)
// (auth.Authenticator.ChangeOwnPassword; docs/spec/rbac.md §1, A user's own password): the
// current password is the request's credential, checked as Basic credentials are, with the
// throttles and the lockout, and the request's Authorization is ignored. 204; a refused current
// password, of whatever user, 401 with the message of every refused password; a throttled account
// or address 429 with Retry-After; a new password that breaks a rule or is one of the last four
// 400; passwords turned off 400 naming the setting; 501 on a server that checks no passwords.
func (h *handler) ownPassword(w http.ResponseWriter, r *http.Request, current, password string, reason *string) (int, any, error) {
	if h.passwords == nil {
		return 0, nil, &clientError{msg: "this server checks no passwords", err: format.ErrUnsupported}
	}
	name, err := h.ownName(r)
	if err != nil {
		return 0, nil, err
	}
	o, err := h.ownOrigin(r, reason)
	if err != nil {
		return 0, nil, err
	}
	err = h.passwords.ChangeOwnPassword(r.Context(), name, current, password, o)
	var te *auth.ThrottledError
	switch {
	case errors.As(err, &te):
		return tooManyLogins(w, te)
	case errors.Is(err, auth.ErrBadCredentials): // the authenticator's message, when it has one
		return 0, nil, &clientError{msg: h.passwords.RefusalMessage(err, msgBadPassword), err: format.ErrUnauthenticated}
	case err != nil:
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// ownName returns the name that the path segment {user} gives a user's own change: a name as it
// is, and for a uid (index.IsUID) the name of its live user. A uid of no live user stays as it is:
// no user has such a name, so that the change is refused as one of a name no user has is, and
// tells nobody whether the uid is a user's.
func (h *handler) ownName(r *http.Request) (string, error) {
	s := r.PathValue("user")
	if !index.IsUID(s) {
		return s, nil
	}
	u, err := h.users.Index.GetUserByUID(r.Context(), s)
	switch {
	case errors.Is(err, index.ErrNotFound) || err == nil && u.DeletedAt != nil:
		return s, nil
	case err != nil:
		return "", err
	}
	return u.Name, nil
}
