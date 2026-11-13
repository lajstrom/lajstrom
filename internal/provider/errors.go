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
	// ErrLimit means the backend refused a write for a limit or a quota of its own that the
	// write would pass: a count (ECR's LimitExceededException: repositories a region, images a
	// repository), a storage quota (Harbor's project quota), a size limit (Gitea's and Forgejo's
	// LIMIT_SIZE_CONTAINER). It is no fault of Lajstrom's credentials, and the same write fails
	// again until the operator raises the limit or frees room: it is not retried. The message
	// of an error that wraps it names the limit, and holds nothing of the registry's own: no
	// URL, instance prefix, account or message. A 413 stays a *RejectedError (the file is too
	// large for the backend), and a 429 ErrUnavailable (a rate, which passes with time).
	ErrLimit = errors.New("provider: refused for a limit or a quota of the registry")
	// ErrUnavailable means the backend takes no request now, or does not serve yet a write it
	// acknowledged: the provider did not send the request, because the backend failed
	// repeatedly and its circuit breaker is open (an *UnavailableError); the backend answered
	// 429 or 503 (a *RejectedError); or a push is not served after the read-back (an
	// *UnavailableError with a Reason). Each says when to try again (RetryAfter). It also means
	// that the provider could not reach the backend: its host name does not resolve (#561), it
	// refused or reset the connection (#593), or its host or network cannot be reached
	// (EHOSTUNREACH, ENETUNREACH); or that the backend did not answer: after the retries, a
	// request timed out or its connection closed without an answer, and the provider's circuit
	// breaker counted that as the backend's failure (#670). These say no time. A deadline or
	// cancel of the caller's context, and a client's body that stalls or breaks off as it
	// streams to the backend, are not the backend's failure: they are not ErrUnavailable and
	// keep their own error.
	ErrUnavailable = errors.New("provider: unavailable")
	// ErrRejected means the backend refused a request with a status that no other sentinel
	// names: a 4xx such as 400 BLOB_UPLOAD_INVALID, MANIFEST_INVALID or SIZE_INVALID, 413 or
	// 416, and the 429 or 503 of a busy backend, which is ErrUnavailable too. A *RejectedError
	// says how.
	ErrRejected = errors.New("provider: rejected")
	// ErrManifestUnknown means the registry answered 404 with the error code MANIFEST_UNKNOWN
	// to a request for a manifest by its digest (Provider.StatManifest): the one answer that
	// counts as "unknown" for prune (docs/spec/guarantees.md, INV-5). An error that wraps it
	// wraps ErrNotFound too; a 404 without the code, NAME_UNKNOWN among them, wraps
	// ErrNotFound alone.
	ErrManifestUnknown = errors.New("provider: manifest unknown")
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
// to the backend again, or until the backend serves what it did not yet: a push it acknowledged
// and does not serve after the read-back (ReadBack), on an eventually consistent backend.
type UnavailableError struct {
	// RetryAfter is the time until then, zero when a request may go now.
	RetryAfter time.Duration
	// Reason says why, for the log; empty is an open circuit breaker.
	Reason string
}

func (e *UnavailableError) Error() string {
	if e.Reason != "" {
		return "provider: unavailable: " + e.Reason
	}
	return "provider: unavailable: the registry failed repeatedly, requests fail at once for " + e.RetryAfter.Round(time.Second).String()
}

// Unwrap returns ErrUnavailable, so that errors.Is matches the sentinel.
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

// RetryAfter returns the RetryAfter of the *UnavailableError that err wraps, or of its
// *RejectedError of a busy backend, and whether it wraps one: every ErrUnavailable says when,
// but a backend that could not be reached (a host name that does not resolve, a connection
// refused or reset, a host or network that cannot be reached) or did not answer (a timeout, a
// connection closed without an answer).
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
