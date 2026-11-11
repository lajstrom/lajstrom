package provider

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Sentinel errors. Implementations translate backend and oras-go errors (errdef, errcode) into
// these and wrap them with %w, so callers never inspect registry-specific errors.
var (
	// ErrNotFound means the repository, manifest, blob or tag does not exist in the backend.
	ErrNotFound = errors.New("provider: not found")
	// ErrConflict means the backend refused a write because of existing content, for example
	// an immutable tag.
	ErrConflict = errors.New("provider: conflict")
	// ErrUnsupported means the backend does not support the operation (see Capabilities).
	ErrUnsupported = errors.New("provider: unsupported")
	// ErrUnauthorized means the backend rejected Lajstrom's own credentials.
	ErrUnauthorized = errors.New("provider: unauthorized")
	// ErrUnavailable means the backend takes no request now: the provider did not send it,
	// because the backend failed repeatedly and its circuit breaker is open (an
	// *UnavailableError), or the backend answered 429 or 503 (a *RejectedError). Both say when
	// to try again (RetryAfter). It also means that the provider could not reach the backend
	// because its host name does not resolve (#561), which says no time.
	ErrUnavailable = errors.New("provider: unavailable")
	// ErrRejected means the backend refused a request with a status that no other sentinel
	// names: a 4xx such as 400 BLOB_UPLOAD_INVALID, MANIFEST_INVALID or SIZE_INVALID, 413 or
	// 416, and the 429 or 503 of a busy backend, which is ErrUnavailable too. A *RejectedError
	// says how.
	ErrRejected = errors.New("provider: rejected")
)

// RejectedError is an ErrRejected with what the backend answered. Its message names the
// registry by registries[].name and holds nothing else of the backend's: no URL, no instance
// prefix, no message of the registry's own; the facade answers the client with it.
type RejectedError struct {
	// Registry is registries[].name.
	Registry string
	// Status is the backend's status: a 4xx, or 503.
	Status int
	// Code is the first error code of the answer, "" when it had none.
	Code string
	// RetryAfter is the Retry-After of a 429 or 503, zero when it had none.
	RetryAfter time.Duration
	// Hint is what the operator may change so that the request passes, when the provider knows
	// it; "" otherwise.
	Hint string
}

func (e *RejectedError) Error() string {
	msg := "registry " + e.Registry + " refused the request: " + strconv.Itoa(e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Hint != "" {
		msg += "; hint: " + e.Hint
	}
	return msg
}

// Unwrap returns ErrRejected, and ErrUnavailable for a busy backend (Busy), so that errors.Is
// matches the sentinels.
func (e *RejectedError) Unwrap() []error {
	if e.Busy() {
		return []error{ErrRejected, ErrUnavailable}
	}
	return []error{ErrRejected}
}

// Busy reports whether the backend refused the request because it takes none now: 429, 503.
// It throttles or sheds Lajstrom, whose credential every client shares, not one client.
func (e *RejectedError) Busy() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable
}

// UnavailableError is an ErrUnavailable that says how long until the provider sends a request
// to the backend again.
type UnavailableError struct {
	// RetryAfter is the time until then, zero when a request may go now.
	RetryAfter time.Duration
}

func (e *UnavailableError) Error() string {
	return "provider: unavailable: the registry failed repeatedly, requests fail at once for " + e.RetryAfter.Round(time.Second).String()
}

// Unwrap returns ErrUnavailable, so that errors.Is matches the sentinel.
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

// RetryAfter returns the RetryAfter of the *UnavailableError that err wraps, or of its
// *RejectedError of a busy backend, and whether it wraps one: every ErrUnavailable says when,
// but a host name that does not resolve.
func RetryAfter(err error) (time.Duration, bool) {
	var u *UnavailableError
	if errors.As(err, &u) {
		return u.RetryAfter, true
	}
	var r *RejectedError
	if errors.As(err, &r) && r.Busy() {
		return r.RetryAfter, true
	}
	return 0, false
}
