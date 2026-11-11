package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/server"
)

// clientError is an error whose message is written for the client: the API answers exactly
// this message, whatever the status. err, the cause, keeps the sentinel that decides the status
// through format.StatusOf.
type clientError struct {
	msg string
	err error
}

// Error returns the message for the client.
func (e *clientError) Error() string { return e.msg }

// Unwrap returns the cause.
func (e *clientError) Unwrap() error { return e.err }

// invalid returns a *clientError wrapping format.ErrInvalid (400 INVALID) with the message
// fmt.Sprintf(msg, args...), which starts with the field it is about.
func invalid(msg string, args ...any) error {
	return &clientError{msg: fmt.Sprintf(msg, args...), err: format.ErrInvalid}
}

// envelope is the body of every error answer (docs/spec/http-routing.md §4):
// {"error": {"code": "…", "message": "…"}}.
type envelope struct {
	Error envelopeError `json:"error"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Codes of the error envelope (docs/spec/http-routing.md §4).
const (
	codeInvalid     = "INVALID"
	codeForbidden   = "FORBIDDEN"
	codeNotFound    = "NOT_FOUND"
	codeConflict    = "CONFLICT"
	codeUnavailable = "UNAVAILABLE"
	codeInternal    = "INTERNAL"
)

// codeOf returns the envelope code of an error status. There is no code of its own for 401 in
// this version: it is FORBIDDEN, told apart by the status and the Basic challenge. 412, a
// conditional delete that found the version changed, is CONFLICT as 409 is. Every other 4xx,
// 413 included, is INVALID; 501, 502 and 503 are UNAVAILABLE, any other 5xx INTERNAL.
func codeOf(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return codeForbidden
	case http.StatusNotFound:
		return codeNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		return codeConflict
	case http.StatusNotImplemented, http.StatusBadGateway, http.StatusServiceUnavailable:
		return codeUnavailable
	}
	if status < http.StatusInternalServerError {
		return codeInvalid
	}
	return codeInternal
}

// basicChallenge is the WWW-Authenticate header of a 401, the one format.WriteError sends on
// the frontends (docs/spec/rbac.md §3), so that clients send credentials.
const basicChallenge = `Basic realm="lajstrom"`

// fail answers err with the error envelope. The status is format.StatusOf(err), the one error
// table of Lajstrom's HTTP answers, and the code follows from it (codeOf). The message is
// err.Error() for a 4xx and for a *clientError, whose message is written for the client (the
// 501 of an endpoint that comes later); any other 5xx answers only its status text, and its
// cause is logged with the request id. A 401 carries the Basic challenge, and a 503 of a
// registry whose circuit breaker is open, or of a database that does not answer, Retry-After
// (format.SetRetryAfter).
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := format.StatusOf(err)
	msg := err.Error()
	var ce *clientError
	if status >= http.StatusInternalServerError && !errors.As(err, &ce) {
		msg = http.StatusText(status)
		h.log.ErrorContext(r.Context(), "api: request failed", "method", r.Method, "path", r.URL.Path,
			"status", status, "requestId", server.RequestID(r.Context()), "error", err.Error())
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", basicChallenge)
	}
	format.SetRetryAfter(w.Header(), err)
	h.writeJSON(w, r, status, envelope{Error: envelopeError{Code: codeOf(status), Message: msg}})
}

// internalBody is the answer when an answer cannot be encoded.
const internalBody = `{"error":{"code":"` + codeInternal + `","message":"Internal Server Error"}}`

// writeJSON answers status with v as the JSON body.
func (h *handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.log.ErrorContext(r.Context(), "api: answer not encoded", "method", r.Method, "path", r.URL.Path,
			"requestId", server.RequestID(r.Context()), "error", err.Error())
		status, body = http.StatusInternalServerError, []byte(internalBody)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n')) // a write error means the client went away
}
