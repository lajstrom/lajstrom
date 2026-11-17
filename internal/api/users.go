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
// on */* (docs/spec/rbac.md §6). Kind is local, the default, or service: a service account
// (docs/spec/rbac.md §1), which has Owner, user:<name>, user:<uid> or group:<name>, and neither
// a password nor Admin.
type UserRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
	Kind     string `json:"kind"`
	Owner    string `json:"owner"`
}

// UserPatch is the body of PATCH /api/v1/users/{user}: the fields to change, at least one. Owner
// is a service account's only.
type UserPatch struct {
	Name     *string `json:"name"`
	Disabled *bool   `json:"disabled"`
	Owner    *string `json:"owner"`
}

// PasswordRequest is the body of POST /api/v1/users/{user}/password.
type PasswordRequest struct {
	Password string `json:"password"`
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
// with the user and its Location.
func (h *handler) createUser(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	var req UserRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	var (
		u   *auth.User
		err error
	)
	switch req.Kind {
	case "", auth.KindLocal:
		if req.Owner != "" {
			return 0, nil, invalid("owner: only a service account has an owner (kind service)")
		}
		u, err = h.users.Create(r.Context(), req.Name, req.Password, req.Admin, auth.Origin(origin(r)))
	case auth.KindService:
		switch {
		case req.Password != "":
			return 0, nil, invalid("password: a service account has no password: it authenticates with its tokens")
		case req.Admin:
			return 0, nil, invalid("admin: a service account is bound with POST /api/v1/bindings, not made an admin here")
		}
		u, err = h.users.CreateService(r.Context(), req.Name, req.Owner, auth.Origin(origin(r)))
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
// under the old name, then the name; each change is an audit event of its own (user.disable or
// user.enable, user.owner, user.rename), and a change to what the user already has is no event.
// With If-Match, the change is made only while the user has a revision it names.
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
	if patch.Name == nil && patch.Disabled == nil && patch.Owner == nil {
		return 0, nil, invalid("body: nothing to change; the fields are name, disabled and owner")
	}
	if r, err = conditional(r, TableUsers, "user "+name); err != nil {
		return 0, nil, err
	}
	u, err := h.users.Change(r.Context(), name, auth.UserChange{Disabled: patch.Disabled, Owner: patch.Owner, Name: patch.Name},
		auth.Origin(origin(r)))
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
	if r, err = conditional(r, TableUsers, "user "+name); err != nil {
		return 0, nil, err
	}
	if err := h.users.Delete(r.Context(), name, auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// unlockUser ends the lock of the lockout of a user and sets its count of wrong passwords to 0
// (auth.Users.Unlock; docs/spec/rbac.md §1, Local passwords): 204, with a user.unlock audit row
// when a lock was in force. It takes no body.
func (h *handler) unlockUser(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	if err := h.users.Unlock(r.Context(), name, auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// setPassword sets the password of a user from the PasswordRequest of the body: 204.
func (h *handler) setPassword(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	var req PasswordRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	if err := h.users.SetPassword(r.Context(), name, req.Password, auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}
