package index

import (
	"errors"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
)

var (
	// ErrNotFound means no row matches. Get* methods return it wrapped.
	ErrNotFound = errors.New("index: not found")
	// ErrConflict means a unique constraint was violated: a duplicate name, or a tag that
	// already points at another digest (see TagConflictError).
	ErrConflict = errors.New("index: conflict")
	// ErrSchemaTooNew means the database has migrations this binary does not know, and one of
	// them requires a version above the newest this binary knows (ApplyMigrations).
	ErrSchemaTooNew = errors.New("index: the schema is newer than this binary")
	// ErrBreakingMigration means a pending migration requires a version above the schema's,
	// so binaries serving on the schema would stop working, and it was not applied
	// (ApplyMigrations, MigrateOptions.Breaking).
	ErrBreakingMigration = errors.New("index: a pending migration would break the replicas serving on the schema")
	// ErrUnavailable means the database did not answer, and may answer a later attempt: the
	// store cannot connect to it, or it ended the connection or refuses new ones while it
	// shuts down or starts (Postgres), or it stayed locked past the busy timeout or its file
	// cannot be opened (SQLite). Every method may return it wrapped.
	ErrUnavailable = errors.New("index: unavailable")
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

// ReindexConflictError is the error StartReindexRun returns when a running run overlaps the new
// one and its heartbeat is fresh: a run of every repo overlaps every run, a run of one repo a
// run of every repo and a run of the same repo. errors.Is(err, ErrConflict) reports true for it.
type ReindexConflictError struct {
	// Running is the run that holds the lock.
	Running ReindexRun
}

// Error implements error.
func (e *ReindexConflictError) Error() string {
	scope := "every repo"
	if e.Running.RepoID != nil {
		scope = fmt.Sprintf("repo %d", *e.Running.RepoID)
	}
	return fmt.Sprintf("index: a reindex of %s is running on %s since %s (run %d)", scope, e.Running.Replica,
		e.Running.StartedAt.Format(time.RFC3339), e.Running.ID)
}

// Unwrap returns ErrConflict.
func (e *ReindexConflictError) Unwrap() error { return ErrConflict }

// ReplicaConflictError is the error StartReplica returns on SQLite when the row of another
// replica has a fresh heartbeat: SQLite serves one replica, whose locks are its process's.
// errors.Is(err, ErrConflict) reports true for it.
type ReplicaConflictError struct {
	// Live is the row of the replica that serves on the database.
	Live Replica
}

// Error implements error.
func (e *ReplicaConflictError) Error() string {
	return fmt.Sprintf("index: replica %s (host %s, process %d, %s) serves on this database since %s, last heartbeat %s",
		e.Live.ID, e.Live.Host, e.Live.PID, e.Live.Version, e.Live.StartedAt.Format(time.RFC3339),
		e.Live.HeartbeatAt.Format(time.RFC3339))
}

// Unwrap returns ErrConflict.
func (e *ReplicaConflictError) Unwrap() error { return ErrConflict }
