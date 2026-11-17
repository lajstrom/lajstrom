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
	// returning fn's error. The Writer must not be used after fn returns. With a ctx of
	// WithRepeatableRead, fn may run more than once, each time in a new transaction.
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
	// every token. The deploy tokens over a mask (Token.RepoMask) have neither, so only both
	// filters nil returns them.
	ListTokens(ctx context.Context, userID, repoID *int64) ([]Token, error)
	// ListTokenPage returns the tokens f keeps whose id is above f.After, by id, at most f.Limit
	// (docs/spec/index-schema.md, Pages of tokens and of open sessions); a Limit below 1 returns
	// none.
	ListTokenPage(ctx context.Context, f TokenFilter) ([]Token, error)

	// GetBootstrapToken returns the bootstrap token, used and expired ones included; the caller
	// checks UsedAt and ExpiresAt.
	GetBootstrapToken(ctx context.Context) (*BootstrapToken, error)

	// GetConfigState returns the configuration last recorded (docs/spec/rbac.md §5), or an
	// error wrapping ErrNotFound when none was.
	GetConfigState(ctx context.Context) (*ConfigState, error)

	// ListBindings returns the bindings of principal (canonical form, docs/spec/rbac.md §1),
	// or every binding when principal is "".
	ListBindings(ctx context.Context, principal string) ([]Binding, error)
	// ReadAccess returns the rows of the access report (docs/spec/rbac.md §14, #1279) in one
	// read-only transaction that reads one snapshot, as ExportControlPlane reads, at the
	// database's time, ReadAt (docs/spec/index-schema.md, The access report). It writes nothing.
	// Inside Tx it reads on the transaction of Tx.
	ReadAccess(ctx context.Context) (*Access, error)
	// The requests that wait for a second admin (docs/spec/rbac.md §13, docs/spec/index-schema.md,
	// Approvals).

	// GetApproval returns the request id, in any state; ErrNotFound when there is none.
	GetApproval(ctx context.Context, id int64) (*Approval, error)
	// ListApprovals returns the requests of state, or of any state when state is "", by id,
	// newest first: those whose id is below before (0: from the newest), at most limit; none when
	// limit is below 1.
	ListApprovals(ctx context.Context, state string, before int64, limit int) ([]Approval, error)

	// AuthVersion returns the counter of auth_version, which every write of bindings and of
	// repos increments in its transaction (docs/spec/rbac.md §3). An evaluator reads it before
	// the bindings and the repos of its snapshot, so that a snapshot is never newer than the
	// version it is kept under: a write between the reads makes the next request reload once
	// more.
	AuthVersion(ctx context.Context) (int64, error)

	// GetIdentity returns the OIDC identity of issuer and subject, the iss and sub of an ID
	// token (docs/spec/rbac.md §8).
	GetIdentity(ctx context.Context, issuer, subject string) (*Identity, error)
	// ListIdentityGroups returns the groups of every identity, with its user's id and uid and
	// its GroupsExpireAt, ordered by uid and name, expired or not: the RBAC snapshot compares
	// the time at each evaluation.
	ListIdentityGroups(ctx context.Context) ([]IdentityGroup, error)

	// GetKey returns the key of kind that is not retired; repoID nil selects the server key.
	GetKey(ctx context.Context, repoID *int64, kind string) (*Key, error)
	// ListKeys returns the keys of a repo, retired ones included; repoID nil selects the
	// server keys.
	ListKeys(ctx context.Context, repoID *int64) ([]Key, error)

	// GetSession returns the open publication session with key in a repo.
	GetSession(ctx context.Context, repoID int64, key string) (*Session, error)
	// GetSessionByID returns the publication session with the given id, in any state.
	GetSessionByID(ctx context.Context, id int64) (*Session, error)
	// ListOpenSessions returns the open publication sessions of a repo whose ExpiresAt is after
	// at and whose id is above after, by id, at most limit; never a blob upload of the facade,
	// whose key starts with blob-upload: (docs/spec/index-schema.md, Pages of tokens and of open
	// sessions). A limit below 1 returns none.
	ListOpenSessions(ctx context.Context, repoID int64, at time.Time, after int64, limit int) ([]Session, error)

	// ListAudit returns the audit events matching f, newest first; with f.Feed, those whose ID
	// is above f.After, by ID ascending (docs/spec/rbac.md §5, the feed; #1278).
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error)
	// SpanAudit counts the audit rows that PruneAudit would delete with the cut-off before: At
	// before it, State not AuditPending, and not a push whose Subject is a digest (a
	// referrer's, which the repair of a referrers index reads), with the first and the last of
	// their ids and times (docs/spec/index-schema.md, The audit feed and its prune).
	SpanAudit(ctx context.Context, before time.Time) (AuditSpan, error)

	// CI trust policies (docs/spec/rbac.md §11, docs/spec/index-schema.md, ci_policies).

	// ListCIPolicies returns the CI trust policies made through the API, ordered by id.
	ListCIPolicies(ctx context.Context) ([]CIPolicy, error)
	// GetCIPolicy returns the CI trust policy called name; ErrNotFound when there is none.
	GetCIPolicy(ctx context.Context, name string) (*CIPolicy, error)

	// Webhooks (docs/spec/webhooks.md, docs/spec/index-schema.md, Webhooks).

	// GetWebhook returns the webhook with the given id.
	GetWebhook(ctx context.Context, id int64) (*Webhook, error)
	// ListWebhooks returns every webhook, ordered by id.
	ListWebhooks(ctx context.Context) ([]Webhook, error)
	// ListWebhookDeliveries returns the delivery log of a webhook, newest first: the deliveries
	// whose id is below before (0: from the newest), at most limit; none when limit is below 1.
	ListWebhookDeliveries(ctx context.Context, webhookID, before int64, limit int) ([]WebhookDelivery, error)

	// Coordination.

	// GetTagClaim returns the claim on tag of the package path pkg (NameOCI) in a repo.
	GetTagClaim(ctx context.Context, repoID int64, pkg, tag string) (*TagClaim, error)
	// GetFetchClaim returns the claim of a proxy fetch on tag of the package path pkg in a repo.
	GetFetchClaim(ctx context.Context, repoID int64, pkg, tag string) (*FetchClaim, error)
	// GetReindexRun returns the reindex run with the given id.
	GetReindexRun(ctx context.Context, id int64) (*ReindexRun, error)
	// GetOIDCLogin returns the row of oidc_logins with the given id (docs/spec/rbac.md §8).
	GetOIDCLogin(ctx context.Context, id int64) (*OIDCLogin, error)
	// GetOIDCLoginByState returns the code login whose state hashes to state (hex sha256).
	GetOIDCLoginByState(ctx context.Context, state string) (*OIDCLogin, error)
	// GetOIDCLoginByDeviceCode returns the device request whose device code hashes to
	// deviceCode (hex sha256).
	GetOIDCLoginByDeviceCode(ctx context.Context, deviceCode string) (*OIDCLogin, error)
	// GetOIDCLoginByUserCode returns the device request of the user code, without its hyphen.
	GetOIDCLoginByUserCode(ctx context.Context, userCode string) (*OIDCLogin, error)
	// GetOIDCLoginByHandoff returns the code login whose handoff, the code its callback handed a
	// web UI, hashes to handoff (hex sha256; docs/spec/rbac.md §10).
	GetOIDCLoginByHandoff(ctx context.Context, handoff string) (*OIDCLogin, error)
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
	// ListPackagesAfter returns a page of ListPackages(prefix): the packages whose name comes
	// after after in byte order ("" = from the first), ordered by name, at most limit of them;
	// none when limit is below 1 (docs/spec/index-schema.md, Pages of packages and versions).
	ListPackagesAfter(ctx context.Context, repoID int64, prefix, after string, limit int) ([]Package, error)
	// ListPackageFacts returns the facts of the packages with the given distinct ids, one per
	// id in their order; the zero facts but PackageID for a package without versions. latest
	// picks each package's latest version; nil picks the first by VersionKey
	// (docs/spec/index-schema.md, The package list, one package and search).
	ListPackageFacts(ctx context.Context, packageIDs []int64, latest LatestFunc) ([]PackageFacts, error)
	// ListPackagePage returns a page of the packages of a repo that q keeps, in its order after
	// its key, each with its key (docs/spec/index-schema.md, The package list, one package and
	// search).
	ListPackagePage(ctx context.Context, repoID int64, q PackageQuery) ([]PackagePlace, error)
	// ListAttrValues returns the values of key in the Attrs of the versions rows of a repo,
	// each with the number of packages that have it, in byte order; none for a key that
	// PackageQuery.Attrs would match nothing with.
	ListAttrValues(ctx context.Context, repoID int64, key string) ([]AttrValue, error)
	// Search returns the hits of one rank of the search, after its key, in its order
	// (docs/spec/index-schema.md, The package list, one package and search).
	Search(ctx context.Context, q SearchQuery) ([]SearchHit, error)

	// GetVersion returns the version of a package by version and variant ("" when none).
	GetVersion(ctx context.Context, packageID int64, version, variant string) (*Version, error)
	// GetVersionByTag returns the version of a package that has tag.
	GetVersionByTag(ctx context.Context, packageID int64, tag string) (*Version, error)
	// GetVersionByDigest returns the version in a repo whose manifest digest is d.
	GetVersionByDigest(ctx context.Context, repoID int64, d digest.Digest) (*Version, error)
	// ListVersions returns the versions of a package ordered by CreatedAt, then ID.
	ListVersions(ctx context.Context, packageID int64) ([]Version, error)
	// ListVersionPage returns the rows of the first limit versions of a package whose
	// VersionKey comes after after (nil = from the newest) in the order newest first, every
	// variant of each: a version's rows are together, ordered by Variant. None when limit is
	// below 1 (docs/spec/index-schema.md, Pages of packages and versions). With without, kinds
	// of attachments (Attachment*), only the rows that have no attachment of any of them count,
	// for the versions and their keys alike (#89, ?without= of the versions list).
	ListVersionPage(ctx context.Context, packageID int64, after *VersionKey, limit int, without ...string) ([]Version, error)
	// ListUnseenVersions returns the versions of a repo whose IndexedAt is before seenBefore,
	// with their packages, ordered by package name and version id: the sweep of reindex.
	ListUnseenVersions(ctx context.Context, repoID int64, seenBefore time.Time) ([]VersionMatch, error)
	// ListVersionsWithoutSummary returns at most limit versions rows whose Summary was never
	// written and whose Metadata is kept, the rows before migration 0029, after the id afterID
	// in id order (docs/spec/index-schema.md, The summaries of the rows before 0029).
	ListVersionsWithoutSummary(ctx context.Context, afterID int64, limit int) ([]Version, error)

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

	// The journal of an npm or a pypi package (ADR 0012; docs/spec/index-schema.md, the journal).

	// ListJournal returns the journal entries of a package ordered by Seq.
	ListJournal(ctx context.Context, packageID int64) ([]JournalEntry, error)
	// ListJournalState returns the current dist-tags, deprecations and yanks of a package, the
	// fold of its journal, ordered by Kind and Key.
	ListJournalState(ctx context.Context, packageID int64) ([]JournalState, error)

	// Attachments (#89; docs/spec/index-schema.md, Attachments).

	// ListAttachments returns the attachments of the versions ids, newest first: CreatedAt
	// descending, those without one after the others, then by Digest and Layer.
	ListAttachments(ctx context.Context, versionIDs []int64) ([]Attachment, error)

	// RepoStats returns the counts of the derived tables of each repo of repoIDs that has a
	// package, in no order; a repo without a package has none (docs/spec/index-schema.md, A
	// repo's stats). It runs a fixed number of statements for every few hundred repos.
	RepoStats(ctx context.Context, repoIDs []int64) ([]RepoStats, error)

	// ListVersionFacts returns the facts of the versions with the given ids, one per id in their
	// order: its files counted and their sizes added up, its attachments counted by kind; the
	// zero facts but VersionID for an id without rows (docs/spec/index-schema.md, Pages of
	// packages and versions).
	ListVersionFacts(ctx context.Context, versionIDs []int64) ([]VersionFacts, error)

	// The policy (#101; docs/spec/policy.md, docs/spec/index-schema.md, Policy).

	// ListPolicyRules returns every policy rule, the expired ones too, by id.
	ListPolicyRules(ctx context.Context) ([]PolicyRule, error)
	// GetPolicyRule returns the rule id; ErrNotFound when there is none.
	GetPolicyRule(ctx context.Context, id int64) (*PolicyRule, error)
	// PolicyGeneration returns policy_state.generation, which every change of the rules and of
	// the verdicts increments.
	PolicyGeneration(ctx context.Context) (int64, error)
	// ListVerdicts returns the verdicts f selects, newest first (CreatedAt descending, those
	// without one last), then by Referrer and Layer.
	ListVerdicts(ctx context.Context, f VerdictFilter) ([]Verdict, error)
	// GetPolicySeen returns when the repo first saw the version without a publish time;
	// ErrNotFound when it has no row.
	GetPolicySeen(ctx context.Context, repoID int64, name, version, variant string) (*PolicySeen, error)

	// The mapping of an encrypted repo (#114; docs/spec/index-schema.md).

	// GetBlobMap returns the row of the stored digest d at the package path path of the repo.
	GetBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest) (*BlobMap, error)
	// FindBlobMap returns the rows of the plaintext digest d at the package path, of kind, or of
	// both kinds when kind is "", the newest first (IndexedAt, then ID).
	FindBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest, kind string) ([]BlobMap, error)
	// ListBlobMap returns the rows of the repo at the package path, or at every path when path is
	// "", ordered by Path, then Stored.
	ListBlobMap(ctx context.Context, repoID int64, path string) ([]BlobMap, error)
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

	// CreateRepo inserts r, at Revision 1; (format, name) must be new. It increments
	// AuthVersion, as UpdateRepo, DeleteRepo, CreateBinding and DeleteBinding do, in the same
	// transaction.
	CreateRepo(ctx context.Context, r *Repo) error
	// UpdateRepo stores r by r.ID, sets UpdatedAt and moves Revision by one. Format, Name and
	// Kind cannot change: they are filled from the row, and so is Revision.
	UpdateRepo(ctx context.Context, r *Repo) error
	// DeleteRepo removes the repo and every row that refers to it, proxy_meta and a group's
	// group_members rows included. A repo that is a member of a group is ErrConflict, and
	// nothing is deleted.
	DeleteRepo(ctx context.Context, id int64) error
	// ConvertRepo makes the proxy repo id a hosted one (docs/spec/proxy-and-group.md §2.6):
	// Kind KindHosted, Upstream NULL, UpdatedAt now, Revision moved by one, and AuthVersion
	// incremented. It is the one change of a kind; UpdateRepo makes none. A repo of another
	// kind, or with a fetch claim that beat at or after staleBefore (a fetch running), is
	// ErrConflict, and nothing changes; no repo is ErrNotFound.
	ConvertRepo(ctx context.Context, id int64, staleBefore time.Time) error
	// SetGroupMembers makes the group_members rows of the group groupID exactly memberIDs, in
	// that order, and invalidates the group's cached documents (InvalidateIndexCache of every
	// scope). An id that is no repo is ErrNotFound, an id named twice ErrConflict. The caller
	// writes the group's repos row in the same transaction, so that AuthVersion moves with it.
	SetGroupMembers(ctx context.Context, groupID int64, memberIDs []int64) error

	// CreateUser inserts u, at Revision 1; the name must be new. UID is made (NewUID) when it
	// is empty.
	CreateUser(ctx context.Context, u *User) error
	// UpdateUser stores Name, PasswordHash, Kind, Disabled, DeletedAt, DeletedName, Owner,
	// MustChange and PasswordHistory of u by u.ID, in one statement, moves Revision by one, and
	// fills UID, CreatedAt and Revision from the row. A name another user has is ErrConflict.
	UpdateUser(ctx context.Context, u *User) error
	// TouchUser sets LastSeenAt of the user to seenAt when it is null or earlier, else changes
	// nothing: it only moves forward, as TouchToken (#1283). It moves neither Revision nor
	// AuthVersion. No user wraps ErrNotFound.
	TouchUser(ctx context.Context, id int64, seenAt time.Time) error
	// The lockout (#1280; docs/spec/index-schema.md, The lockout). FailPassword adds one to
	// PasswordFailures unless LockedUntil is later than at, which changes nothing and returns
	// false; the failure that reaches limit sets LockedUntil to lockUntil and the count to 0, in
	// the same statement, and returns true, so that of callers that fail at once exactly one
	// locks. ResetPasswordFailures sets the count to 0 while LockedUntil is not later than at,
	// else changes nothing and wraps ErrConflict. UnlockUser sets the count to 0 and LockedUntil
	// to nil, and returns the LockedUntil it ended when that was later than at (nil: none). None
	// moves Revision or AuthVersion. No user wraps ErrNotFound.
	FailPassword(ctx context.Context, id int64, at, lockUntil time.Time, limit int) (bool, error)
	ResetPasswordFailures(ctx context.Context, id int64, at time.Time) error
	UnlockUser(ctx context.Context, id int64, at time.Time) (*time.Time, error)

	// CreateToken inserts t; the hash must be new.
	CreateToken(ctx context.Context, t *Token) error
	// RevokeToken sets RevokedAt of the token while it is null; a token revoked already keeps
	// its time and wraps ErrConflict, so that of callers that revoke it at once one does (#1277).
	// No token wraps ErrNotFound.
	RevokeToken(ctx context.Context, id int64, at time.Time) error
	// TouchToken sets LastUsedAt of the token to lastUsed when it is null or earlier, else
	// changes nothing: it only moves forward (#1277). No token wraps ErrNotFound.
	TouchToken(ctx context.Context, id int64, lastUsed time.Time) error

	// PutBootstrapToken makes t the bootstrap token when the stored one is still the one whose
	// hash is old: with old "" it inserts t when there is no row, otherwise it replaces the row
	// whose hash is old, UsedAt cleared. When the row is another (a replica made or replaced it
	// in between), nothing changes and the error wraps ErrConflict: of replicas that start
	// together, one token is made. CreatedAt is now when zero.
	PutBootstrapToken(ctx context.Context, t *BootstrapToken, old string) error
	// PutConfigState makes c the configuration recorded when the stored one is still the one of
	// fingerprint old: with old "" it inserts c when there is no row, otherwise it replaces the
	// row whose fingerprint is old. When the row is another (a replica recorded another
	// configuration in between), nothing changes and the error wraps ErrConflict, so that of
	// replicas that start together one records a change. Its caller appends the config.change
	// row in the same transaction. RecordedAt is now when zero.
	PutConfigState(ctx context.Context, c *ConfigState, old string) error
	// UseBootstrapToken sets UsedAt of the bootstrap token to at when its hash is hash, it is
	// not used and its ExpiresAt is after at; otherwise ErrNotFound. In a transaction it holds
	// the row until the transaction ends, so that of two exchanges of the token one commits and
	// the other finds it used.
	UseBootstrapToken(ctx context.Context, hash string, at time.Time) error

	// CreateBinding inserts b.
	CreateBinding(ctx context.Context, b *Binding) error
	// DeleteBinding removes the binding.
	DeleteBinding(ctx context.Context, id int64) error

	// The requests that wait for a second admin (docs/spec/rbac.md §13, #1285): control plane,
	// which grants nothing, so no write moves AuthVersion.

	// CreateApproval inserts a, pending whatever its State, and fills ID, State, and RequestedAt
	// when it is zero; a request of the same Kind and Subject pending wraps ErrConflict: the same
	// change pending twice.
	CreateApproval(ctx context.Context, a *Approval) error
	// DecideApproval stores State (ApprovalApproved or ApprovalRejected), DecidedBy, DecidedAt
	// (now when nil), DecisionReason and Result of a by a.ID while the row is pending, in one
	// statement, and fills a from the row. A row that is not pending changes nothing and wraps
	// ErrConflict, so that of two decisions one is made; no row wraps ErrNotFound. Its caller makes
	// the change of an approval in the same transaction.
	DecideApproval(ctx context.Context, a *Approval) error
	// ExpireApprovals marks expired the pending requests whose ExpiresAt is not after at, in one
	// statement, DecidedBy anonymous and DecidedAt at, and returns them as stored, by id, for
	// their audit rows: of the replicas that mark one at once, one returns it.
	ExpireApprovals(ctx context.Context, at time.Time) ([]Approval, error)

	// CreateIdentity inserts i and fills ID, and CreatedAt and LoginAt when zero; an identity
	// of the same Issuer and Subject wraps ErrConflict.
	CreateIdentity(ctx context.Context, i *Identity) error
	// DeleteIdentity removes the identity and its groups.
	DeleteIdentity(ctx context.Context, id int64) error
	// SetIdentityGroups makes the identity_groups rows of the identity exactly names, stores
	// loginAt and groupsExpireAt, and increments AuthVersion, in one transaction; a name given
	// twice wraps ErrConflict.
	SetIdentityGroups(ctx context.Context, id int64, names []string, loginAt, groupsExpireAt time.Time) error

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
	// name it had when deleted, for a deleted user). Only PruneAudit deletes audit rows, and only
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
	// PruneAudit deletes at most limit of the rows that SpanAudit counts with before whose id is
	// at most upTo, the lowest ids first, and returns how many it deleted. Outside Tx it is a
	// transaction of its own, so that a prune holds its rows for one batch only (#1278).
	PruneAudit(ctx context.Context, before time.Time, upTo int64, limit int) (int, error)

	// CI trust policies (docs/spec/rbac.md §11): control plane. Each write increments
	// AuthVersion in its transaction, as a write of bindings does.

	// CreateCIPolicy inserts p, whose name must be new (ErrConflict), at revision 1, and fills
	// ID, CreatedAt when it is zero, UpdatedAt, CreatedAt, and Revision; State "" is
	// CIPolicyActive, and UpdatedBy "" is CreatedBy.
	CreateCIPolicy(ctx context.Context, p *CIPolicy) error
	// UpdateCIPolicy stores Issuer, Claims, Grants, State, ExpiresAt, RenewalExpiresAt,
	// UpdatedBy, ApprovedBy and ApprovedAt of p by p.ID while the row's revision is p.Revision,
	// sets UpdatedAt, moves the revision by one, and fills Name, CreatedBy and CreatedAt from the
	// row: they never change. A row at another revision is ErrConflict, and nothing changes: of
	// two updates of what one read, one is made.
	UpdateCIPolicy(ctx context.Context, p *CIPolicy) error
	// DeleteCIPolicy removes the policy.
	DeleteCIPolicy(ctx context.Context, id int64) error

	// Webhooks (docs/spec/webhooks.md, docs/spec/index-schema.md, Webhooks): control plane.

	// CreateWebhook inserts h and fills ID, UID (NewUID) when it is empty, CreatedAt when it is
	// zero, UpdatedAt, and Revision, 1.
	CreateWebhook(ctx context.Context, h *Webhook) error
	// UpdateWebhook stores URL, Events, SecretEnc, Active and Description of h by h.ID, sets
	// UpdatedAt, moves Revision by one, and fills UID, Mask, CreatedBy and CreatedAt from the
	// row, which never change, and Revision.
	UpdateWebhook(ctx context.Context, h *Webhook) error
	// DeleteWebhook removes the webhook and its deliveries.
	DeleteWebhook(ctx context.Context, id int64) error
	// CreateWebhookDeliveries inserts ds, the outbox rows of an event, and fills their IDs:
	// State DeliveryPending when empty, CreatedAt now when zero, NextAt CreatedAt when zero. In
	// a transaction they commit or roll back with it: the outbox is written in the transaction
	// of the event's audit row (webhooks.md §4).
	CreateWebhookDeliveries(ctx context.Context, ds []WebhookDelivery) error
	// ClaimWebhookDeliveries claims for replica at most limit pending deliveries of active
	// webhooks whose NextAt is at or before now and that are unclaimed or claimed before
	// staleBefore, in NextAt order: ClaimedBy replica, ClaimedAt now. On Postgres the rows are
	// locked with SKIP LOCKED, so that two replicas that claim at once never take one row, nor
	// wait on each other.
	ClaimWebhookDeliveries(ctx context.Context, replica string, now, staleBefore time.Time, limit int) ([]WebhookClaim, error)
	// FinishWebhookDelivery stores State, Attempts, NextAt, LastAt, LastStatus, LastError and
	// DeliveredAt of d and clears its claim, while the row's claim is still d's ClaimedBy and
	// ClaimedAt; otherwise nothing changes and the error wraps ErrNotFound: the claim went stale
	// and another replica took the row over.
	FinishWebhookDelivery(ctx context.Context, d *WebhookDelivery) error
	// DeleteWebhookDeliveries removes the deliveries created before before, whatever their
	// state, and returns how many (webhooks.retention).
	DeleteWebhookDeliveries(ctx context.Context, before time.Time) (int, error)

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

	// CreateOIDCLogin inserts l and fills ID, and CreatedAt when zero; a state, device code,
	// user code or handoff that another row has wraps ErrConflict.
	CreateOIDCLogin(ctx context.Context, l *OIDCLogin) error
	// UpdateOIDCLogin stores Status, UserID, PolledAt, Handoff, AuthTime, AMR and ACR of l while
	// the row's status is from; a row whose status is not from wraps ErrConflict and nothing changes, so that a
	// state, an approval, a device code and a handoff are each used once.
	UpdateOIDCLogin(ctx context.Context, l *OIDCLogin, from string) error
	// ExpireOIDCLogins deletes the rows whose ExpiresAt is before before, and returns how many.
	ExpireOIDCLogins(ctx context.Context, before time.Time) (int, error)
	// UseDPoPProof stores key, the sha256 hex of a DPoP proof's key thumbprint and jti
	// (docs/spec/rbac.md §10, dpop_proofs), until expiresAt, in one statement: a key stored that
	// expires at now or later wraps ErrConflict, a proof used already, and nothing changes; one
	// that expired before now is taken over.
	UseDPoPProof(ctx context.Context, key string, expiresAt, now time.Time) error
	// ExpireDPoPProofs deletes the keys whose expiresAt is before before, and returns how many.
	ExpireDPoPProofs(ctx context.Context, before time.Time) (int, error)
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
	// SetVersionSummaries writes the summary of each id of summaries, "" for a manifest without
	// one, to those of the rows whose Summary was never written, in one transaction, and
	// returns how many it wrote: a row written since, or deleted, is skipped
	// (docs/spec/index-schema.md, The summaries of the rows before 0029).
	SetVersionSummaries(ctx context.Context, summaries map[int64]string) (int, error)

	// PutIndexCache inserts c or replaces the row with the same (RepoID, Scope, Kind) when
	// generation, the IndexCacheGeneration the render read before it began, is still the repo's,
	// and reports whether it stored c. False means that an invalidation came after the render
	// began: c may be of what it removed, and nothing changes. A put waits for an invalidation of
	// the repo that is not committed yet, and an invalidation for a put that is not, so that a
	// render never outlives the invalidation that follows it (docs/spec/guarantees.md, INV-27).
	PutIndexCache(ctx context.Context, c *Cache, generation int64) (bool, error)
	// InvalidateIndexCache increments the IndexCacheGeneration of the repo and of every group it
	// is a member of, and removes their cached documents of a scope, or of every scope when scope
	// is "": an apt group's documents are rendered from its members (docs/spec/index-schema.md).
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

	// ResetDerived removes every derived row of a repo, or of all repos when repoID is 0, and
	// increments their IndexCacheGeneration; of a repo, the cached documents of the groups it is a
	// member of go too, as InvalidateIndexCache does. Reindex never starts from it, it marks and sweeps
	// (docs/spec/index-schema.md). It never touches control-plane tables, tag claims or
	// proxy_meta, which is a cache of the upstream and not of the registry.
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

	// The journal of npm and pypi (derived; ADR 0012, docs/spec/index-schema.md), written by ingest,
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

	// The attachments of versions (derived, #89; docs/spec/index-schema.md, Attachments),
	// written by ingest, reindex and delete. They go with their version: DeleteVersion,
	// DeletePackage and ResetDerived take a version's rows.

	// ReplaceAttachments makes the rows of the referrer d of the version exactly rows, in one
	// transaction, and fills their ID, VersionID and Digest (none removes them). A row with a
	// Tag also removes the version's rows under that tag of another referrer: cosign moved its
	// legacy tag. IndexedAt is now when zero. A version that does not exist wraps ErrNotFound,
	// and nothing is written.
	ReplaceAttachments(ctx context.Context, versionID int64, d digest.Digest, rows []Attachment) error
	// TouchAttachments sets IndexedAt of the rows with the given ids, and nothing else: the rows
	// a reindex saw without reading their referrer. Ids that do not exist are skipped.
	TouchAttachments(ctx context.Context, ids []int64, at time.Time) error
	// DeleteAttachments removes the rows of the referrer d among the versions of a repo, the
	// delete of a referrer, and returns how many it removed.
	DeleteAttachments(ctx context.Context, repoID int64, d digest.Digest) (int, error)
	// SweepAttachments removes the rows of a version whose IndexedAt is before seenBefore, the
	// sweep of reindex once it listed the version's referrers, and returns how many it removed.
	SweepAttachments(ctx context.Context, versionID int64, seenBefore time.Time) (int, error)

	// The policy (#101; docs/spec/index-schema.md, Policy). ReplaceAttachments writes the
	// verdicts row of each row with a Verdict, and a verdicts row goes with its attachment. Every
	// method below that changes a row, and ReplaceAttachments and DeleteAttachments when they
	// write or remove a verdicts row, increment policy_state.generation in their transaction.

	// CreatePolicyRule inserts r and fills its ID; CreatedAt is now when zero.
	CreatePolicyRule(ctx context.Context, r *PolicyRule) error
	// DeletePolicyRule removes the rule id; ErrNotFound when there is none.
	DeletePolicyRule(ctx context.Context, id int64) error
	// ReplaceOCIVerdicts makes the verdicts rows of the referrer d of an oci repo, those without
	// an attachment, exactly rows, and fills their ID, RepoID and Referrer; IndexedAt is now when
	// zero.
	ReplaceOCIVerdicts(ctx context.Context, repoID int64, d digest.Digest, rows []Verdict) error
	// DeleteOCIVerdicts removes the verdicts rows without an attachment of the referrer d of a
	// repo, and returns how many.
	DeleteOCIVerdicts(ctx context.Context, repoID int64, d digest.Digest) (int, error)
	// TouchOCIVerdicts sets IndexedAt of the rows with the given ids; ids that do not exist are
	// skipped. It does not move the generation.
	TouchOCIVerdicts(ctx context.Context, ids []int64, at time.Time) error
	// SweepOCIVerdicts removes the rows without an attachment of a repo's OCI repository path
	// whose IndexedAt is before seenBefore, and returns how many.
	SweepOCIVerdicts(ctx context.Context, repoID int64, path string, seenBefore time.Time) (int, error)
	// PutPolicySeen inserts s unless the row of its key exists, and fills s.SeenAt with the
	// row's; SeenAt is now when zero. It does not move the generation.
	PutPolicySeen(ctx context.Context, s *PolicySeen) error

	// PutBlobMap inserts the rows, or replaces those of the same repo, path and stored digest,
	// in one transaction, and fills their ID; IndexedAt is now when zero.
	PutBlobMap(ctx context.Context, rows []BlobMap) error
	// DeleteBlobMap removes the row of the stored digest d at the package path; none is no error.
	DeleteBlobMap(ctx context.Context, repoID int64, path string, d digest.Digest) error
	// SweepBlobMap removes the rows of the repo at the package path whose IndexedAt is before
	// seenBefore, but those of kind BlobMapBlob indexed after keepBlobsAfter (a blob uploaded and
	// not yet named by a manifest), and returns how many it removed.
	SweepBlobMap(ctx context.Context, repoID int64, path string, seenBefore, keepBlobsAfter time.Time) (int, error)
}
