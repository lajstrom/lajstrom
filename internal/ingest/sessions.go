package ingest

import (
	"context"
	"io"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Sessions accumulate the files of one version for formats whose upload protocol takes several
// requests (maven: each file is a PUT, maven-metadata.xml commits). Blobs go to the registry as
// they arrive; the session row in the index records their descriptors, and Commit builds and
// stores the manifest through the pipeline. See docs/architecture.md §3 (flow B).
//
// The implementation comes with issue #53.
type Sessions interface {
	// Open returns the open session for key in repo, creating it when there is none.
	Open(ctx context.Context, repo index.Repo, key string, principal auth.Principal) (*index.Session, error)

	// Add pushes the bytes of r as a blob and records its descriptor, under name, in the
	// session; the blob is in the registry when Add returns. size is -1 when unknown.
	Add(ctx context.Context, sessionID int64, name string, r io.Reader, size int64) (ocispec.Descriptor, error)

	// Commit builds the manifest from the session's files and stores it as Pipeline.Ingest
	// does, then marks the session committed.
	Commit(ctx context.Context, sessionID int64) (*Result, error)

	// Abort marks the session aborted. Pushed blobs stay for the registry's garbage collection.
	Abort(ctx context.Context, sessionID int64) error

	// Expire marks open sessions whose deadline is before now as expired and returns how
	// many it changed.
	Expire(ctx context.Context, now time.Time) (int, error)
}
