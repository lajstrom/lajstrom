package api

import (
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// AccessReviewRequest is the body of POST /api/v1/access-reviews (docs/spec/rbac.md §14): the
// sha256 of the document's bytes as the reviewer got them, 64 lowercase hexadecimal digits; its
// generatedAt (RFC 3339) and scope (all when absent); and a note of 1 to 4096 bytes, line feeds
// allowed. No changeReason: a review changes no access.
type AccessReviewRequest struct {
	SHA256      string  `json:"sha256"`
	GeneratedAt string  `json:"generatedAt"`
	Scope       *string `json:"scope"`
	Note        string  `json:"note"`
}

// recordAccessReview answers POST /api/v1/access-reviews (docs/spec/rbac.md §14): the admin role,
// checked before the body is read, with any credential that reaches it; then the review of the
// body, written as the audit row access.review by the request's principal
// (auth.RecordAccessReview): 201 with the row as GET /api/v1/audit answers it. A field out of its
// bounds is 400 naming it, and nothing is written. It takes no changeReason, whatever
// auth.requireReason says, and waits for no second admin: a review changes no access. Past
// reviews are GET /api/v1/audit?action=access.review.
func (h *handler) recordAccessReview(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	var req AccessReviewRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	review := auth.AccessReview{SHA256: req.SHA256, Scope: auth.AccessAll, Note: req.Note}
	if req.Scope != nil {
		review.Scope = *req.Scope
	}
	if req.GeneratedAt != "" {
		at, err := time.Parse(time.RFC3339, req.GeneratedAt)
		if err != nil {
			return 0, nil, invalid("generatedAt: %q is not an RFC 3339 time, like 2026-11-18T10:00:00Z", req.GeneratedAt)
		}
		review.GeneratedAt = at
	}
	ev, err := auth.RecordAccessReview(r.Context(), h.repos.Index, review, origin(r))
	if err != nil {
		return 0, nil, serviceError(err)
	}
	kind, err := auth.PrincipalKind(r.Context(), h.repos.Index, ev.Principal)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, audit.NewEntry(ev, kind), nil
}
