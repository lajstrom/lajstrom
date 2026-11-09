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
	Kind          string          // hosted; proxy and group after the MVP
	AnonymousRead bool            // read for everyone (docs/spec/rbac.md §3, step 5)
	Policy        json.RawMessage // maxFileSize, maven.sessionTimeout, …
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

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
// It serialises the writers of one tag across replicas (docs/spec/index-schema.md,
// Coordination).
type TagClaim struct {
	ID        int64
	RepoID    int64
	Package   string        // the format's spelling (lajstrom.name)
	Tag       string        // the OCI tag being published
	Digest    digest.Digest // manifest digest being published
	Principal string        // canonical form of the principal that publishes
	ClaimedAt time.Time
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
}

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

// AuditFilter selects audit events for Reader.ListAudit. Empty strings and zero times do not
// filter; Since is inclusive and Until exclusive; Limit 0 means 100.
type AuditFilter struct {
	Principal string
	Repo      string
	Action    string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}
