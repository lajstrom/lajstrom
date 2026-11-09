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

// UserList is the answer of GET /api/v1/users: a page of the live users ordered by name, and
// in Next the cursor of the page after it, absent on the last page.
type UserList struct {
	Users []auth.User `json:"users"`
	Next  string      `json:"next,omitempty"`
}

// UserRequest is the body of POST /api/v1/users. Admin also binds the user to the admin role
// on */* (docs/spec/rbac.md §6).
type UserRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
}

// UserPatch is the body of PATCH /api/v1/users/{user}: the fields to change, at least one.
type UserPatch struct {
	Name     *string `json:"name"`
	Disabled *bool   `json:"disabled"`
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

// allowUsers decides whether the request may manage users: the admin role (manage on the
// system, auth.System; docs/spec/http-routing.md §4).
func (h *handler) allowUsers(r *http.Request) error {
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

// listUsers answers a page of the live users, ordered by name: ?limit of them (100 when
// absent, 1000 at most), after the user of ?cursor, the Next of the page before.
func (h *handler) listUsers(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowUsers(r); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	users, err := h.users.List(r.Context())
	if err != nil {
		return 0, nil, err
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

// createUser creates a local user from the UserRequest of the body: 201 with the user and its
// Location.
func (h *handler) createUser(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowUsers(r); err != nil {
		return 0, nil, err
	}
	var req UserRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	u, err := h.users.Create(r.Context(), req.Name, req.Password, req.Admin, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("Location", "/api/v1/users/"+u.Name)
	return http.StatusCreated, u, nil
}

// getUser answers one live user.
func (h *handler) getUser(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowUsers(r); err != nil {
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
	return http.StatusOK, u, nil
}

// updateUser applies the UserPatch of the body and answers the user as it is then: disabled
// first, under the old name, then the name; each change is an audit event of its own
// (user.disable or user.enable, user.rename), and a change to what the user already has is no
// event.
func (h *handler) updateUser(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowUsers(r); err != nil {
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
	if patch.Name == nil && patch.Disabled == nil {
		return 0, nil, invalid("body: nothing to change; the fields are name and disabled")
	}
	ctx, o := r.Context(), auth.Origin(origin(r))
	if _, err := h.users.Get(ctx, name); err != nil {
		return 0, nil, err
	}
	switch {
	case patch.Disabled == nil:
	case *patch.Disabled:
		err = h.users.Disable(ctx, name, o)
	default:
		err = h.users.Enable(ctx, name, o)
	}
	if err == nil && patch.Name != nil {
		if err = h.users.Rename(ctx, name, *patch.Name, o); err == nil {
			name = *patch.Name
		}
	}
	if err != nil {
		return 0, nil, serviceError(err)
	}
	u, err := h.users.Get(ctx, name)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, u, nil
}

// deleteUser deletes a user (auth.Users.Delete): 204.
func (h *handler) deleteUser(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowUsers(r); err != nil {
		return 0, nil, err
	}
	name, err := h.userName(r)
	if err != nil {
		return 0, nil, err
	}
	if err := h.users.Delete(r.Context(), name, auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}

// setPassword sets the password of a user from the PasswordRequest of the body: 204.
func (h *handler) setPassword(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowUsers(r); err != nil {
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
