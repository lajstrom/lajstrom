package index

import (
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
)

var (
	// ErrNotFound means no row matches. Get* methods return it wrapped.
	ErrNotFound = errors.New("index: not found")
	// ErrConflict means a unique constraint was violated: a duplicate name, or a tag that
	// already points at another digest (see TagConflictError).
	ErrConflict = errors.New("index: conflict")
)

// TagConflictError is the error UpsertVersion returns, possibly wrapped, when the version's tag
// already exists in the package with another manifest digest: the append-only tag rule of
// docs/spec/manifest-profile.md §4.2. ClaimTag returns it for a fresh claim of the tag with
// another digest. errors.Is(err, ErrConflict) reports true for it.
type TagConflictError struct {
	// Tag is the tag that already exists.
	Tag string
	// Existing is the digest the tag points at.
	Existing digest.Digest
}

// Error implements error.
func (e *TagConflictError) Error() string {
	return fmt.Sprintf("index: tag %q already points at %s", e.Tag, e.Existing)
}

// Unwrap returns ErrConflict.
func (e *TagConflictError) Unwrap() error { return ErrConflict }
