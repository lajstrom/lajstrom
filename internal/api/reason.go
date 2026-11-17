package api

import (
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// A reason with each change of the control plane (docs/spec/rbac.md §13, docs/spec/http-routing.md
// §4, A reason; #1285): changeReason, a field of the JSON body of a route that takes one, or the
// query parameter ?changeReason= of a route that takes none, kept in the change's audit row as
// details.changeReason. With auth.requireReason (Options.RequireReason) a change without one is
// 400, after the role is checked and before anything is written. A person's own credentials take
// one when it is given and never need it.

// changeReasonParam is the field of a body, and the query parameter, of the reason of a change.
const changeReasonParam = "changeReason"

// Reason is the reason of a change in a JSON body, embedded in the body of every route of a
// change of the control plane (docs/spec/rbac.md §13).
type Reason struct {
	// ChangeReason is a ticket or a sentence, 1 to auth.MaxChangeReason bytes of UTF-8 without
	// control characters; nil when the body has none.
	ChangeReason *string `json:"changeReason,omitempty"`
}

// changeOrigin returns the Origin of a change of the control plane made by r (origin) with its
// reason: body, the changeReason of the JSON body (nil when it has none, and for a route without
// a body), or ?changeReason=. Both forms, a parameter given twice, and a reason that
// auth.CheckChangeReason refuses are 400 naming changeReason; none, while auth.requireReason is
// on, is 400 naming it and the setting. The caller asks after the role, and before it writes.
func (h *handler) changeOrigin(r *http.Request, body *string) (Origin, error) {
	return h.reasonOrigin(r, body, h.requireReason)
}

// ownOrigin is changeOrigin for a change of a person's own credentials, which takes a reason
// when one is given and needs none whatever auth.requireReason says (docs/spec/rbac.md §13).
func (h *handler) ownOrigin(r *http.Request, body *string) (Origin, error) {
	return h.reasonOrigin(r, body, false)
}

// reasonOrigin is changeOrigin, a reason needed when required is true.
func (h *handler) reasonOrigin(r *http.Request, body *string, required bool) (Origin, error) {
	o := origin(r)
	query, inQuery := r.URL.Query()[changeReasonParam]
	reason := ""
	switch {
	case body != nil && inQuery:
		return o, invalid("changeReason: in the body or in the query, not both")
	case len(query) > 1:
		return o, invalid("changeReason: once in the query")
	case body != nil:
		reason = *body
	case inQuery:
		reason = query[0]
	case required:
		return o, invalid("changeReason: required (auth.requireReason): a ticket or a sentence, the reference of the " +
			"change's approval, in the body or, on a route without one, as ?changeReason=")
	default:
		return o, nil
	}
	if err := auth.CheckChangeReason(reason); err != nil {
		return o, invalid("changeReason: %s", err.Error())
	}
	o.ChangeReason = reason
	return o, nil
}
