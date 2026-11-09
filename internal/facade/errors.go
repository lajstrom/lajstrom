package facade

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The error codes of the distribution spec ("Error Codes") that the facade answers with, and
// TAG_INVALID, distribution's code for a tag that cannot be written, which clients know.
const (
	codeBlobUnknown         = "BLOB_UNKNOWN"
	codeBlobUploadInvalid   = "BLOB_UPLOAD_INVALID"
	codeBlobUploadUnknown   = "BLOB_UPLOAD_UNKNOWN"
	codeDigestInvalid       = "DIGEST_INVALID"
	codeManifestBlobUnknown = "MANIFEST_BLOB_UNKNOWN"
	codeManifestInvalid     = "MANIFEST_INVALID"
	codeManifestUnknown     = "MANIFEST_UNKNOWN"
	codeNameInvalid         = "NAME_INVALID"
	codeNameUnknown         = "NAME_UNKNOWN"
	codeSizeInvalid         = "SIZE_INVALID"
	codeUnauthorized        = "UNAUTHORIZED"
	codeDenied              = "DENIED"
	codeUnsupported         = "UNSUPPORTED"
	codeTooManyRequests     = "TOOMANYREQUESTS"
	codeTagInvalid          = "TAG_INVALID"
	// codeUnavailable is distribution's code of a 503, which docker retries: a registry whose
	// circuit breaker is open.
	codeUnavailable = "UNAVAILABLE"
	// codeUnknown is distribution's code of a 5xx; the spec defines codes for 4xx answers only.
	codeUnknown = "UNKNOWN"
)

// apiError is an error the facade answers in the distribution format: the code that the
// handler chose for what it was doing, the status (zero: format.StatusOf of the cause, the one
// error table of the frontends, so that index and provider errors map as everywhere), the
// message for the client (empty: the cause's), the detail of the error entry (nil: none), and
// the cause, for the status and the log. insufficient marks the 401 of a valid bearer that
// lacks the repository: its challenge says error="insufficient_scope".
type apiError struct {
	status       int
	code         string
	msg          string
	detail       any
	err          error
	insufficient bool
}

func (e *apiError) Error() string {
	switch {
	case e.err == nil:
		return e.msg
	case e.msg == "":
		return e.err.Error()
	}
	return e.msg + ": " + e.err.Error()
}

func (e *apiError) Unwrap() error { return e.err }

// methodNotAllowed is the answer to a method a route does not have.
func methodNotAllowed(r *http.Request) error {
	return &apiError{status: http.StatusMethodNotAllowed, code: codeUnsupported,
		msg: fmt.Sprintf("%s is not supported on %s", r.Method, r.URL.Path)}
}

// errorBody is the error document of the distribution spec.
type errorBody struct {
	Errors []errorEntry `json:"errors"`
}

type errorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// fail answers err as the distribution spec's error document. The status is the apiError's,
// or format.StatusOf of the cause; the code follows the status where the status decides it
// (401 UNAUTHORIZED with the challenge, 403 DENIED, 429 TOOMANYREQUESTS, 501 UNSUPPORTED, the
// 503 of an open circuit breaker UNAVAILABLE with Retry-After, other 5xx UNKNOWN) and is the
// handler's otherwise. The challenge of a 401 is the bearer challenge of
// the token flow (docs/spec/rbac.md §4) with the scope of the request (requestScope), and
// error="insufficient_scope" when a valid bearer lacked it. A 5xx says only its status text,
// without a detail, and its cause is logged with the request id: a backend error must not
// reach the client.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{err: err}
	}
	status := ae.status
	if status == 0 {
		status = format.StatusOf(ae.err)
	}
	code, msg, detail := ae.code, ae.msg, ae.detail
	if msg == "" && ae.err != nil {
		msg = ae.err.Error()
	}
	format.SetRetryAfter(w.Header(), err)
	switch {
	case status == http.StatusServiceUnavailable && errors.Is(err, provider.ErrUnavailable):
		code, msg, detail = codeUnavailable, http.StatusText(status), nil
		h.o.Log.WarnContext(r.Context(), "facade: registry unavailable", "method", r.Method, "path", r.URL.Path,
			"error", err.Error(), "requestId", server.RequestID(r.Context()))
	case status >= http.StatusInternalServerError && status != http.StatusNotImplemented:
		code, msg, detail = codeUnknown, http.StatusText(status), nil
		h.o.Log.ErrorContext(r.Context(), "facade: request failed", "method", r.Method, "path", r.URL.Path,
			"status", status, "error", err.Error(), "requestId", server.RequestID(r.Context()))
	case status == http.StatusUnauthorized:
		code = codeUnauthorized
		realm, service := token.Realm(h.o.ExternalURL, r)
		w.Header().Set("WWW-Authenticate", token.Challenge(realm, service, requestScope(r), ae.insufficient))
	case status == http.StatusForbidden:
		code = codeDenied
	case status == http.StatusTooManyRequests:
		code = codeTooManyRequests
	case status == http.StatusNotImplemented:
		code = codeUnsupported
	case code == "":
		code = codeOfStatus(status)
	}
	writeJSON(w, status, errorBody{Errors: []errorEntry{{Code: code, Message: msg, Detail: detail}}})
}

// codeOfStatus is the code of an error whose handler named none.
func codeOfStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return codeNameUnknown
	case http.StatusRequestEntityTooLarge:
		return codeSizeInvalid
	case http.StatusConflict:
		return codeTagInvalid
	}
	return codeUnsupported
}

// writeJSON answers status with v as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil { // the values are the facade's own structs and string slices
		body, status = []byte(`{"errors":[{"code":"UNKNOWN","message":"Internal Server Error"}]}`), http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)+1))
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
