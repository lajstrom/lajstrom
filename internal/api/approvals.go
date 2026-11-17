package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The requests that wait for a second admin (docs/spec/rbac.md §13, http-routing.md §4; #1285),
// made in place of the changes that grant the most (auth.Approvals): /api/v1/approvals lists
// them, an admin other than the requester approves one, and any admin rejects one.

// ApprovalList is the answer of GET /api/v1/approvals: a page of the requests, newest first, and
// in Next the cursor of the page after it, absent on the last page.
type ApprovalList struct {
	Approvals []auth.Approval `json:"approvals"`
	Next      string          `json:"next,omitempty"`
}

// listApprovals answers a page of the requests of ?state, or of every state, newest first: ?limit
// of them after the request of ?cursor (page). The admin role first.
func (h *handler) listApprovals(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	limit, after, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	var before int64
	if after != "" {
		if before, err = strconv.ParseInt(after, 10, 64); err != nil || before < 1 {
			return 0, nil, invalid("cursor: %q is not the next of a page of this list", r.URL.Query().Get("cursor"))
		}
	}
	list, err := h.approvals.List(r.Context(), r.URL.Query().Get("state"), before, limit+1)
	if err != nil {
		return 0, nil, serviceError(err)
	}
	out := ApprovalList{Approvals: list[:min(limit, len(list))]}
	if len(list) > limit {
		out.Next = cursor(strconv.FormatInt(out.Approvals[len(out.Approvals)-1].ID, 10))
	}
	return http.StatusOK, out, nil
}

// getApproval answers one request; 404 for an id that is none. The admin role first.
func (h *handler) getApproval(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	id, err := approvalID(r)
	if err != nil {
		return 0, nil, err
	}
	a, err := h.approvals.Get(r.Context(), id)
	if err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusOK, a, nil
}

// approveApproval approves a request, and so makes its change: 200 with the request decided. In
// this order, before the request is looked up: the admin role, under the admin mode a web UI's
// sign-in with a second factor (allowAdmin); the credential of an approval, the admin's password
// or a web UI's fresh sign-in through OIDC, 403 for any other token and 401 asking for more for a
// sign-in that is not fresh (auth.Approvals.MayApprove); the reason, ?changeReason=. Then the
// requester is 403, and a request decided or expired, or whose change no longer holds, 409. It
// takes no body, and no If-Match: a request never changes.
func (h *handler) approveApproval(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	if err := h.approvals.MayApprove(r.Context()); err != nil {
		return 0, nil, serviceError(err)
	}
	id, err := approvalID(r)
	if err != nil {
		return 0, nil, err
	}
	o, err := h.changeOrigin(r, nil)
	if err != nil {
		return 0, nil, err
	}
	a, err := h.approvals.Approve(r.Context(), id, o)
	if err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusOK, a, nil
}

// rejectApproval rejects a request, its requester's being a withdrawal: 200 with the request
// decided; 409 for one decided or expired. The admin role, with any credential that reaches it,
// first; the reason is ?changeReason=. It takes no body.
func (h *handler) rejectApproval(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	id, err := approvalID(r)
	if err != nil {
		return 0, nil, err
	}
	o, err := h.changeOrigin(r, nil)
	if err != nil {
		return 0, nil, err
	}
	a, err := h.approvals.Reject(r.Context(), id, o)
	if err != nil {
		return 0, nil, serviceError(err)
	}
	return http.StatusOK, a, nil
}

// approvalID returns the request of the path's {id}; one that is not a positive number is 404,
// the answer of a request that does not exist.
func approvalID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, &clientError{msg: fmt.Sprintf("request %s does not exist", r.PathValue("id")), err: index.ErrNotFound}
	}
	return id, nil
}
