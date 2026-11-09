package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// BindingList is the answer of GET /api/v1/bindings: a page of the bindings ordered by id, and
// in Next the cursor of the page after it, absent on the last page.
type BindingList struct {
	Bindings []auth.Binding `json:"bindings"`
	Next     string         `json:"next,omitempty"`
}

// BindingRequest is the body of POST /api/v1/bindings: user:<name> or user:<uid>, the role and
// the mask (docs/spec/rbac.md §3).
type BindingRequest struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
	RepoMask  string `json:"repoMask"`
}

// listBindings answers a page of the bindings, of the principal of ?principal or of every
// principal, ordered by id: ?limit of them after the binding of ?cursor (page).
func (h *handler) listBindings(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	var afterID int64
	if after != "" {
		if afterID, err = strconv.ParseInt(after, 10, 64); err != nil {
			return 0, nil, invalid("cursor: %q is not the next of a page of this list", r.URL.Query().Get("cursor"))
		}
	}
	all, err := h.bindings.List(r.Context(), r.URL.Query().Get("principal"))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	i, _ := slices.BinarySearchFunc(all, afterID, func(b auth.Binding, id int64) int {
		if b.ID <= id {
			return -1
		}
		return 1
	})
	list := BindingList{Bindings: all[i:min(i+limit, len(all))]}
	if i+limit < len(all) {
		list.Next = cursor(strconv.FormatInt(list.Bindings[len(list.Bindings)-1].ID, 10))
	}
	return http.StatusOK, list, nil
}

// createBinding creates a binding from the BindingRequest of the body: 201 with the binding
// and its Location.
func (h *handler) createBinding(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	var req BindingRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	b, err := h.bindings.Create(r.Context(), req.Principal, req.Role, req.RepoMask, auth.Origin(origin(r)))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/bindings/%d", b.ID))
	return http.StatusCreated, b, nil
}

// deleteBinding deletes a binding: 204; 404 for an id that is not a binding, 409 for the last
// admin binding of an enabled user.
func (h *handler) deleteBinding(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, nil, invalid("id: %q is not the id of a binding", r.PathValue("id"))
	}
	if err := h.bindings.Delete(r.Context(), id, auth.Origin(origin(r))); err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusNoContent, nil, nil
}
