package format

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// basicChallenge is the WWW-Authenticate header of a 401 on a frontend: clients answer it with
// HTTP Basic credentials (docs/spec/rbac.md §1).
const basicChallenge = `Basic realm="lajstrom"`

// WriteError answers err with the status its sentinel maps to and a text/plain body (with
// X-Content-Type-Options: nosniff) and returns the status. 4xx bodies are err.Error()+"\n";
// 5xx bodies are http.StatusText(status)+"\n" (the cause is for the log, not the client).
// A 401 carries WWW-Authenticate: Basic realm="lajstrom", a 405 Allow: GET, HEAD, a 503 of
// provider.ErrUnavailable or of an *UnavailableError Retry-After (SetRetryAfter). See StatusOf
// for the mapping.
func WriteError(w http.ResponseWriter, err error) int {
	status := StatusOf(err)
	msg := http.StatusText(status)
	if status < http.StatusInternalServerError {
		msg = err.Error()
	}
	switch status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", basicChallenge)
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", "GET, HEAD")
	}
	SetRetryAfter(w.Header(), err)
	http.Error(w, msg, status) // text/plain, nosniff, msg+"\n", no stale Content-Length
	return status
}

// SetRetryAfter sets Retry-After in h when err says when to try again: a
// provider.ErrUnavailable of an open circuit breaker, whose time left until its trial request
// becomes whole seconds, at least 1, or an *UnavailableError of an upstream that asked to wait.
// Every writer of error answers calls it (WriteError, the OCI facade, the management API), so
// that the header is set wherever StatusOf answers 503.
func SetRetryAfter(h http.Header, err error) {
	after, ok := provider.RetryAfter(err)
	var u *UnavailableError
	if !ok && errors.As(err, &u) {
		after, ok = u.RetryAfter, true
	}
	if !ok {
		return
	}
	secs := max(int64((after+time.Second-1)/time.Second), 1)
	h.Set("Retry-After", strconv.FormatInt(secs, 10))
}

// StatusOf returns the status WriteError answers err with. It is the one error table of the
// frontends; a plugin wraps these sentinels with %w and does not choose statuses itself.
// The checks run in this order, with errors.Is and errors.As:
//
//	ErrInvalid                                 400 Bad Request
//	ErrUnauthenticated                         401 Unauthorized
//	ErrForbidden                               403 Forbidden
//	ErrMethodNotAllowed                        405 Method Not Allowed, with Allow: GET, HEAD
//	index.ErrNotFound, provider.ErrNotFound    404 Not Found
//	index.ErrConflict, provider.ErrConflict    409 Conflict
//	*http.MaxBytesError,                       413 Content Too Large
//	  *provider.RejectedError of a 413           (the backend's: the file is too large for it)
//	ErrUnsupported, provider.ErrUnsupported    501 Not Implemented
//	provider.ErrUnauthorized,                  502 Bad Gateway (the backend rejected our
//	  *provider.RejectedError not busy           credentials, or what Lajstrom sent it, #494)
//	provider.ErrUnavailable                    503 Service Unavailable, with Retry-After (SetRetryAfter):
//	                                           an open circuit breaker, or the backend's 429 or 503
//	ErrBadGateway                              502 Bad Gateway (a proxy's upstream failed)
//	ErrUnavailable                             503 Service Unavailable, with Retry-After (the upstream's)
//	ErrGatewayTimeout                          504 Gateway Timeout
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
	case errors.Is(err, ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed
	case errors.Is(err, index.ErrNotFound), errors.Is(err, provider.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, index.ErrConflict), errors.Is(err, provider.ErrConflict):
		return http.StatusConflict
	case errors.As(err, &tooLarge), refusedBy(err, http.StatusRequestEntityTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrUnsupported), errors.Is(err, provider.ErrUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, provider.ErrUnauthorized), refusedBy(err, 0):
		return http.StatusBadGateway
	case errors.Is(err, provider.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrBadGateway):
		return http.StatusBadGateway
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrGatewayTimeout):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// refusedBy reports whether err is the backend's refusal of what Lajstrom sent it, a
// *provider.RejectedError of a backend that is not busy, with status, or with any when status
// is 0 (#494). A frontend's client sent Lajstrom its file, not the request the backend refused.
func refusedBy(err error, status int) bool {
	var rej *provider.RejectedError
	return errors.As(err, &rej) && !rej.Busy() && (status == 0 || rej.Status == status)
}
