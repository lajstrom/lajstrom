package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
)

// SourceBootstrap is the source of the user.create event of the first admin made with the
// bootstrap token.
const SourceBootstrap = "bootstrap"

// BootstrapRequest is the body of POST /api/v1/bootstrap: the bootstrap token, and the name and
// the password of the first admin.
type BootstrapRequest struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// bootstrap creates the first admin from the BootstrapRequest of the body
// (auth.Bootstrap.Exchange): 201 with the user and its Location. It asks no authorizer: the
// token is the credential. Once an enabled user has an admin binding, every request is the 404
// of a path that is no endpoint, whatever its body. A wrong token is 403, an address throttled
// for its wrong tokens 429 with Retry-After, and the right token used or expired 403 saying so.
func (h *handler) bootstrap(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if h.boot == nil {
		return h.notFound(w, r)
	}
	switch open, err := h.boot.Open(r.Context()); {
	case err != nil:
		return 0, nil, err
	case !open:
		return h.notFound(w, r)
	}
	var req BootstrapRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	o := origin(r)
	o.Source = SourceBootstrap
	u, err := h.boot.Exchange(r.Context(), req.Token, req.Name, req.Password, auth.Origin(o))
	var te *auth.ThrottledError
	switch {
	case errors.As(err, &te):
		w.Header().Set("Retry-After", strconv.Itoa(max(int(math.Ceil(te.RetryAfter.Seconds())), 1)))
		return http.StatusTooManyRequests, envelope{Error: envelopeError{Code: codeUnavailable,
			Message: "too many wrong bootstrap tokens: try again after Retry-After seconds"}}, nil
	case errors.Is(err, auth.ErrBootstrapClosed):
		return h.notFound(w, r)
	case errors.Is(err, auth.ErrBootstrapToken):
		return 0, nil, &clientError{msg: err.Error(), err: format.ErrForbidden}
	case err != nil:
		return 0, nil, serviceError(err)
	}
	w.Header().Set("Location", "/api/v1/users/"+u.Name)
	return http.StatusCreated, u, nil
}
