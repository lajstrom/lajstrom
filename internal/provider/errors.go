package provider

import (
	"errors"
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
	// ErrUnavailable means the provider did not send the request: the backend failed
	// repeatedly and its circuit breaker is open. An *UnavailableError says when it takes a
	// request again (RetryAfter).
	ErrUnavailable = errors.New("provider: unavailable")
)

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

// RetryAfter returns the RetryAfter of the *UnavailableError that err wraps, and whether it
// wraps one.
func RetryAfter(err error) (time.Duration, bool) {
	var u *UnavailableError
	if errors.As(err, &u) {
		return u.RetryAfter, true
	}
	return 0, false
}
