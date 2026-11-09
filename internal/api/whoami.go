package api

import (
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
)

// Whoami is the answer of GET /api/v1/whoami: the request's principal in its canonical form
// (docs/spec/rbac.md §1), its kind and its name: a user's name, with its uid; a deploy token's
// id.
type Whoami struct {
	Principal string `json:"principal"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

// whoami answers the request's principal. No authorizer is asked: every principal may ask who
// it is. The anonymous principal is answered 401 with the Basic challenge, so that a client
// sends credentials.
func (h *handler) whoami(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	p := auth.PrincipalFrom(r.Context())
	if p.Kind == auth.KindAnonymous {
		return 0, nil, &clientError{msg: "credentials are needed", err: format.ErrUnauthenticated}
	}
	w := Whoami{Principal: p.String(), Kind: string(p.Kind), Name: p.Name}
	if p.Kind == auth.KindUser {
		u, err := auth.LookupUser(r.Context(), h.tokens.Index, p)
		if err != nil {
			return 0, nil, err
		}
		w.Name, w.UID = u.Name, u.UID
	}
	return http.StatusOK, w, nil
}
