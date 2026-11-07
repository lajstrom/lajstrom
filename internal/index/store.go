package index

import (
	"context"
	"time"

	"github.com/opencontainers/go-digest"
)

// Store is the index database behind one interface: the control plane and the derived tables
// of docs/spec/index-schema.md. internal/index/sqlite and internal/index/postgres implement it.
// Both engines pass the conformance suite in internal/index/storetest.
//
// Each method of Store is a transaction of its own; Tx groups several writes into one.
type Store interface {
	Reader
	Writer

	// Migrate applies the embedded migrations that are not applied yet. It is idempotent.
	Migrate(ctx context.Context) error

	// Tx runs fn in one transaction. It commits when fn returns nil and rolls back otherwise,
	// returning fn's error. The Writer must not be used after fn returns.
	Tx(ctx context.Context, fn func(Writer) error) error

	// Close releases the database connection.
	Close() error
}

// Reader is the read side of the index; protocol frontends use it.
//
// Get* methods return an error wrapping ErrNotFound when no row matches. List* and Find*
// methods return an empty result, not an error, when nothing matches.
type Reader interface {
	// Control plane.

	// GetRepo returns the repo with the given format and name.
	GetRepo(ctx context.Context, format, name string) (*Repo, error)
	// GetRepoByID returns the repo with the given id.
	GetRepoByID(ctx context.Context, id int64) (*Repo, error)
	// ListRepos returns the repos of format, or of every format when format is "", ordered by
	// format and name.
	ListRepos(ctx context.Context, format string) ([]Repo, error)

	// GetUser returns the user with the given name.
	GetUser(ctx context.Context, name string) (*User, error)
	// ListUsers returns all users ordered by name.
	ListUsers(ctx context.Context) ([]User, error)

	// GetTokenByHash returns the token whose secret hashes to hash (hex sha256), revoked and
	// expired tokens included; the caller checks RevokedAt and ExpiresAt.
	GetTokenByHash(ctx context.Context, hash string) (*Token, error)
	// ListTokens returns the tokens owned by userID and bound to repoID; a nil filter matches
	// every token.
	ListTokens(ctx context.Context, userID, repoID *int64) ([]Token, error)

	// ListBindings returns the bindings of principal (canonical form, docs/spec/rbac.md §1),
	// or every binding when principal is "".
	ListBindings(ctx context.Context, principal string) ([]Binding, error)

	// GetKey returns the key of kind that is not retired; repoID nil selects the server key.
	GetKey(ctx context.Context, repoID *int64, kind string) (*Key, error)
	// ListKeys returns the keys of a repo, retired ones included; repoID nil selects the
	// server keys.
	ListKeys(ctx context.Context, repoID *int64) ([]Key, error)

	// GetSession returns the open publication session with key in a repo.
	GetSession(ctx context.Context, repoID int64, key string) (*Session, error)
	// GetSessionByID returns the publication session with the given id, in any state.
	GetSessionByID(ctx context.Context, id int64) (*Session, error)

	// ListAudit returns the audit events matching f, newest first.
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)

	// Coordination.

	// GetTagClaim returns the claim on tag of package pkg (format spelling) in a repo.
	GetTagClaim(ctx context.Context, repoID int64, pkg, tag string) (*TagClaim, error)

	// Derived.

	// GetPackage returns the package with the given name (format spelling) in a repo.
	GetPackage(ctx context.Context, repoID int64, name string) (*Package, error)
	// ListPackages returns the packages of a repo whose name starts with prefix ("" = all),
	// ordered by name.
	ListPackages(ctx context.Context, repoID int64, prefix string) ([]Package, error)

	// GetVersion returns the version of a package by version and variant ("" when none).
	GetVersion(ctx context.Context, packageID int64, version, variant string) (*Version, error)
	// GetVersionByTag returns the version of a package that has tag.
	GetVersionByTag(ctx context.Context, packageID int64, tag string) (*Version, error)
	// GetVersionByDigest returns the version in a repo whose manifest digest is d.
	GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*Version, error)
	// ListVersions returns the versions of a package ordered by CreatedAt, then ID.
	ListVersions(ctx context.Context, packageID int64) ([]Version, error)

	// ListFiles returns the files of a version in manifest order.
	ListFiles(ctx context.Context, versionID int64) ([]File, error)
	// FindFiles returns the files called name in a repo with their versions and packages;
	// apt pool downloads and maven lookups go by file name.
	FindFiles(ctx context.Context, repoID int64, name string) ([]FileMatch, error)

	// GetIndexCache returns the cached document of kind for a scope of a repo.
	GetIndexCache(ctx context.Context, repoID int64, scope, kind string) (*Cache, error)

	// ListOCIRepos returns the OCI repositories recorded for a repo, ordered by path.
	ListOCIRepos(ctx context.Context, repoID int64) ([]OCIRepo, error)
}

// Writer is the write side of the index; ingest, reindex, delete and the management API use
// it. Writes to derived tables happen only in ingest, reindex and delete
// (docs/spec/index-schema.md, Invariants and rules).
//
// Create* and Upsert* methods take a pointer to the model and fill ID, and CreatedAt (and
// UpdatedAt where the table has it) when they are zero. Unique violations return an error
// wrapping ErrConflict; updates and deletes of a missing id return one wrapping ErrNotFound.
type Writer interface {
	Reader

	// Control plane.

	// CreateRepo inserts r; (format, name) must be new.
	CreateRepo(ctx context.Context, r *Repo) error
	// UpdateRepo stores r by r.ID and sets UpdatedAt. Format and Name cannot change.
	UpdateRepo(ctx context.Context, r *Repo) error
	// DeleteRepo removes the repo and every row that refers to it.
	DeleteRepo(ctx context.Context, id int64) error

	// CreateUser inserts u; the name must be new.
	CreateUser(ctx context.Context, u *User) error
	// UpdateUser stores u by u.ID. Name cannot change.
	UpdateUser(ctx context.Context, u *User) error

	// CreateToken inserts t; the hash must be new.
	CreateToken(ctx context.Context, t *Token) error
	// RevokeToken sets RevokedAt of the token.
	RevokeToken(ctx context.Context, id int64, at time.Time) error
	// TouchToken sets LastUsedAt of the token.
	TouchToken(ctx context.Context, id int64, lastUsed time.Time) error

	// CreateBinding inserts b.
	CreateBinding(ctx context.Context, b *Binding) error
	// DeleteBinding removes the binding.
	DeleteBinding(ctx context.Context, id int64) error

	// CreateKey inserts k.
	CreateKey(ctx context.Context, k *Key) error
	// RetireKey sets RetiredAt of the key.
	RetireKey(ctx context.Context, id int64, at time.Time) error

	// UpsertSession inserts s when s.ID is 0 and otherwise stores State, Files and ExpiresAt
	// of row s.ID; it sets UpdatedAt.
	UpsertSession(ctx context.Context, s *Session) error
	// DeleteSession removes the session.
	DeleteSession(ctx context.Context, id int64) error
	// ExpireSessions sets State to expired on open sessions whose ExpiresAt is before before,
	// and returns how many it changed.
	ExpireSessions(ctx context.Context, before time.Time) (int, error)

	// AppendAudit inserts ev; At is set when zero. Audit rows are never changed.
	AppendAudit(ctx context.Context, ev *AuditEvent) error

	// Coordination: tag claims, the lock on a tag that ingest takes before the registry write
	// (docs/spec/index-schema.md, Coordination).

	// ClaimTag inserts c, sets ClaimedAt to now when it is zero, and fills ID. When a claim
	// for (RepoID, Package, Tag) exists, the claim rule decides: with c's digest, c is filled
	// from it and the claim is shared (two replicas publishing the same bytes); with another
	// digest and ClaimedAt before staleBefore, the row is taken over (digest, principal and
	// ClaimedAt are c's, ID is the row's); with another digest and a fresh claim, nothing
	// changes and the error is a *TagConflictError. A race between writers ends the same
	// way: the loser gets the TagConflictError, never a driver error.
	ClaimTag(ctx context.Context, c *TagClaim, staleBefore time.Time) error
	// ReleaseTagClaim removes the claim.
	ReleaseTagClaim(ctx context.Context, id int64) error
	// ExpireTagClaims removes the claims whose ClaimedAt is before before, and returns how
	// many it removed (reindex and the reconcile loop).
	ExpireTagClaims(ctx context.Context, before time.Time) (int, error)

	// Derived.

	// UpsertPackage inserts p or, when (RepoID, Name) exists, fills p from the existing row.
	UpsertPackage(ctx context.Context, p *Package) error
	// UpsertVersion inserts v or, when (PackageID, Version, Variant) exists with the same
	// digest, refreshes it in place (idempotent re-ingest). When (PackageID, Tag) exists with
	// a different digest it changes nothing and returns a *TagConflictError: tags are
	// append-only and never re-pointed.
	UpsertVersion(ctx context.Context, v *Version) error
	// ReplaceFiles replaces the files of a version with files and fills their IDs.
	ReplaceFiles(ctx context.Context, versionID int64, files []File) error
	// DeleteVersion removes the version and its files.
	DeleteVersion(ctx context.Context, id int64) error
	// DeletePackage removes the package, its versions and their files.
	DeletePackage(ctx context.Context, id int64) error

	// PutIndexCache inserts c or replaces the row with the same (RepoID, Scope, Kind).
	PutIndexCache(ctx context.Context, c *Cache) error
	// InvalidateIndexCache removes the cached documents of a scope of a repo, or of every
	// scope when scope is "".
	InvalidateIndexCache(ctx context.Context, repoID int64, scope string) error

	// UpsertOCIRepo inserts r or sets SeenAt of the row with the same (RepoID, Path).
	UpsertOCIRepo(ctx context.Context, r *OCIRepo) error
	// PruneOCIRepos removes the OCI repositories of a repo not seen since seenBefore and
	// returns how many it removed.
	PruneOCIRepos(ctx context.Context, repoID int64, seenBefore time.Time) (int, error)

	// ResetDerived removes every derived row of a repo, or of all repos when repoID is 0,
	// before a reindex. It never touches control-plane tables or tag claims.
	ResetDerived(ctx context.Context, repoID int64) error
}
