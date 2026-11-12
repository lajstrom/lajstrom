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

	// WithReferrersLock calls fn holding the lock on the referrers of subject in the OCI
	// repository name (without the instance prefix), and returns fn's error: the writers of
	// the sha256-<hex> tag-schema index of one subject never overlap, on any replica
	// (docs/spec/index-schema.md, Coordination). On Postgres the lock is pg_advisory_xact_lock
	// on a key hashed from name and subject, in a transaction held until fn returns, even when
	// ctx ends meanwhile: fn holds one connection of the pool and should not wait on the store.
	// The transaction lifts idle_in_transaction_session_timeout for itself; a timeout it cannot
	// lift (a pooler's: PgBouncer's idle_transaction_timeout must be 0), a terminated backend or
	// a failover still ends it, and the lock with it. So when fn returns nil, the transaction
	// is committed, and when that fails the error wraps ErrUnavailable and says that the lock
	// was lost: fn ran to its end, and another writer may have held the lock meanwhile. It is
	// never nil then. On SQLite, which serves one replica, it is a lock of the process. When
	// ctx ends while the lock is awaited, fn is not called and the error wraps ctx's.
	WithReferrersLock(ctx context.Context, name string, subject digest.Digest, fn func(context.Context) error) error

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
	// ListGroupMembers returns the members of the group groupID in member order
	// (docs/spec/index-schema.md, group_members); none for a repo that is not a group.
	ListGroupMembers(ctx context.Context, groupID int64) ([]Repo, error)
	// ListGroupsOf returns the groups that have memberID as a member, ordered by format and name.
	ListGroupsOf(ctx context.Context, memberID int64) ([]Repo, error)

	// GetUser returns the live user with the given name.
	GetUser(ctx context.Context, name string) (*User, error)
	// GetUserByUID returns the user with the given uid, live or deleted.
	GetUserByUID(ctx context.Context, uid string) (*User, error)
	// GetUserByID returns the user with the given id, live or deleted: the owner of a token.
	GetUserByID(ctx context.Context, id int64) (*User, error)
	// ListUsers returns the live users ordered by name.
	ListUsers(ctx context.Context) ([]User, error)

	// GetTokenByHash returns the token whose secret hashes to hash (hex sha256), revoked and
	// expired tokens included; the caller checks RevokedAt and ExpiresAt.
	GetTokenByHash(ctx context.Context, hash string) (*Token, error)
	// ListTokens returns the tokens owned by userID and bound to repoID; a nil filter matches
	// every token.
	ListTokens(ctx context.Context, userID, repoID *int64) ([]Token, error)

	// GetBootstrapToken returns the bootstrap token, used and expired ones included; the caller
	// checks UsedAt and ExpiresAt.
	GetBootstrapToken(ctx context.Context) (*BootstrapToken, error)

	// ListBindings returns the bindings of principal (canonical form, docs/spec/rbac.md §1),
	// or every binding when principal is "".
	ListBindings(ctx context.Context, principal string) ([]Binding, error)
	// AuthVersion returns the counter of auth_version, which every write of bindings and of
	// repos increments in its transaction (docs/spec/rbac.md §3). An evaluator reads it before
	// the bindings and the repos of its snapshot, so that a snapshot is never newer than the
	// version it is kept under: a write between the reads makes the next request reload once
	// more.
	AuthVersion(ctx context.Context) (int64, error)

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

	// GetTagClaim returns the claim on tag of the package path pkg (NameOCI) in a repo.
	GetTagClaim(ctx context.Context, repoID int64, pkg, tag string) (*TagClaim, error)
	// GetFetchClaim returns the claim of a proxy fetch on tag of the package path pkg in a repo.
	GetFetchClaim(ctx context.Context, repoID int64, pkg, tag string) (*FetchClaim, error)
	// GetReindexRun returns the reindex run with the given id.
	GetReindexRun(ctx context.Context, id int64) (*ReindexRun, error)
	// LatestReindexRun returns the newest run that covered repoID, its own or a run of every
	// repo; repoID 0 selects the runs of every repo only.
	LatestReindexRun(ctx context.Context, repoID int64) (*ReindexRun, error)
	// ListReplicas returns the rows of replicas ordered by StartedAt, then ID; a row whose
	// HeartbeatAt is older than 2 minutes is of a replica that is gone.
	ListReplicas(ctx context.Context) ([]Replica, error)
	// DatabaseTime returns the time of the database's clock: the server's on Postgres
	// (clock_timestamp()), the host's on SQLite, which runs in the process. A replica compares
	// its own clock with it (docs/operations.md, Clocks).
	DatabaseTime(ctx context.Context) (time.Time, error)

	// Derived.

	// GetPackage returns the package with the given name (format spelling) in a repo.
	GetPackage(ctx context.Context, repoID int64, name string) (*Package, error)
	// GetPackageByNameOCI returns the package of a repo whose escaped package path (NameOCI)
	// is nameOCI; a repo has at most one.
	GetPackageByNameOCI(ctx context.Context, repoID int64, nameOCI string) (*Package, error)
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
	// ListUnseenVersions returns the versions of a repo whose IndexedAt is before seenBefore,
	// with their packages, ordered by package name and version id: the sweep of reindex.
	ListUnseenVersions(ctx context.Context, repoID int64, seenBefore time.Time) ([]VersionMatch, error)

	// ListFiles returns the files of a version in manifest order.
	ListFiles(ctx context.Context, versionID int64) ([]File, error)
	// FindFiles returns the files called name in a repo with their versions and packages;
	// apt pool downloads and maven lookups go by file name.
	FindFiles(ctx context.Context, repoID int64, name string) ([]FileMatch, error)

	// GetIndexCache returns the cached document of kind for a scope of a repo.
	GetIndexCache(ctx context.Context, repoID int64, scope, kind string) (*Cache, error)
	// IndexCacheGeneration returns the generation of a repo's rendered documents: 0 at first,
	// incremented by every InvalidateIndexCache and ResetDerived of the repo in its transaction.
	// A render reads it before anything it renders from, and passes it to PutIndexCache.
	IndexCacheGeneration(ctx context.Context, repoID int64) (int64, error)

	// ListOCIRepos returns the OCI repositories recorded for a repo, ordered by path.
	ListOCIRepos(ctx context.Context, repoID int64) ([]OCIRepo, error)

	// A cache of the upstream (ADR 0013).

	// GetProxyMeta returns the proxy_meta row of a proxy repo's name key and document kind.
	GetProxyMeta(ctx context.Context, repoID int64, name, kind string) (*ProxyMeta, error)

	// The npm journal (ADR 0012; docs/spec/index-schema.md, npm).

	// ListJournal returns the journal entries of a package ordered by Seq.
	ListJournal(ctx context.Context, packageID int64) ([]JournalEntry, error)
	// ListJournalState returns the current dist-tags and deprecations of a package, the fold of
	// its journal, ordered by Kind and Key.
	ListJournalState(ctx context.Context, packageID int64) ([]JournalState, error)
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

	// CreateRepo inserts r; (format, name) must be new. It increments AuthVersion, as
	// UpdateRepo, DeleteRepo, CreateBinding and DeleteBinding do, in the same transaction.
	CreateRepo(ctx context.Context, r *Repo) error
	// UpdateRepo stores r by r.ID and sets UpdatedAt. Format, Name and Kind cannot change: they
	// are filled from the row.
	UpdateRepo(ctx context.Context, r *Repo) error
	// DeleteRepo removes the repo and every row that refers to it, proxy_meta and a group's
	// group_members rows included. A repo that is a member of a group is ErrConflict, and
	// nothing is deleted.
	DeleteRepo(ctx context.Context, id int64) error
	// SetGroupMembers makes the group_members rows of the group groupID exactly memberIDs, in
	// that order. An id that is no repo is ErrNotFound, an id named twice ErrConflict. The caller
	// writes the group's repos row in the same transaction, so that AuthVersion moves with it.
	SetGroupMembers(ctx context.Context, groupID int64, memberIDs []int64) error

	// CreateUser inserts u; the name must be new. UID is made (NewUID) when it is empty.
	CreateUser(ctx context.Context, u *User) error
	// UpdateUser stores Name, PasswordHash, Kind, Disabled, DeletedAt and DeletedName of u by
	// u.ID and fills UID and CreatedAt from the row. A name another user has is ErrConflict.
	UpdateUser(ctx context.Context, u *User) error

	// CreateToken inserts t; the hash must be new.
	CreateToken(ctx context.Context, t *Token) error
	// RevokeToken sets RevokedAt of the token.
	RevokeToken(ctx context.Context, id int64, at time.Time) error
	// TouchToken sets LastUsedAt of the token.
	TouchToken(ctx context.Context, id int64, lastUsed time.Time) error

	// PutBootstrapToken makes t the bootstrap token when the stored one is still the one whose
	// hash is old: with old "" it inserts t when there is no row, otherwise it replaces the row
	// whose hash is old, UsedAt cleared. When the row is another (a replica made or replaced it
	// in between), nothing changes and the error wraps ErrConflict: of replicas that start
	// together, one token is made. CreatedAt is now when zero.
	PutBootstrapToken(ctx context.Context, t *BootstrapToken, old string) error
	// UseBootstrapToken sets UsedAt of the bootstrap token to at when its hash is hash, it is
	// not used and its ExpiresAt is after at; otherwise ErrNotFound. In a transaction it holds
	// the row until the transaction ends, so that of two exchanges of the token one commits and
	// the other finds it used.
	UseBootstrapToken(ctx context.Context, hash string, at time.Time) error

	// CreateBinding inserts b.
	CreateBinding(ctx context.Context, b *Binding) error
	// DeleteBinding removes the binding.
	DeleteBinding(ctx context.Context, id int64) error

	// CreateKey inserts k.
	CreateKey(ctx context.Context, k *Key) error
	// RetireKey sets RetiredAt of the key.
	RetireKey(ctx context.Context, id int64, at time.Time) error

	// UpsertSession inserts s when s.ID is 0 and otherwise stores State, Files and ExpiresAt
	// of row s.ID; it sets UpdatedAt. A key has one open session in a repo: inserting a second
	// is ErrConflict.
	UpsertSession(ctx context.Context, s *Session) error
	// TouchSession sets ExpiresAt of session id to expiresAt and returns the row, when the
	// session is open and its ExpiresAt is after at; otherwise ErrNotFound. In a transaction it
	// holds the row until the transaction ends, so that a read and a write of its Files that
	// follow are not interleaved with another transaction's.
	TouchSession(ctx context.Context, id int64, at, expiresAt time.Time) (*Session, error)
	// DeleteSession removes the session.
	DeleteSession(ctx context.Context, id int64) error
	// ExpireSessions sets State to expired on open sessions whose ExpiresAt is before before,
	// and returns how many it changed.
	ExpireSessions(ctx context.Context, before time.Time) (int, error)

	// AppendAudit inserts ev; At is set when zero, State to AuditOK when empty, and
	// PrincipalName, when empty and Principal is user:<uid>, to that user's name at the time (the
	// name it had when deleted, for a deleted user). Audit rows are never deleted, and only
	// CompleteAudit and ExpirePendingAudit change one: its state.
	AppendAudit(ctx context.Context, ev *AuditEvent) error
	// CompleteAudit stores State, Error and Details of ev in the row ev.ID, the pending row of a
	// registry write (AuditOK or AuditFailed), and fills ev from the row. A row keeps the first
	// 1024 bytes of Error, here and in AppendAudit. A row that is not
	// pending or unknown, or none, is ErrNotFound: a result is recorded once, and a writer that
	// went on after ExpirePendingAudit gave up on it still records its own.
	CompleteAudit(ctx context.Context, ev *AuditEvent) error
	// ExpirePendingAudit sets the state of the pending rows whose At is before before to
	// AuditUnknown, and returns how many it changed (reindex and the reconcile loop, with
	// ingest.claimTTL).
	ExpirePendingAudit(ctx context.Context, before time.Time) (int, error)

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

	// Coordination: fetch claims, the lock of a proxy fetch on one artifact across replicas
	// (docs/spec/index-schema.md, Proxy and group repos). ResetDerived does not touch them.

	// ClaimFetch inserts c, sets ClaimedAt to now when it is zero and HeartbeatAt to ClaimedAt
	// when it is zero, and fills ID. When a claim of (RepoID, Package, Tag) exists, its
	// HeartbeatAt decides: at or after staleBefore, the error wraps ErrConflict and nothing
	// changes; before it, the row is deleted and c inserted with a new id, so that the
	// heartbeats and the release of the fetch that held it find nothing. The loser of a race
	// gets the ErrConflict, never a driver error.
	ClaimFetch(ctx context.Context, c *FetchClaim, staleBefore time.Time) error
	// HeartbeatFetchClaim sets HeartbeatAt of the claim; ErrNotFound when it is gone, released
	// or taken over, and the fetch should not go on believing it holds it.
	HeartbeatFetchClaim(ctx context.Context, id int64, at time.Time) error
	// ReleaseFetchClaim removes the claim.
	ReleaseFetchClaim(ctx context.Context, id int64) error
	// ExpireFetchClaims removes the claims whose HeartbeatAt is before before, and returns how
	// many it removed (reindex and the reconcile loop).
	ExpireFetchClaims(ctx context.Context, before time.Time) (int, error)

	// Coordination: replicas, a row per serve process on the database
	// (docs/spec/index-schema.md, Coordination).

	// StartReplica deletes the rows whose HeartbeatAt is before staleBefore and inserts r, in one
	// transaction; StartedAt is now when zero, HeartbeatAt StartedAt when zero. On SQLite, which
	// serves one replica, a row of another replica whose HeartbeatAt is at or after staleBefore
	// is a *ReplicaConflictError naming it, and nothing changes; on Postgres the rows coexist. An
	// ID that has a row is ErrConflict.
	StartReplica(ctx context.Context, r *Replica, staleBefore time.Time) error
	// HeartbeatReplica sets HeartbeatAt of the replica's row; ErrNotFound when the row is gone: a
	// start that found it stale deleted it.
	HeartbeatReplica(ctx context.Context, id string, at time.Time) error
	// StopReplica deletes the replica's row.
	StopReplica(ctx context.Context, id string) error

	// Coordination: reindex runs (docs/spec/index-schema.md, Coordination).

	// StartReindexRun inserts r as running under the reindex lock and fills ID and State;
	// StartedAt is now when zero, HeartbeatAt StartedAt when zero. A running run whose scope overlaps r's and
	// whose HeartbeatAt is at or after staleBefore is a *ReindexConflictError and nothing
	// changes; an older one is closed as abandoned first.
	StartReindexRun(ctx context.Context, r *ReindexRun, staleBefore time.Time) error
	// HeartbeatReindexRun sets HeartbeatAt of a running run; ErrNotFound when the run is not
	// running any more (a later start closed it as abandoned), and the run should stop.
	HeartbeatReindexRun(ctx context.Context, id int64, at time.Time) error
	// FinishReindexRun stores State (done, failed or abandoned), FinishedAt (now when nil),
	// Report and Error of the running run r.ID, and deletes the finished runs of its scope
	// beyond the ten newest. ErrNotFound when the run is not running.
	FinishReindexRun(ctx context.Context, r *ReindexRun) error

	// Operational: the sightings of prune (docs/spec/index-schema.md, Coordination), written by
	// reindex only. ResetDerived does not touch them.

	// RecordSighting inserts s or, when the row of (RepoID, Path, Digest) exists, sets its
	// LastAt; either way it fills FirstAt from the row. LastAt is now when zero, and FirstAt of
	// a new row is LastAt.
	RecordSighting(ctx context.Context, s *Sighting) error
	// ClearSightings removes the sightings of a repo whose LastAt is before before, and
	// returns how many it removed: those a run did not record again.
	ClearSightings(ctx context.Context, repoID int64, before time.Time) (int, error)

	// Derived. Every version write and delete keeps packages.created_at the earliest CreatedAt
	// of the package's versions.

	// UpsertPackage inserts p or, when (RepoID, Name) exists, fills p from the existing row.
	// NameOCI is unique within a repo too: when another name holds it, nothing changes and the
	// error wraps ErrConflict and names both (two spellings of one raw path, #97).
	UpsertPackage(ctx context.Context, p *Package) error
	// UpsertVersion inserts v or, when (PackageID, Version, Variant) exists with the same
	// digest, refreshes it in place (idempotent re-ingest). When (PackageID, Tag) exists with
	// a different digest it changes nothing and returns a *TagConflictError: tags are
	// append-only and never re-pointed.
	UpsertVersion(ctx context.Context, v *Version) error
	// TouchVersions sets IndexedAt of the versions with the given ids, and nothing else: the
	// rows a reindex saw without writing them. Ids that do not exist are skipped.
	TouchVersions(ctx context.Context, ids []int64, at time.Time) error
	// ReplaceFiles replaces the files of a version with files and fills their IDs.
	ReplaceFiles(ctx context.Context, versionID int64, files []File) error
	// DeleteVersion removes the version and its files.
	DeleteVersion(ctx context.Context, id int64) error
	// DeletePackage removes the package, its versions and their files.
	DeletePackage(ctx context.Context, id int64) error
	// DeleteEmptyPackages removes the packages of a repo that have no version, and returns how
	// many it removed.
	DeleteEmptyPackages(ctx context.Context, repoID int64) (int, error)

	// PutIndexCache inserts c or replaces the row with the same (RepoID, Scope, Kind) when
	// generation, the IndexCacheGeneration the render read before it began, is still the repo's,
	// and reports whether it stored c. False means that an invalidation came after the render
	// began: c may be of what it removed, and nothing changes. A put waits for an invalidation of
	// the repo that is not committed yet, and an invalidation for a put that is not, so that a
	// render never outlives the invalidation that follows it (docs/spec/guarantees.md, INV-27).
	PutIndexCache(ctx context.Context, c *Cache, generation int64) (bool, error)
	// InvalidateIndexCache increments the repo's IndexCacheGeneration and removes the cached
	// documents of a scope of the repo, or of every scope when scope is "".
	InvalidateIndexCache(ctx context.Context, repoID int64, scope string) error

	// UpsertOCIRepo inserts r or sets SeenAt of the row with the same (RepoID, Path).
	UpsertOCIRepo(ctx context.Context, r *OCIRepo) error
	// DeleteOCIRepo removes the row of (repoID, path): a repository that holds no tag any more
	// (docs/spec/index-schema.md, oci_repos). ErrNotFound when there is none. The caller lists
	// the repository's tags again after it, and records it again when one is there: a push
	// records its repository after its manifest PUT, so either the push comes after the delete
	// or its tag is in that listing.
	DeleteOCIRepo(ctx context.Context, repoID int64, path string) error
	// PruneOCIRepos removes the OCI repositories of a repo not seen since seenBefore and
	// returns how many it removed.
	PruneOCIRepos(ctx context.Context, repoID int64, seenBefore time.Time) (int, error)

	// ResetDerived removes every derived row of a repo, or of all repos when repoID is 0,
	// before a reindex, and increments their IndexCacheGeneration. It never touches
	// control-plane tables, tag claims or proxy_meta, which is a cache of the upstream and not
	// of the registry.
	ResetDerived(ctx context.Context, repoID int64) error

	// A cache of the upstream (ADR 0013; docs/spec/proxy-and-group.md §4). A write of a row
	// does not invalidate the repo's rendered documents of the name: the caller removes the
	// index_cache rows of scope Name in the same transaction, a 304 (TouchProxyMeta) none.

	// PutProxyMeta inserts m or replaces the row of (RepoID, Name, Kind), keeping its id, and
	// fills ID. CheckedAt is now when zero.
	PutProxyMeta(ctx context.Context, m *ProxyMeta) error
	// TouchProxyMeta sets CheckedAt of the row and nothing else: the upstream answered 304.
	TouchProxyMeta(ctx context.Context, id int64, checkedAt time.Time) error
	// DeleteProxyMeta removes the rows of the name key name in a repo, of every kind, or every
	// row of the repo when name is "", and returns how many it removed.
	DeleteProxyMeta(ctx context.Context, repoID int64, name string) (int, error)
	// ExpireProxyMeta removes the absent rows of a repo whose CheckedAt is before before, and
	// returns how many it removed: the sweep of negative entries, by the repo's negativeTTL.
	ExpireProxyMeta(ctx context.Context, repoID int64, before time.Time) (int, error)

	// The npm journal (derived; ADR 0012, docs/spec/index-schema.md, npm), written by ingest,
	// reindex and delete. Both tables go with their package: DeletePackage, DeleteEmptyPackages
	// and ResetDerived take a package's rows.

	// UpsertJournal inserts e and fills ID or, when the row of (PackageID, Seq) has e's tag and
	// digest, sets its IndexedAt and fills e from it. Another tag or digest under the Seq, or
	// e's tag under another Seq, is a *TagConflictError and nothing changes: an entry is never
	// rewritten. CreatedAt and IndexedAt are set to now when zero.
	UpsertJournal(ctx context.Context, e *JournalEntry) error
	// TouchJournal sets IndexedAt of the entries with the given ids, and nothing else: the
	// entries a reindex saw without writing them. Ids that do not exist are skipped.
	TouchJournal(ctx context.Context, ids []int64, at time.Time) error
	// DeleteJournal removes the journal entry: the sweep of reindex, the delete of an entry.
	DeleteJournal(ctx context.Context, id int64) error
	// ReplaceJournalState makes the journal_state rows of the package exactly state and fills
	// their PackageID: the fold of its entries, written in the transaction of the entries it
	// folds. A missing package wraps ErrNotFound; two rows of one (Kind, Key), ErrConflict.
	ReplaceJournalState(ctx context.Context, packageID int64, state []JournalState) error
}
