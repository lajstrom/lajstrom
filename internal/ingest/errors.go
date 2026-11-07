package ingest

import (
	"errors"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Errors returned by the pipeline, always wrapped with context. ErrInvalid and ErrConflict are
// the sentinels of the packages that own the rule, re-exported so that callers can test
// against either name; ingest adds no sentinel of its own for them.
var (
	// ErrInvalid is format.ErrInvalid: the manifest or the upload does not satisfy the
	// manifest profile or the plugin's checks (400, facade MANIFEST_INVALID).
	ErrInvalid = format.ErrInvalid

	// ErrConflict is index.ErrConflict: the tag exists with another digest (409, facade
	// TAG_INVALID). The error chain contains an *index.TagConflictError with the existing
	// digest, also when the collision is found in the backend rather than in the index.
	ErrConflict = index.ErrConflict

	// ErrNotImplemented was what the pipeline returned before issue #12 filled it in. Nothing
	// returns it now; it stays for the parts that come later (publication sessions, #53).
	ErrNotImplemented = errors.New("ingest: not implemented, see issue #12")
)
