package format

import (
	"errors"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// basicChallenge is the WWW-Authenticate header of a 401 on a frontend: clients answer it with
// HTTP Basic credentials (docs/spec/rbac.md §1).
const basicChallenge = `Basic realm="lajstrom"`

// WriteError answers err with the status its sentinel maps to and a text/plain body (with
// X-Content-Type-Options: nosniff) and returns the status. 4xx bodies are err.Error()+"\n";
// 5xx bodies are http.StatusText(status)+"\n" (the cause is for the log, not the client).
// A 401 carries WWW-Authenticate: Basic realm="lajstrom". See StatusOf for the mapping.
func WriteError(w http.ResponseWriter, err error) int {
	status := StatusOf(err)
	msg := http.StatusText(status)
	if status < http.StatusInternalServerError {
		msg = err.Error()
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", basicChallenge)
	}
	http.Error(w, msg, status) // text/plain, nosniff, msg+"\n", no stale Content-Length
	return status
}

// StatusOf returns the status WriteError answers err with. It is the one error table of the
// frontends; a plugin wraps these sentinels with %w and does not choose statuses itself.
// The checks run in this order, with errors.Is and errors.As:
//
//	ErrInvalid                                 400 Bad Request
//	ErrUnauthenticated                         401 Unauthorized
//	ErrForbidden                               403 Forbidden
//	index.ErrNotFound, provider.ErrNotFound    404 Not Found
//	index.ErrConflict, provider.ErrConflict    409 Conflict
//	*http.MaxBytesError                        413 Content Too Large
//	ErrUnsupported, provider.ErrUnsupported    501 Not Implemented
//	provider.ErrUnauthorized                   502 Bad Gateway (the backend rejected our credentials)
//	anything else, nil included                500 Internal Server Error
func StatusOf(err error) int {
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return http.StatusInternalServerError
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, index.ErrNotFound), errors.Is(err, provider.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, index.ErrConflict), errors.Is(err, provider.ErrConflict):
		return http.StatusConflict
	case errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrUnsupported), errors.Is(err, provider.ErrUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, provider.ErrUnauthorized):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
