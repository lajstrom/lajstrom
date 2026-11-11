package index

import (
	"encoding/json"
	"time"

	"github.com/opencontainers/go-digest"
)

// Models: one struct per table of docs/spec/index-schema.md. Conventions: ids are int64;
// timestamps are UTC; free-form json columns (Policy, Metadata, Files, Details) are
// json.RawMessage, owned by the caller, while Scopes and Attrs have Go types and the store
// marshals them; a nullable foreign key or timestamp is a pointer (nil = NULL); a nullable text
// or json column is NULL when empty.

// Repo is a row of repos (control plane).
type Repo struct {
	ID            int64
	Format        string          // plugin name: raw, pypi, apt, maven
	Name          string          // unique per format
	Registry      string          // name of the backend registry in the configuration
	Prefix        string          // path of the repo's packages below the registry's instance prefix; the MVP always uses <format>/<name>
	Kind          string          // KindHosted, KindProxy or KindGroup; set by CreateRepo, never updated
	AnonymousRead bool            // read for everyone (docs/spec/rbac.md §3, step 5)
	Policy        json.RawMessage // maxFileSize, maven.sessionTimeout, …
	// Upstream is a proxy repo's settings (docs/spec/proxy-and-group.md §2.2): the upstream's
	// name, the TTLs, offline, allow and deny; nil on another kind. internal/proxy reads it.
	Upstream json.RawMessage
	// Group is a group's settings but its members (docs/spec/proxy-and-group.md §2.3), {"merge"};
	// nil on another kind. A group's members are group_members rows (Reader.ListGroupMembers,
	// Writer.SetGroupMembers), its Registry is "" and its Policy nil.
	Group     json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// The kinds of repos (docs/spec/proxy-and-group.md §1, ADR 0013).
const (
	KindHosted = "hosted" // holds what was published to it
	KindProxy  = "proxy"  // holds what it fetched from its upstream
	KindGroup  = "group"  // holds nothing: its members, in order
)

// User is a row of users (control plane).
type User struct {
	ID int64
	// UID is the user's identity, a UUID v7 (NewUID), made when the row is inserted and never
	// changed: principals refer to it, user:<uid> (docs/spec/rbac.md §1).
	UID string
	// Name is unique among the users; a deleted user's is ~<uid>, which no name can be.
	Name         string
	PasswordHash string // argon2id; empty for users without a password
	Kind         string // local; oidc after the MVP
	Disabled     bool
	CreatedAt    time.Time
	// DeletedAt is when the user was deleted; nil for a live user. DeletedName is the name it
	// had then.
	DeletedAt   *time.Time
	DeletedName string
}

// Token is a row of tokens (control plane): a personal access token or a deploy token.
type Token struct {
	ID         int64
	Kind       string   // pat or deploy
	UserID     *int64   // owner of a PAT
	RepoID     *int64   // repo of a deploy token
	Name       string   // label chosen by the creator
	Hash       string   // hex sha256 of the secret; the secret is never stored
	Scopes     []string // read and/or write; empty on a PAT means the user's full rights
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// BootstrapToken is the row of bootstrap_token (control plane): the one-time token that makes
// the first admin, the same on every replica. The table has at most one row.
type BootstrapToken struct {
	Hash      string // hex sha256 of the secret; the secret is never stored
	Replica   string // host name and process id of the serve that made it
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time // when it made the admin; it is never valid again
}

// Binding is a row of bindings (control plane): a role on a repo mask (docs/spec/rbac.md §3).
type Binding struct {
	ID        int64
	Principal string // canonical form: user:<name>, group:<name>, token:<id>, anonymous
	Role      string // reader, writer, maintainer, admin
	RepoMask  string // apt/ubuntu-internal, apt/*, */payments, */*
	CreatedAt time.Time
}

// Key is a row of keys (control plane): an apt signing key or the JWT signing key.
type Key struct {
	ID          int64
	RepoID      *int64 // nil for a server key
	Kind        string // gpg or jwt
	Public      []byte
	PrivateEnc  []byte // private key encrypted with the master key
	Fingerprint string
	CreatedAt   time.Time
	RetiredAt   *time.Time
}

// Session is a row of sessions (control plane): a publication session for a format whose
// upload protocol takes several requests (maven).
type Session struct {
	ID        int64
	RepoID    int64
	Key       string          // what the session publishes, e.g. g:a:v
	Principal string          // canonical form of the principal that opened it
	State     string          // open, committed, aborted, expired
	Files     json.RawMessage // descriptors of the blobs uploaded so far
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
}

// TagClaim is a row of tag_claims (coordination, neither control plane nor derived): the lock
// a publish holds on a tag from before the registry write until its index rows are written.
// It serialises the writers of one tag across replicas; the claim on a package path, with the
// tag "", the writers of its spellings (docs/spec/index-schema.md, Coordination).
type TagClaim struct {
	ID        int64
	RepoID    int64
	Package   string        // the package path (Package.NameOCI)
	Tag       string        // the OCI tag being published; "" on a package path
	Digest    digest.Digest // manifest digest being published; on a package path, of the spelling
	Principal string        // canonical form of the principal that publishes
	ClaimedAt time.Time
}

// FetchClaim is a row of fetch_claims (coordination): the lock a proxy fetch holds on one
// artifact from before its download until after its ingest, so that two replicas that miss it
// at once download it once (docs/spec/index-schema.md, Proxy and group repos). It is not a tag
// claim: the digest is unknown until the download ends, and the ingest inside the fetch takes
// its tag claims as every publish does.
type FetchClaim struct {
	ID          int64
	RepoID      int64
	Package     string // the package path (Plugin.EscapeName)
	Tag         string // profile.Tag of the version and variant
	Principal   string // canonical form of the principal whose request started the fetch
	Replica     string // host name and process id
	ClaimedAt   time.Time
	HeartbeatAt time.Time // stale 2 minutes after the last beat
}

// Replica is a row of replicas (coordination): a serve process on the database, from its start
// to its stop (docs/spec/index-schema.md, Coordination).
type Replica struct {
	ID          string // made at the start: a UUID v7 (NewUID)
	Host        string // host name
	PID         int
	Version     string // the binary's version
	StartedAt   time.Time
	HeartbeatAt time.Time // stale 2 minutes after the last beat
}

// The states of a reindex run (docs/spec/index-schema.md, Coordination).
const (
	ReindexRunning   = "running"   // the lock of its scope while HeartbeatAt is fresh
	ReindexDone      = "done"      // went to the end, whatever its report lists
	ReindexFailed    = "failed"    // stopped on an error of its own
	ReindexAbandoned = "abandoned" // its heartbeat stopped; closed by a later start
)

// ReindexRun is a row of reindex_runs (coordination): one run of reindex, and while it is
// running the lock that keeps overlapping runs apart across replicas and the CLI.
type ReindexRun struct {
	ID          int64
	RepoID      *int64 // nil: a run of every repo
	State       string // ReindexRunning, ReindexDone, ReindexFailed or ReindexAbandoned
	Source      string // cli, api, reconcile
	Principal   string // canonical form of the principal that started it
	Replica     string // host name and process id
	Options     ReindexOptions
	StartedAt   time.Time
	HeartbeatAt time.Time
	FinishedAt  *time.Time
	Report      json.RawMessage // the run's report (reindex.Report); nil while it runs
	Error       string          // why a run failed or was abandoned; "" otherwise
}

// ReindexOptions are the options a run was started with, stored as reindex_runs.options.
type ReindexOptions struct {
	Prune  bool `json:"prune,omitempty"`
	Verify bool `json:"verify,omitempty"`
}

// AuditEvent is a row of audit (control plane, append-only; docs/spec/rbac.md §5).
type AuditEvent struct {
	ID        int64
	At        time.Time
	Principal string // canonical form
	// PrincipalName is a user's name at the time of the event; AppendAudit looks it up by the
	// uid of Principal when it is empty. Empty for other principals.
	PrincipalName string
	Action        string          // push, delete, repo.create, token.create, …
	Repo          string          // <format>/<repo>, empty for system actions
	Subject       string          // package and version, or the user acted upon
	Digest        digest.Digest   // manifest digest, empty when none
	IP            string          // client address
	Details       json.RawMessage // anything else, e.g. the request id
	// State is the outcome of the change (AuditOK, …); AppendAudit stores AuditOK when it is
	// empty. Only the row of a registry write is ever another: pending before the write, then
	// ok, failed or unknown (docs/spec/index-schema.md, audit).
	State string
	Error string // why a failed row failed; empty otherwise
}

// The states of an audit row. A change of the index alone is ok from its insert, in the
// transaction of the change. A registry write has its row pending, in a transaction of its own
// before the write, and the row is completed after it: ok in the transaction that indexes the
// write, or failed with the error. A row that stays pending, because its writer stopped between
// the two, is unknown once it is older than ingest.claimTTL (Writer.ExpirePendingAudit).
const (
	AuditPending = "pending" // the registry write is under way
	AuditOK      = "ok"      // the change is done, and its answer was a success
	AuditFailed  = "failed"  // the write was answered with an error: the registry's, or the index's after it
	AuditUnknown = "unknown" // the writer stopped while pending: the registry may or may not have the write
)

// Package is a row of packages (derived).
type Package struct {
	ID        int64
	RepoID    int64
	Name      string // the format's spelling (lajstrom.name)
	NameOCI   string // the escaped package path (Plugin.EscapeName)
	CreatedAt time.Time
}

// Version is a row of versions (derived): one manifest.
type Version struct {
	ID           int64
	PackageID    int64
	Version      string
	Variant      string // empty when the format has none
	Tag          string
	Digest       digest.Digest // manifest digest
	ArtifactType string
	Attrs        map[string]string // lajstrom.<format>.* annotations
	Metadata     json.RawMessage   // metadata layer; nil when absent or omitted for size
	CreatedAt    time.Time         // org.opencontainers.image.created
	IndexedAt    time.Time         // when ingest or reindex last wrote or saw the row
	Degraded     bool              // written from the annotations alone: the metadata layer was unreadable
}

// File is a row of files (derived): one file layer of a version.
type File struct {
	ID        int64
	VersionID int64
	Name      string // org.opencontainers.image.title
	MediaType string
	Digest    digest.Digest // layer digest
	Size      int64
	Attrs     map[string]string // layer annotations
}

// VersionMatch is a version together with its package, as Reader.ListUnseenVersions returns it.
type VersionMatch struct {
	Version Version
	Package Package
}

// FileMatch is a file together with its version and package, as Reader.FindFiles returns it:
// the package's NameOCI locates the OCI repository that holds the blob.
type FileMatch struct {
	File    File
	Version Version
	Package Package
}

// Cache is a row of index_cache (derived): a rendered index document. It is called Cache, not
// IndexCache, because the package name already says index.
type Cache struct {
	ID         int64
	RepoID     int64
	Scope      string // jammy/main/amd64, simple/requests, com.example/lib-core
	Kind       string // Packages, Release, InRelease, simple, maven-metadata
	Content    []byte
	SHA256     string // hex sha256 of Content
	RenderedAt time.Time
	SignedAt   *time.Time
}

// OCIRepo is a row of oci_repos (derived): an OCI repository seen under a repo's path, for
// backends without a catalog and for reconcile.
type OCIRepo struct {
	ID     int64
	RepoID int64
	Path   string // <format>/<repo>/<package path>, without the instance prefix (as Provider names it)
	SeenAt time.Time
}

// The kinds of a journal_state row (docs/spec/index-schema.md, npm).
const (
	JournalDistTag    = "dist-tag"   // Key is the dist-tag, Value the version it names
	JournalDeprecated = "deprecated" // Key is the version, Value its deprecation message
)

// JournalEntry is a row of journal (derived): one entry of an npm package's journal, the manifest
// under the tag _j<seq> of the package's OCI repository (docs/spec/manifest-profile.md §8).
type JournalEntry struct {
	ID           int64
	PackageID    int64
	Seq          int64         // lajstrom.npm.seq: 1 for the first entry, one more for each next
	Tag          string        // _j and Seq zero-padded to eight digits: _j00000007
	Digest       digest.Digest // the entry's manifest digest
	ArtifactType string
	Attrs        map[string]string // the entry's lajstrom.npm.* annotations
	CreatedAt    time.Time         // org.opencontainers.image.created
	IndexedAt    time.Time         // when ingest or reindex last wrote or saw the row
}

// JournalState is a row of journal_state (derived): one dist-tag or one deprecation of an npm
// package, the fold of its journal in Seq order.
type JournalState struct {
	PackageID int64
	Kind      string // JournalDistTag or JournalDeprecated
	Key       string // the dist-tag; the version
	Value     string // the version; the message
	Seq       int64  // the entry that set it
}

// The states of a proxy_meta row.
const (
	ProxyMetaPresent = "present" // the upstream answered with a document
	ProxyMetaAbsent  = "absent"  // the upstream answered 404 or 410: a negative entry
)

// ProxyMeta is a row of proxy_meta (derived, a cache of the upstream: docs/spec/index-schema.md,
// Proxy and group repos): the upstream's document of one name of a proxy repo, as the plugin
// chose to keep it. Reindex neither reads nor writes it, and ResetDerived leaves it. Its expiry
// is not stored: it is CheckedAt plus the repo's TTL of its state, read with the row.
type ProxyMeta struct {
	ID          int64
	RepoID      int64
	Name        string // the plugin's key of the name (format.Proxier.Key)
	Kind        string // the plugin's document kind
	State       string // ProxyMetaPresent or ProxyMetaAbsent
	Content     []byte // the document as the plugin kept it; nil when absent
	ContentType string // of Content; "" when absent
	SHA256      string // hex sha256 of Content; "" when absent
	// ETag and LastModified are the upstream's validators as it sent them, for the conditional
	// request of a revalidation; "" when it sent none.
	ETag         string
	LastModified string
	CheckedAt    time.Time // when the upstream last answered for the name
}

// AuditFilter selects audit events for Reader.ListAudit. Empty strings and zero times do not
// filter; Since is inclusive and Until exclusive; Limit 0 means 100.
type AuditFilter struct {
	Principal string
	Repo      string
	Action    string
	State     string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}
