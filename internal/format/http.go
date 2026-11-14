package format

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// basicChallenge is the WWW-Authenticate header of a 401 on a frontend: clients answer it with
// HTTP Basic credentials (docs/spec/rbac.md §1).
const basicChallenge = `Basic realm="lajstrom"`

// WriteError answers err with the status its sentinel maps to and a text/plain body (with
// X-Content-Type-Options: nosniff) and returns the status. 4xx bodies are err.Error()+"\n", but
// for the fixed message of a 408; 5xx bodies are http.StatusText(status)+"\n" (the cause is for the log, not the client), with
// what a *PartialError deleted before it failed (ClientMessage).
// A 401 carries WWW-Authenticate: Basic realm="lajstrom", a 405 Allow: GET, HEAD, a 503 of
// provider.ErrUnavailable, of index.ErrUnavailable or of an *UnavailableError Retry-After
// (SetRetryAfter). See StatusOf for the mapping.
func WriteError(w http.ResponseWriter, err error) int {
	status := StatusOf(err)
	msg := ClientMessage(err)
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

// indexRetryAfter is the Retry-After of index.ErrUnavailable: a database that restarts or
// fails over takes seconds.
const indexRetryAfter = 5 * time.Second

// stalledMessage is the message of a 408: the cause names the connection's addresses, and the
// registry's URL when the body streamed into it.
const stalledMessage = "the request body stopped: no byte came within the server's stall timeout"

// ClientMessage is the message an error answer gives the client: err.Error() for a 4xx but a
// 408, stalledMessage for a 408, the status text for a 5xx, whose cause is for the log,
// followed for a 5xx by what a *PartialError in err deleted before it failed, so that a client
// learns it whatever the status (docs/spec/guarantees.md, INV-13).
func ClientMessage(err error) string {
	status := StatusOf(err)
	switch {
	case status == http.StatusRequestTimeout:
		return stalledMessage
	case status < http.StatusInternalServerError:
		return err.Error()
	}
	msg := http.StatusText(status)
	var p *PartialError
	if errors.As(err, &p) {
		msg += "; " + p.deleted()
	}
	return msg
}

// PartialError is the error of a delete of several manifests that failed part way: a raw path,
// an npm package or the versions an npm packument update leaves out, a version with every
// variant. Err is the failure, which decides the status; Deleted names what was deleted before
// it, which stays deleted.
type PartialError struct {
	// Deleted names the versions or variants deleted before the failure, each with its
	// manifest digest: "1.0.0 (sha256:…)".
	Deleted []string
	// Err is the failure.
	Err error
}

func (e *PartialError) Error() string { return e.Err.Error() + "; " + e.deleted() }

// Unwrap returns Err.
func (e *PartialError) Unwrap() error { return e.Err }

// deleted is the part of the message that names Deleted.
func (e *PartialError) deleted() string {
	return "deleted before the failure, and still deleted: " + strings.Join(e.Deleted, ", ")
}

// Partial returns err as a *PartialError naming deleted, or err itself when nothing was
// deleted.
func Partial(err error, deleted []string) error {
	if err == nil || len(deleted) == 0 {
		return err
	}
	return &PartialError{Deleted: deleted, Err: err}
}

// SetRetryAfter sets Retry-After in h when err says when to try again: a
// provider.ErrUnavailable of an open circuit breaker, whose time left until its trial request
// becomes whole seconds, at least 1, an *UnavailableError of an upstream that asked to wait, or
// index.ErrUnavailable, 5 seconds. Every writer of error answers calls it (WriteError, the OCI
// facade, the management API, the authentication middleware), so that the header is set
// wherever StatusOf answers 503.
func SetRetryAfter(h http.Header, err error) {
	after, ok := provider.RetryAfter(err)
	var u *UnavailableError
	if !ok && errors.As(err, &u) {
		after, ok = u.RetryAfter, true
	}
	if !ok && errors.Is(err, index.ErrUnavailable) {
		after, ok = indexRetryAfter, true
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
//	os.ErrDeadlineExceeded                     408 Request Timeout, with a fixed message: the client
//	                                             sent no byte of the body within server.stallTimeout
//	                                             (stalled), also where a plugin made it ErrInvalid
//	ErrInvalid                                 400 Bad Request
//	ErrUnauthenticated                         401 Unauthorized
//	ErrForbidden, provider.ErrLimit            403 Forbidden (the caller lacks the action; the
//	                                             registry refused the write for a limit or a quota
//	                                             of its own, which the message names, #357)
//	ErrMethodNotAllowed                        405 Method Not Allowed, with Allow: GET, HEAD
//	index.ErrNotFound, provider.ErrNotFound    404 Not Found
//	index.ErrConflict, provider.ErrConflict    409 Conflict
//	ErrPrecondition                            412 Precondition Failed
//	*http.MaxBytesError,                       413 Content Too Large
//	  *provider.RejectedError of a 413           (the backend's: the file is too large for it)
//	ErrUnsupported, provider.ErrUnsupported    501 Not Implemented
//	provider.ErrUnauthorized,                  502 Bad Gateway (the backend rejected our
//	  *provider.RejectedError not busy           credentials, or what Lajstrom sent it, #494)
//	provider.ErrUnavailable                    503 Service Unavailable, with Retry-After (SetRetryAfter):
//	                                           an open circuit breaker, or the backend's 429 or 503
//	index.ErrUnavailable                       503 Service Unavailable, with Retry-After: 5 (the database
//	                                             did not answer)
//	ErrBadGateway                              502 Bad Gateway (a proxy's upstream failed)
//	ErrUnavailable                             503 Service Unavailable, with Retry-After (the upstream's)
//	ErrGatewayTimeout                          504 Gateway Timeout
//	anything else, nil included                500 Internal Server Error
func StatusOf(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	if status, ok := clientStatus(err); ok {
		return status
	}
	return serverStatus(err)
}

// clientStatus returns the 4xx of the table of StatusOf that err has, in its order.
func clientStatus(err error) (int, bool) {
	var tooLarge *http.MaxBytesError
	switch {
	case stalled(err):
		return http.StatusRequestTimeout, true
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest, true
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized, true
	case errors.Is(err, ErrForbidden), errors.Is(err, provider.ErrLimit):
		return http.StatusForbidden, true
	case errors.Is(err, ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed, true
	case errors.Is(err, index.ErrNotFound), errors.Is(err, provider.ErrNotFound):
		return http.StatusNotFound, true
	case errors.Is(err, index.ErrConflict), errors.Is(err, provider.ErrConflict):
		return http.StatusConflict, true
	case errors.Is(err, ErrPrecondition):
		return http.StatusPreconditionFailed, true
	case errors.As(err, &tooLarge), refusedBy(err, http.StatusRequestEntityTooLarge):
		return http.StatusRequestEntityTooLarge, true
	}
	return 0, false
}

// stalled reports whether err comes from a read of the request body that server.stallTimeout
// cut: the read deadline of the client's connection passed (internal/server, stall), which is
// the client's doing (RFC 9110 §15.5.9). Lajstrom sets a deadline on no other connection, so
// os.ErrDeadlineExceeded is always the client's: a write deadline of the same bound is a client
// that stopped reading the answer.
func stalled(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// serverStatus returns the 5xx of the table of StatusOf that err has, in its order, 500 when
// none.
func serverStatus(err error) int {
	switch {
	case errors.Is(err, ErrUnsupported), errors.Is(err, provider.ErrUnsupported):
		return http.StatusNotImplemented
	case errors.Is(err, provider.ErrUnauthorized), refusedBy(err, 0):
		return http.StatusBadGateway
	case errors.Is(err, provider.ErrUnavailable), errors.Is(err, index.ErrUnavailable):
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
