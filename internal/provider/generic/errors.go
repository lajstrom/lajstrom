package generic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// maxErrorBody bounds how much of an error response is read to find its error codes.
const maxErrorBody = 8 << 10

// backendError is a registry error translated into a sentinel of package provider. Its message
// names the operation, the Lajstrom name and the status and error codes of the response, and
// leaves out the backend URL, the instance prefix and the registry's own message (which can
// carry the prefixed name): format.WriteError puts the message of a 4xx error in the response,
// and clients never see the prefix (docs/spec/http-routing.md §2). errors.Is matches the
// sentinel, and errors.As still finds the *errcode.ErrorResponse or the oras-go error.
type backendError struct {
	op     string // "fetch raw/scratch/notes@sha256:…"
	kind   error  // provider.ErrNotFound, ErrConflict, ErrUnsupported, ErrUnauthorized or a *RejectedError
	detail string // "HTTP 404, MANIFEST_UNKNOWN"; empty when the cause was not an HTTP answer
	cause  error
}

func (e *backendError) Error() string {
	msg := "generic: " + e.op + ": " + e.kind.Error()
	if e.detail != "" {
		msg += " (" + e.detail + ")"
	}
	return msg
}

func (e *backendError) Unwrap() []error { return []error{e.kind, e.cause} }

// credentialError is the failure of a rotating credential (Options.Credential), as the auth
// client met it; translate makes it ErrUnauthorized.
type credentialError struct{ err error }

func (e *credentialError) Error() string { return "credential: " + e.err.Error() }

func (e *credentialError) Unwrap() error { return e.err }

// translate turns an error of oras-go or of a registry response into one that wraps a sentinel
// of package provider where one applies:
//
//   - errdef.ErrNotFound, HTTP 404, NAME_UNKNOWN, MANIFEST_UNKNOWN, BLOB_UNKNOWN,
//     BLOB_UPLOAD_UNKNOWN: ErrNotFound;
//   - HTTP 409, TAG_INVALID, or an error whose message says the tag is immutable: ErrConflict;
//   - HTTP 405, UNSUPPORTED, errdef.ErrUnsupported: ErrUnsupported;
//   - HTTP 401 and 403, UNAUTHORIZED, DENIED, and a Basic challenge without configured
//     credentials: ErrUnauthorized, the registry rejected Lajstrom's own credentials;
//   - a rotating credential that failed (Options.Credential): ErrUnauthorized, with the
//     credential's error in the message instead of oras-go's, which names the URL.
//
// A refused token realm (realmError) is wrapped without what net/http and oras-go put around
// it, which names the request URL. Everything else, context errors included, is wrapped with
// %w and the operation. nil stays nil. errdef.ErrAlreadyExists is not translated: the call
// sites where it means success drop it.
func translate(op string, err error) error {
	if err == nil {
		return nil
	}
	var realm *realmError
	if errors.As(err, &realm) {
		return fmt.Errorf("generic: %s: %w", op, realm)
	}
	var ce *credentialError
	if errors.As(err, &ce) {
		return &backendError{op: op, kind: provider.ErrUnauthorized, detail: ce.Error(), cause: err}
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		if kind := classifyResponse(resp); kind != nil {
			return &backendError{op: op, kind: kind, detail: responseDetail(resp), cause: err}
		}
		return fmt.Errorf("generic: %s: %w", op, err)
	}
	switch {
	case errors.Is(err, errdef.ErrNotFound):
		return &backendError{op: op, kind: provider.ErrNotFound, cause: err}
	case errors.Is(err, errdef.ErrUnsupported):
		return &backendError{op: op, kind: provider.ErrUnsupported, cause: err}
	case errors.Is(err, auth.ErrBasicCredentialNotFound):
		return &backendError{op: op, kind: provider.ErrUnauthorized, detail: "no credentials configured", cause: err}
	}
	return fmt.Errorf("generic: %s: %w", op, err)
}

// classifyResponse returns the sentinel for a registry's error response, or nil. A conflict is
// checked first: registries answer a write to an immutable tag with 400, 403 or 409.
func classifyResponse(resp *errcode.ErrorResponse) error {
	switch {
	case resp.StatusCode == http.StatusConflict || hasCode(resp, "TAG_INVALID") || mentionsImmutable(resp):
		return provider.ErrConflict
	case resp.StatusCode == http.StatusNotFound,
		hasCode(resp, errcode.ErrorCodeNameUnknown, errcode.ErrorCodeManifestUnknown, errcode.ErrorCodeBlobUnknown,
			errcode.ErrorCodeBlobUploadUnknown):
		return provider.ErrNotFound
	case resp.StatusCode == http.StatusMethodNotAllowed, hasCode(resp, errcode.ErrorCodeUnsupported):
		return provider.ErrUnsupported
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden,
		hasCode(resp, errcode.ErrorCodeUnauthorized, errcode.ErrorCodeDenied):
		return provider.ErrUnauthorized
	}
	return nil
}

// translateSession is translate for the answer resp of a request of an upload session, whose
// error codes er holds: a 4xx that no sentinel names is a *provider.RejectedError with its first
// error code and the Retry-After of a 429, which the facade passes on to the client.
func (p *Provider) translateSession(op string, resp *http.Response, er *errcode.ErrorResponse) error {
	if er.StatusCode < 400 || er.StatusCode > 499 || classifyResponse(er) != nil {
		return translate(op, er)
	}
	rej := &provider.RejectedError{Registry: p.name, Status: er.StatusCode}
	if len(er.Errors) > 0 {
		rej.Code = er.Errors[0].Code
	}
	rej.RetryAfter, _ = retryAfter(resp)
	return &backendError{op: op, kind: rej, cause: er}
}

// hasCode reports whether one of the response's errors has one of codes.
func hasCode(resp *errcode.ErrorResponse, codes ...string) bool {
	for _, e := range resp.Errors {
		for _, c := range codes {
			if e.Code == c {
				return true
			}
		}
	}
	return false
}

// mentionsImmutable reports whether an error of the response says that a tag is immutable,
// the way registries with tag immutability refuse to re-point one.
func mentionsImmutable(resp *errcode.ErrorResponse) bool {
	for _, e := range resp.Errors {
		if strings.Contains(strings.ToLower(e.Message), "immutable") {
			return true
		}
	}
	return false
}

// responseDetail is "HTTP <status>" followed by the error codes of the response.
func responseDetail(resp *errcode.ErrorResponse) string {
	parts := []string{fmt.Sprintf("HTTP %d", resp.StatusCode)}
	for _, e := range resp.Errors {
		parts = append(parts, e.Code)
	}
	return strings.Join(parts, ", ")
}

// parseErrorResponse reads the error codes of a registry response that did not succeed, as
// oras-go does for the requests it sends; the requests this package sends itself
// (capability probes, OpenBlob, StatBlob) use it so that translate treats both alike.
func parseErrorResponse(resp *http.Response) *errcode.ErrorResponse {
	e := &errcode.ErrorResponse{StatusCode: resp.StatusCode}
	if resp.Request != nil {
		e.Method, e.URL = resp.Request.Method, resp.Request.URL
	}
	var body struct {
		Errors errcode.Errors `json:"errors"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&body) == nil {
		e.Errors = body.Errors
	}
	return e
}
