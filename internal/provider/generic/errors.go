package generic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

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
	kind   error  // provider.ErrNotFound, ErrConflict, ErrUnsupported, ErrUnauthorized, ErrUnavailable or a *RejectedError
	detail string // "HTTP 404, MANIFEST_UNKNOWN"; empty when the cause was not an HTTP answer
	cause  error
	also   error // provider.ErrManifestUnknown from StatManifest (manifestUnknown); nil otherwise
}

func (e *backendError) Error() string {
	msg := "generic: " + e.op + ": " + e.kind.Error()
	if e.detail != "" {
		msg += " (" + e.detail + ")"
	}
	return msg
}

func (e *backendError) Unwrap() []error {
	if e.also != nil {
		return []error{e.kind, e.also, e.cause}
	}
	return []error{e.kind, e.cause}
}

// credentialError is the failure of a rotating credential (Options.Credential), as the auth
// client met it; translate makes it ErrUnauthorized.
type credentialError struct{ err error }

func (e *credentialError) Error() string { return "credential: " + e.err.Error() }

func (e *credentialError) Unwrap() error { return e.err }

// translate is translateAs for the registry of p, with the wait it last asked for
// (breaker.waitLeft).
func (p *Provider) translate(op string, err error) error {
	return translateAs(p.name, p.clients.breaker.waitLeft, op, err)
}

// translateAs turns an error of oras-go or of a registry response into one that wraps a
// sentinel of package provider where one applies:
//
//   - errdef.ErrNotFound, HTTP 404, NAME_UNKNOWN, MANIFEST_UNKNOWN, BLOB_UNKNOWN,
//     BLOB_UPLOAD_UNKNOWN: ErrNotFound;
//   - HTTP 409, TAG_INVALID, or an error whose message says the tag is immutable: ErrConflict;
//   - HTTP 405, UNSUPPORTED, errdef.ErrUnsupported: ErrUnsupported;
//   - HTTP 401 and 403, UNAUTHORIZED, DENIED, and a Basic challenge without configured
//     credentials: ErrUnauthorized, the registry rejected Lajstrom's own credentials;
//   - a rotating credential that failed (Options.Credential): ErrUnauthorized, with the
//     credential's error in the message instead of oras-go's, which names the URL;
//   - any other 4xx, and a 503: a *provider.RejectedError of the registry named registry, with
//     the first error code of the answer; one of a 429 or 503 is ErrUnavailable too and waits
//     what wait returns, the Retry-After the registry last sent: oras-go's error has no header;
//   - a *net.DNSError, the registry's host name (or its token realm's, or a redirect's) that
//     does not resolve, "no such host" or a temporary failure: ErrUnavailable, without a wait
//     (#561). The message leaves out the name; errors.As still finds the *net.DNSError.
//
// A refused token realm (realmError) is wrapped without what net/http and oras-go put around
// it, which names the request URL. Everything else, context errors included, is wrapped with
// %w and the operation. nil stays nil. errdef.ErrAlreadyExists is not translated: the call
// sites where it means success drop it.
func translateAs(registry string, wait func() time.Duration, op string, err error) error {
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
		if refused(resp.StatusCode) {
			rej := &provider.RejectedError{Registry: registry, Status: resp.StatusCode}
			if len(resp.Errors) > 0 {
				rej.Code = resp.Errors[0].Code
			}
			if rej.Busy() {
				rej.RetryAfter = wait()
			}
			return &backendError{op: op, kind: rej, cause: err}
		}
		return fmt.Errorf("generic: %s: %w", op, err)
	}
	var dns *net.DNSError
	switch {
	case errors.Is(err, errdef.ErrNotFound):
		return &backendError{op: op, kind: provider.ErrNotFound, cause: err}
	case errors.Is(err, errdef.ErrUnsupported):
		return &backendError{op: op, kind: provider.ErrUnsupported, cause: err}
	case errors.Is(err, auth.ErrBasicCredentialNotFound):
		return &backendError{op: op, kind: provider.ErrUnauthorized, detail: "no credentials configured", cause: err}
	case errors.As(err, &dns):
		return &backendError{op: op, kind: provider.ErrUnavailable, detail: "the host name does not resolve", cause: err}
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

// refused reports whether a registry's status that no sentinel names is its refusal of the
// request (provider.RejectedError): a 4xx, or a 503; any other 5xx is its failure.
func refused(status int) bool {
	return status >= 400 && status <= 499 || status == http.StatusServiceUnavailable
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
