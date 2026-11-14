package generic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/egress"
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
	kind   error  // provider.ErrNotFound, ErrConflict, ErrUnsupported, ErrUnauthorized, ErrLimit, ErrUnavailable or a *RejectedError
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
// (breaker.waitLeft), and the error rule of an upstream (upstreamError). A limit or a quota of
// the registry (provider.ErrLimit) is logged at warn: it is the operator's to raise, and the
// client's answer, a 4xx, is not logged elsewhere.
func (p *Provider) translate(op string, err error) error {
	out := translateAs(p.name, p.clients.breaker.waitLeft, op, err)
	switch {
	case p.upstream:
		return p.upstreamError(op, out, err)
	case errors.Is(out, provider.ErrLimit):
		p.log.Warn("registry refused a write for a limit or a quota of its own; raise it or free room", "error", out.Error())
	}
	return out
}

// translateAs turns an error of oras-go or of a registry response into one that wraps a
// sentinel of package provider where one applies:
//
//   - errdef.ErrNotFound, HTTP 404, NAME_UNKNOWN, MANIFEST_UNKNOWN, BLOB_UNKNOWN,
//     BLOB_UPLOAD_UNKNOWN: ErrNotFound;
//   - HTTP 409, TAG_INVALID, or an error whose message says the tag is immutable: ErrConflict;
//   - HTTP 405, UNSUPPORTED, errdef.ErrUnsupported: ErrUnsupported;
//   - a 403 or DENIED for a limit or a quota of the registry's own (limitOf): ErrLimit, with
//     the registry's configured name and the limit in the message (#357);
//   - any other HTTP 401 and 403, UNAUTHORIZED, DENIED, and a Basic challenge without
//     configured credentials: ErrUnauthorized, the registry rejected Lajstrom's own credentials;
//   - a rotating credential that failed (Options.Credential): ErrUnauthorized, with the
//     credential's error in the message instead of oras-go's, which names the URL;
//   - any other 4xx, and a 503: a *provider.RejectedError of the registry named registry, with
//     the first error code of the answer; one of a 429 or 503 is ErrUnavailable too and waits
//     what wait returns, the Retry-After the registry last sent: oras-go's error has no header;
//   - a *net.DNSError, the registry's host name (or its token realm's, or a redirect's) that
//     does not resolve, "no such host" or a temporary failure: ErrUnavailable, without a wait
//     (#561). The message leaves out the name; errors.As still finds the *net.DNSError.
//   - a connection that the registry (or its token realm, or a redirect's host) refused
//     (ECONNREFUSED), or reset (ECONNRESET, or EPIPE on a write after the reset), after the
//     retries: ErrUnavailable, without a wait (#593). So is net.ErrClosed, a write of the
//     request body on a connection that net/http closed as its read met the reset first, the
//     way an upload sees a reset. errors.Is still finds the cause.
//   - a host or a network that cannot be reached (EHOSTUNREACH, ENETUNREACH), and, after the
//     retries, a request that timed out (the dial, the TLS handshake, the wait for the response
//     headers) or met an EOF (the registry closed the connection without an answer) and that
//     the breaker counted as the registry's failure (*noAnswerError): ErrUnavailable, without a
//     wait (#670). A timeout or an EOF of the caller's context, of a client's body (#643), of
//     a request that waited on a client's body (#648), or of an answer's body is not the
//     registry's and stays as it is.
//
// A refused token realm (realmError), and a redirect or a dial that a guard refused
// (egress.ErrRefused, Options.PublicRedirects), are wrapped without what net/http and oras-go
// put around them, which names the request URL. Everything else, context errors included, is wrapped with
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
	if errors.Is(err, egress.ErrRefused) {
		var ue *url.Error // names the URL, which may be presigned: never in an error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("generic: %s: %w", op, err)
	}
	var ce *credentialError
	if errors.As(err, &ce) {
		return &backendError{op: op, kind: provider.ErrUnauthorized, detail: ce.Error(), cause: err}
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		if kind := classifyResponse(resp); kind != nil {
			return &backendError{op: op, kind: kind, detail: detailOf(registry, kind, resp), cause: err}
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
	switch {
	case errors.Is(err, errdef.ErrNotFound):
		return &backendError{op: op, kind: provider.ErrNotFound, cause: err}
	case errors.Is(err, errdef.ErrUnsupported):
		return &backendError{op: op, kind: provider.ErrUnsupported, cause: err}
	case errors.Is(err, auth.ErrBasicCredentialNotFound):
		return &backendError{op: op, kind: provider.ErrUnauthorized, detail: "no credentials configured", cause: err}
	}
	if detail := unreachable(err); detail != "" {
		return &backendError{op: op, kind: provider.ErrUnavailable, detail: detail, cause: err}
	}
	return fmt.Errorf("generic: %s: %w", op, err)
}

// unreachable says why err did not reach the registry, or got no answer from it: a host name
// that does not resolve, a connection refused or reset, a host or network that cannot be
// reached, a timeout or an EOF that the breaker counted against the registry (translateAs); or
// returns "".
func unreachable(err error) string {
	var dns *net.DNSError
	var quiet *noAnswerError
	switch {
	case errors.As(err, &dns):
		return "the host name does not resolve"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "the connection was reset"
	case errors.Is(err, net.ErrClosed):
		return "the connection broke during the request"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "the host cannot be reached"
	case errors.As(err, &quiet):
		return quiet.detail()
	}
	return ""
}

// classifyResponse returns the sentinel for a registry's error response, or nil. A conflict is
// checked first: registries answer a write to an immutable tag with 400, 403 or 409. A limit
// comes before the credentials, whose 403 and DENIED it shares (limitOf).
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
	case limitOf(resp) != "":
		return provider.ErrLimit
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden,
		hasCode(resp, errcode.ErrorCodeUnauthorized, errcode.ErrorCodeDenied):
		return provider.ErrUnauthorized
	}
	return nil
}

// The limits limitOf names.
const (
	limitQuota = "its quota"
	limitSize  = "its size limit or its quota"
)

// limitOf names the limit or the quota of its own for which the registry refused a write, or
// returns "" (#357). Only a 403, or a DENIED with any status but 401, can be one:
//
//   - one whose message speaks of a quota or of exceeding a limit, on any request: Harbor's
//     project quota, "… will exceed the configured upper limit of 1.0 MiB" (measured), and
//     Quay's "Quota has been exceeded on namespace";
//   - a 403 to a PATCH or to the closing PUT of an upload session, which the registry opened
//     for the same credentials (inUpload): Gitea's and Forgejo's LIMIT_SIZE_CONTAINER, a 403
//     with an empty body (measured), which cannot be told from their owner's quota. One with
//     UNAUTHORIZED, or whose message speaks of the token or the credentials (ECR's expired
//     token is a 403 DENIED), stays the credentials'.
func limitOf(resp *errcode.ErrorResponse) string {
	denied := resp.StatusCode == http.StatusForbidden || hasCode(resp, errcode.ErrorCodeDenied)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || !denied:
		return ""
	case mentions(resp, speaksOfQuota):
		return limitQuota
	case resp.StatusCode == http.StatusForbidden && inUpload(resp) && !hasCode(resp, errcode.ErrorCodeUnauthorized) &&
		!mentions(resp, speaksOfCredentials):
		return limitSize
	}
	return ""
}

// detailOf is the detail of a response that classifyResponse made kind: responseDetail, or
// limitDetail for a limit.
func detailOf(registry string, kind error, resp *errcode.ErrorResponse) string {
	if errors.Is(kind, provider.ErrLimit) {
		return limitDetail(registry, resp)
	}
	return responseDetail(resp)
}

// limitDetail is the detail of a limit's error: the registry by its configured name, the
// status and codes of the answer, and the limit, never the registry's own message.
func limitDetail(registry string, resp *errcode.ErrorResponse) string {
	limit := limitOf(resp)
	if limit == limitSize {
		return "registry " + registry + ", an upload it had opened, " + responseDetail(resp) + ": " + limit
	}
	return "registry " + registry + ", " + responseDetail(resp) + ": " + limit
}

// inUpload reports whether resp answers a request of an upload session the registry had
// opened: a PATCH, or a PUT that is not a manifest's, the session's closing PUT. Its location
// is the registry's: /blobs/uploads/<id> mostly, /blobs/upload/<id> on GHCR.
func inUpload(resp *errcode.ErrorResponse) bool {
	switch resp.Method {
	case http.MethodPatch:
		return true
	case http.MethodPut:
		return resp.URL != nil && operation(resp.URL.Path) != "manifest"
	}
	return false
}

// mentions reports whether the message of one of the response's errors satisfies says, in
// lower case.
func mentions(resp *errcode.ErrorResponse, says func(lower string) bool) bool {
	for _, e := range resp.Errors {
		if says(strings.ToLower(e.Message)) {
			return true
		}
	}
	return false
}

// speaksOfQuota reports whether a message speaks of a quota or of exceeding a limit.
func speaksOfQuota(msg string) bool {
	return strings.Contains(msg, "quota") || strings.Contains(msg, "exceed") && strings.Contains(msg, "limit")
}

// speaksOfCredentials reports whether a message speaks of the token, the credentials or the
// permissions of the caller.
func speaksOfCredentials(msg string) bool {
	for _, w := range []string{"token", "credential", "authoriz", "authentic", "permission", "expired"} {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
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
	return mentions(resp, func(msg string) bool { return strings.Contains(msg, "immutable") })
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
