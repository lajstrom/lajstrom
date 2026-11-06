package provider

import "errors"

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
)
