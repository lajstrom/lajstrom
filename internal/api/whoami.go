package api

import (
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
)

// Whoami is the answer of GET /api/v1/whoami: the request's principal in its canonical form
// (docs/spec/rbac.md §1), its kind and its name.
type Whoami struct {
	Principal string `json:"principal"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

// whoami answers the request's principal. No authorizer is asked: every principal may ask who
// it is. The anonymous principal is answered 401 with the Basic challenge, so that a client
// sends credentials.
func (h *handler) whoami(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	p := auth.PrincipalFrom(r.Context())
	if p.Kind == auth.KindAnonymous {
		return 0, nil, &clientError{msg: "credentials are needed", err: format.ErrUnauthenticated}
	}
	return http.StatusOK, Whoami{Principal: p.String(), Kind: string(p.Kind), Name: p.Name}, nil
}
