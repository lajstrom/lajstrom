package format

import (
	"errors"
	"time"
)

var (
	// ErrInvalid means a manifest or an upload does not satisfy the manifest profile or the
	// format's own rules (docs/spec/manifest-profile.md §5). Frontends answer 400, the facade
	// MANIFEST_INVALID.
	ErrInvalid = errors.New("format: invalid")

	// ErrUnsupported means the plugin does not implement the operation, for example Build for a
	// format that is only published with oras push.
	ErrUnsupported = errors.New("format: unsupported")

	// ErrUnauthenticated means the request has no principal and needs one: Authorize denied the
	// anonymous principal, or the repo middleware found no repo for it. Frontends answer 401 with
	// a Basic challenge.
	ErrUnauthenticated = errors.New("format: authentication required")

	// ErrForbidden means Authorize denied a principal that can read the repo. Frontends answer
	// 403; a principal without any access gets index.ErrNotFound (docs/spec/rbac.md §3).
	ErrForbidden = errors.New("format: forbidden")

	// ErrPrecondition means a conditional request found the resource changed since the client
	// read it: a conditional delete of a version (Condition, RefusedError). The management API
	// answers 412.
	ErrPrecondition = errors.New("format: precondition failed")

	// ErrStreamed is the error of a BlobFetcher asked, during Validate, for the file of the
	// upload being validated, which streams into the registry and is committed after the checks
	// (ADR 0014, Publisher.UploadStream): Build described it from what the plugin's inspector
	// read of those bytes, and a Validate that would read the file takes that as checked.
	ErrStreamed = errors.New("format: the blob streams in with this upload and is not committed yet")

	// ErrReadBack is what Build returns, wrapped, for a file that streamed into the registry
	// (Publisher.UploadStream) when its inspector did not hold all it needs of the bytes as they
	// passed: a wheel whose METADATA it did not keep (ADR 0014, step 5). Ingest then closes the
	// upload session, so that the registry has the file, and calls Build again with the file's
	// Open reading it back from there; Validate's fetch of the file then reads it too, and is no
	// ErrStreamed. It is never a client's answer.
	ErrReadBack = errors.New("format: the file is to be read back from the registry")
)

// The errors of a proxy's upstream and of a repo that takes no writes (ADR 0013,
// docs/spec/proxy-and-group.md §6). A message names the upstream by its configured name, never
// its URL, a credential or a body the upstream sent.
var (
	// ErrMethodNotAllowed means the repo takes no write of this kind: a write route of a proxy,
	// or of a group without a hosted member. Frontends answer 405 with Allow: GET, HEAD.
	ErrMethodNotAllowed = errors.New("format: method not allowed")

	// ErrBadGateway means the upstream failed: an error status, a connection or TLS error, a
	// refused redirect, Lajstrom's credentials refused, a document too large or unreadable, or
	// bytes that fail their checksum or Validate. Frontends answer 502.
	ErrBadGateway = errors.New("format: bad gateway")

	// ErrUnavailable means the upstream asked to wait (429). Frontends answer 503 with the
	// Retry-After of the *UnavailableError that wraps it.
	ErrUnavailable = errors.New("format: upstream unavailable")

	// ErrGatewayTimeout means the upstream did not answer within its timeout. Frontends answer
	// 504.
	ErrGatewayTimeout = errors.New("format: gateway timeout")
)

// UnavailableError is an ErrUnavailable that says when to ask again: the upstream's own
// Retry-After, at most a minute, 5 seconds when it sent none.
type UnavailableError struct {
	// RetryAfter is the wait.
	RetryAfter time.Duration
}

func (e *UnavailableError) Error() string {
	return "format: upstream unavailable: it asked to wait " + e.RetryAfter.Round(time.Second).String()
}

// Unwrap returns ErrUnavailable, so that errors.Is matches the sentinel.
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }
