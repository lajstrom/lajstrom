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
	// Revision is 1 on insert and moves by one with every update of the row (UpdateRepo,
	// ConvertRepo), which fills it: the version that the API's ETag names (docs/api.md,
	// Revisions). The store sets it; a value given to a write is not stored.
	Revision int64
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
	Kind         string // local, oidc or service (docs/spec/rbac.md §1)
	Disabled     bool
	CreatedAt    time.Time
	// DeletedAt is when the user was deleted; nil for a live user. DeletedName is the name it
	// had then.
	DeletedAt   *time.Time
	DeletedName string
	// Owner is the owner of a service account, user:<uid> or group:<name> (docs/spec/rbac.md
	// §1); "" for every other kind.
	Owner string
	// Revision is the row's revision, as Repo.Revision: UpdateUser moves it, a new password
	// and a deletion too.
	Revision int64
	// LastSeenAt is the user's last activity, its last successful authentication
	// (users.last_seen_at; docs/spec/rbac.md §1, Inactive users); nil when none is recorded.
	// TouchUser writes it, and ImportControlPlane; CreateUser and UpdateUser leave it.
	LastSeenAt *time.Time
}

// Token is a row of tokens (control plane): a personal access token or a deploy token.
type Token struct {
	ID         int64
	Kind       string   // pat or deploy
	UserID     *int64   // owner of a PAT
	RepoID     *int64   // repo of a deploy token of one repo
	Name       string   // label chosen by the creator
	Hash       string   // hex sha256 of the secret; the secret is never stored
	Scopes     []string // read and/or write; empty on a PAT means the user's full rights
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
	// RepoMask is the mask of a deploy token over a mask (docs/spec/rbac.md §1): <format>/<repo>,
	// each segment a name or "*", as a binding's (oci/*); "" for every other token. A deploy
	// token has RepoID or RepoMask, never both.
	RepoMask string
	// DPoPJKT is the JWK SHA-256 thumbprint, base64url, of the DPoP key that the token of a web
	// UI's sign-in is bound to (docs/spec/rbac.md §10, 0028); "" for every other token.
	DPoPJKT string
	// AuthTime and AMR are the auth_time and the amr of the ID token of the OIDC login that a web
	// UI's token was minted for (docs/spec/rbac.md §10, 0033): when and how the person
	// authenticated at the IdP. nil for every other token, and for a login whose ID token had
	// none.
	AuthTime *time.Time
	AMR      []string
}

// Identity is a row of identities (control plane): an OIDC identity, the iss and sub of an ID
// token, and the user it maps to (docs/spec/rbac.md §8).
type Identity struct {
	ID        int64
	UserID    int64
	Issuer    string
	Subject   string
	CreatedAt time.Time
	// LoginAt is the identity's latest login; GroupsExpireAt is when the groups of that login
	// stop counting, nil until SetIdentityGroups first stores it.
	LoginAt        time.Time
	GroupsExpireAt *time.Time
}

// IdentityGroup is a row of identity_groups with what the RBAC snapshot needs of its identity
// and user (ListIdentityGroups).
type IdentityGroup struct {
	IdentityID int64
	UserID     int64
	UID        string     // the user's uid
	Name       string     // the group
	ExpiresAt  *time.Time // the identity's GroupsExpireAt
}

// Kinds and statuses of oidc_logins (docs/spec/index-schema.md).
const (
	// OIDCLoginCode is a login of the authorization code flow, OIDCLoginDevice a device request.
	OIDCLoginCode   = "code"
	OIDCLoginDevice = "device"

	// OIDCPending is a row that waits: for its callback, or for its login.
	OIDCPending = "pending"
	// OIDCUsed is a code login whose state was redeemed.
	OIDCUsed = "used"
	// OIDCApproved, OIDCDenied and OIDCRedeemed are a device request whose login succeeded,
	// whose login was refused, and whose token was given out; and a code login of a web UI whose
	// callback handed the UI its code, whose code was tried with a wrong verifier, and whose code
	// was redeemed.
	OIDCApproved = "approved"
	OIDCDenied   = "denied"
	OIDCRedeemed = "redeemed"
)

// OIDCLogin is a row of oidc_logins (coordination): a login of the code flow or a device
// request, which lives ten minutes (docs/spec/rbac.md §8, §10). State, Binding, DeviceCode and
// Handoff are hex sha256 of the secrets, never the secrets; an empty string is NULL.
type OIDCLogin struct {
	ID   int64
	Kind string // OIDCLoginCode or OIDCLoginDevice
	// A code login: the hashes of its state and of the browser's cookie, its nonce and its PKCE
	// verifier, and the device request it approves, if any.
	State, Binding, Nonce, Verifier string
	DeviceID                        *int64
	// A device request: the hash of its device code, its user code without the hyphen, the
	// user it was approved for and its latest poll.
	DeviceCode, UserCode string
	// A code login that a web UI started: the page of the UI to return to, the S256 challenge of
	// the UI's PKCE verifier, the hash of the code its callback handed the UI, and the thumbprint
	// of the DPoP key the code is redeemed with (0028).
	ReturnTo, CodeChallenge, Handoff, DPoPJKT string
	// AuthTime and AMR are the auth_time and the amr of the ID token of a web UI's login, kept by
	// its callback for the token that redeems its code (0033); nil when it had none.
	AuthTime             *time.Time
	AMR                  []string
	Status               string
	UserID               *int64
	PolledAt             *time.Time
	CreatedAt, ExpiresAt time.Time
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

// ConfigState is the row of config_state: the configuration that serve last recorded with a
// config.change audit row (docs/spec/rbac.md §5, docs/spec/index-schema.md). It holds no secret.
type ConfigState struct {
	// Fingerprint is sha256:<hex> of the canonical JSON of the configuration's keys and values.
	Fingerprint string
	// Config is each key of the configuration that counts, to its value as JSON. A database
	// may return a value in another spelling of the same JSON (jsonb orders an object's keys):
	// compare decoded values.
	Config map[string]json.RawMessage
	// Release is the version of the serve that recorded it, Host its host name.
	Release string
	Host    string
	// RecordedAt is when; PutConfigState sets it to now when zero.
	RecordedAt time.Time
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
	Process     string // its boot, PID namespace and start as the OS tells them (cmd/lajstrom); "" unknown
	Flock       string // the device and inode of the lock file next to the SQLite file it holds (cmd/lajstrom); "" none
	Version     string // the binary's version
	StartedAt   time.Time
	HeartbeatAt time.Time // stale 2 minutes after the last beat
}

// Sighting is a row of prune_sightings (operational state): the registry answered 404
// MANIFEST_UNKNOWN to the digest of an index row's manifest (provider.ErrManifestUnknown).
// Reindex removes the row only on another such answer at least 10 minutes after FirstAt
// (docs/spec/index-schema.md, Coordination; guarantees.md, INV-5).
type Sighting struct {
	RepoID  int64
	Path    string        // the OCI repository, as oci_repos.path holds it
	Digest  digest.Digest // the manifest
	FirstAt time.Time     // the first answer since the row was last cleared
	LastAt  time.Time     // the latest answer
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

// CIPolicy is a row of ci_policies: a CI trust policy made through the API (docs/spec/rbac.md
// §11). The policies of the configuration are not rows.
type CIPolicy struct {
	ID int64
	// Name is unique, and never changes.
	Name string
	// Issuer is the name of an issuer of auth.ci.issuers.
	Issuer string
	// Claims maps each claim the policy names to its values.
	Claims map[string][]string
	// Grants are its roles on masks.
	Grants []CIGrant
	// State is CIPolicyActive, CIPolicyPending or CIPolicyExpired: only an active policy counts.
	State string
	// ExpiresAt is when a policy that grants writer or maintainer stops counting; nil for one
	// that grants reader only, which does not expire.
	ExpiresAt *time.Time
	// RenewalExpiresAt is the expiry of a renewal that waits for an admin's approval; nil when
	// none waits.
	RenewalExpiresAt *time.Time
	// CreatedBy is the principal that made it; it never changes.
	CreatedBy string
	// UpdatedBy is the principal of its latest create, change or renewal: the one admin who may
	// not approve what waits.
	UpdatedBy            string
	CreatedAt, UpdatedAt time.Time
	// ApprovedBy and ApprovedAt are the principal and the time of its latest approval; "" and
	// nil before one.
	ApprovedBy string
	ApprovedAt *time.Time
	// Revision is 1 on insert, and every update moves it by one.
	Revision int64
}

// The states of a CI trust policy of the API (docs/spec/rbac.md §11).
const (
	// CIPolicyActive counts.
	CIPolicyActive = "active"
	// CIPolicyPending grants nothing until a second admin approves it.
	CIPolicyPending = "pending"
	// CIPolicyExpired grants nothing: its ExpiresAt passed, and a replica marked it.
	CIPolicyExpired = "expired"
)

// CIGrant is a role on the repos of a mask, a grant of a CI trust policy.
type CIGrant struct {
	Role string `json:"role"`
	Mask string `json:"mask"`
}

// Webhook is a row of webhooks (control plane; docs/spec/webhooks.md §5): a URL that gets the
// events of the repos its mask matches.
type Webhook struct {
	ID          int64
	UID         string   // a UUID v7 (NewUID), the additional data of SecretEnc; never changes
	Mask        string   // a repo mask of docs/spec/rbac.md §3; never changes
	URL         string   // shown to who may manage the webhook, logged by its host only
	Events      []string // the types it gets (webhooks.md §1); nil: every type
	SecretEnc   []byte   // nonce || AES-256-GCM(secret) with the master key, UID as additional data
	Active      bool     // an inactive webhook gets no new event, and its pending deliveries wait
	Description string
	CreatedBy   string // the principal that created it; never changes
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Revision    int64 // the row's revision, as Repo.Revision: UpdateWebhook moves it, a new secret too
}

// The states of a webhook delivery.
const (
	DeliveryPending   = "pending"   // due at NextAt
	DeliveryDelivered = "delivered" // the receiver answered 2xx
	DeliveryFailed    = "failed"    // the attempts ran out, or the target was refused
)

// WebhookDelivery is a row of webhook_deliveries (control plane): the outbox of one event for
// one webhook, and its line of the delivery log (docs/spec/webhooks.md §4).
type WebhookDelivery struct {
	ID          int64
	WebhookID   int64
	EventID     string // the CloudEvents id, a UUID v7: one per event, the same on every attempt
	EventType   string // a type of webhooks.md §1, or webhook.ping
	Payload     []byte // the exact body sent
	State       string // DeliveryPending, DeliveryDelivered or DeliveryFailed
	Attempts    int
	NextAt      time.Time  // when a pending delivery is due
	ClaimedBy   string     // the replica whose worker holds it; "" when none does
	ClaimedAt   *time.Time // when it was claimed; a claim older than 2 minutes is stale
	LastAt      *time.Time // the last attempt
	LastStatus  int        // the status of the last answer; 0 when none came
	LastError   string     // why the last attempt was not a delivery, in Lajstrom's words
	CreatedAt   time.Time  // when the event was written
	DeliveredAt *time.Time
}

// WebhookClaim is a delivery that Writer.ClaimWebhookDeliveries claimed, with its webhook.
type WebhookClaim struct {
	Delivery WebhookDelivery
	Webhook  Webhook
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
	// Summary is one line of Metadata, profile.Summary (docs/spec/manifest-profile.md §2, The
	// summary): "" when the manifest has none, and in a row written before migration 0029. The
	// store writes it empty, not NULL, in a row that keeps its Metadata, so that a row written
	// before 0029 is told apart (ListVersionsWithoutSummary).
	Summary string
	// OCIRepo is the package path of the OCI repository that holds the manifest when it is not
	// the package's NameOCI: a version that reindex found at another form of the package's OCI
	// name (ADR 0017, a split, #642). "" otherwise, and in every row ingest writes. ManifestPath
	// says where the manifest is.
	OCIRepo string
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

// PackageSort is the order of a PackageQuery (docs/spec/http-routing.md §4, the package list).
type PackageSort string

// The sorts of a PackageQuery: by name, by the newest CreatedAt of a package's versions rows, by
// the count of its versions, by the size of its files.
const (
	SortName     PackageSort = "name"
	SortUpdated  PackageSort = "updated"
	SortVersions PackageSort = "versions"
	SortSize     PackageSort = "size"
)

// PackageQuery is a page of a repo's packages as Reader.ListPackagePage reads it: the packages
// that every filter given keeps, in the order of Sort, after the key After.
type PackageQuery struct {
	// Prefix keeps the packages whose name starts with it.
	Prefix string
	// Text keeps the packages whose name, or the Summary of one of whose versions rows, contains
	// it, ASCII letters in either case.
	Text string
	// Attrs keeps the packages with a versions row whose Attrs have, for every key, one of its
	// values. A key with a character other than a letter, a digit, '.', '_' and '-' matches
	// nothing.
	Attrs map[string][]string
	// Sort is the order, SortName when ""; Desc reverses it. Packages with the same value
	// follow by name, in the same direction.
	Sort PackageSort
	Desc bool
	// After is the key of the last package of the page before; nil for the first page.
	After *PackageKey
	// Limit bounds the page; below 1 it is empty.
	Limit int
}

// PackageKey is the place of a package in the order of a PackageQuery: its value of the sort,
// Updated for SortUpdated and N for SortVersions and SortSize, then its name.
type PackageKey struct {
	Updated time.Time
	N       int64
	Name    string
}

// PackagePlace is a package of a page of Reader.ListPackagePage with its key.
type PackagePlace struct {
	Package Package
	Key     PackageKey
}

// AttrValue is a value of a key of versions.attrs in a repo, and how many packages have a
// versions row with it.
type AttrValue struct {
	Value    string
	Packages int64
}

// SearchRank is a rank of the search, in the order of its hits (docs/spec/http-routing.md §4,
// search).
type SearchRank int

// The ranks of the search: a manifest, then a file layer, whose digest starts with the text; a
// package whose name is the text, starts with it, contains it; a package with a versions row
// whose Summary contains it. A package is in the first of its ranks only.
const (
	RankManifest SearchRank = iota
	RankFile
	RankExact
	RankPrefix
	RankSubstring
	RankSummary
)

// SearchQuery is one rank of the search, as Reader.Search reads it.
type SearchQuery struct {
	// RepoIDs are the repos searched: nil is every repo, and empty none.
	RepoIDs []int64
	Rank    SearchRank
	// Text is a digest's prefix, sha256: and lowercase hex, for RankManifest and RankFile; for
	// the others it compares as PackageQuery.Text, ASCII letters in either case.
	Text string
	// After is the key of the last hit of the page before in the rank; nil for its first.
	After *SearchKey
	// Limit bounds the hits; below 1 there are none.
	Limit int
}

// SearchKey is the place of a hit in its rank, whose order is by the repo's format and name
// and the package's name, then for a digest by the version, the variant and the file's name.
type SearchKey struct {
	Format, Repo, Package  string
	Version, Variant, File string
}

// SearchHit is a hit of Reader.Search: its key and its package, and for a digest its versions
// row, without Metadata, and its files row (RankFile).
type SearchHit struct {
	Key     SearchKey
	Package Package
	Version *Version
	File    *File
}

// PackageFacts is what Reader.ListPackageFacts answers of a package: counts and times over
// its versions rows, its latest and its last published version, and the dist-tags of its journal
// (docs/spec/index-schema.md, The package list, one package and search).
type PackageFacts struct {
	PackageID int64
	Versions  int64     // distinct versions
	Manifests int64     // versions rows, one per variant of each version
	Size      int64     // the sizes of the files rows of all its manifests, added up
	UpdatedAt time.Time // the newest CreatedAt of its manifests; zero without versions
	// Latest is the version that the LatestFunc of ListPackageFacts picked, "" for none, and
	// LatestVariants its variants in byte order.
	Latest         string
	LatestVariants []string
	// LastPublished is the first version by VersionKey, the newest by the publish time of its
	// first variant; "" without versions.
	LastPublished string
	// Tags are the dist-tags of its journal state that name one of its versions, by Key.
	Tags []JournalState
	// Head is the first variant in byte order of Latest, or of LastPublished when Latest is "",
	// without its Metadata; nil without versions.
	Head *Version
}

// LatestFunc picks the latest version of a package for Reader.ListPackageFacts among versions,
// its distinct versions newest first by VersionKey, with state, its journal state
// (Reader.ListJournalState); "" for none. The order of a format's versions is its plugin's
// (format.LatestPicker).
type LatestFunc func(packageID int64, versions []string, state []JournalState) string

// VersionMatch is a version together with its package, as Reader.ListUnseenVersions returns it.
type VersionMatch struct {
	Version Version
	Package Package
}

// VersionKey is the place of a version in Reader.ListVersionPage, which orders by it
// descending: CreatedAt, then Version in byte order (docs/spec/index-schema.md, Pages of
// packages and versions).
type VersionKey struct {
	CreatedAt time.Time // the earliest CreatedAt of the version's rows: its first variant
	Version   string
}

// TokenFilter is what Reader.ListTokenPage keeps: each field but Limit narrows when it is set.
type TokenFilter struct {
	Kind     string // pat or deploy
	UserID   *int64 // the personal access tokens of the user
	RepoID   *int64 // the deploy tokens of the repo, not those over a mask
	RepoMask string // the deploy tokens over the mask, as it was written
	// LiveAt keeps the tokens neither revoked nor expired at it: expires_at at or after it.
	LiveAt time.Time
	After  int64 // the id of the last token of the page before
	Limit  int
}

// RepoStats are the counts of a repo's derived rows, as Reader.RepoStats returns them
// (docs/spec/index-schema.md, A repo's stats).
type RepoStats struct {
	RepoID   int64
	Packages int64 // rows of packages
	// Versions are the distinct (package, version) of versions; Manifests its rows, one per
	// variant of a version.
	Versions  int64
	Manifests int64
	Files     int64 // rows of files of those versions
	Size      int64 // the sum of their files.size
	// LastPublishedAt is the newest versions.created_at; nil without a version.
	LastPublishedAt *time.Time
}

// FileMatch is a file together with its version and package, as Reader.FindFiles returns it:
// ManifestPath of the package and the version locates the OCI repository that holds the blob.
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
	Scope      string // jammy (apt suite), simple/requests, com.example/lib-core, @acme/widgets
	Kind       string // Release, main/binary-amd64/Packages.gz, simple.html, maven-metadata, packument
	Content    []byte
	SHA256     string // hex sha256 of Content
	RenderedAt time.Time
	SignedAt   *time.Time
	// ExpiresAt is when the document stops being served from the cache: the earliest time a
	// policy decision of its render holds until (docs/spec/policy.md §6.1); nil: never.
	ExpiresAt *time.Time
}

// Expired reports whether the document's ExpiresAt is at or before now.
func (ca *Cache) Expired(now time.Time) bool { return ca.ExpiresAt != nil && !ca.ExpiresAt.After(now) }

// OCIRepo is a row of oci_repos (derived): an OCI repository seen under a repo's path, for
// backends without a catalog and for reconcile.
type OCIRepo struct {
	ID     int64
	RepoID int64
	Path   string // <format>/<repo>/<package path>, without the instance prefix (as Provider names it)
	SeenAt time.Time
}

// The kinds of a journal_state row (docs/spec/index-schema.md, the journal).
const (
	JournalDistTag    = "dist-tag"   // Key is the dist-tag, Value the version it names
	JournalDeprecated = "deprecated" // Key is the version, Value its deprecation message
	JournalYanked     = "yanked"     // Key is the pypi version, Value its yank's reason, "" for none
)

// JournalEntry is a row of journal (derived): one entry of an npm or a pypi package's journal,
// the manifest under the tag _j<seq> of the package's OCI repository
// (docs/spec/manifest-profile.md §8).
type JournalEntry struct {
	ID           int64
	PackageID    int64
	Seq          int64         // lajstrom.<format>.seq: 1 for the first entry, one more for each next
	Tag          string        // _j and Seq zero-padded to eight digits: _j00000007
	Digest       digest.Digest // the entry's manifest digest
	ArtifactType string
	Attrs        map[string]string // the entry's lajstrom.npm.* or lajstrom.pypi.* annotations
	CreatedAt    time.Time         // org.opencontainers.image.created
	IndexedAt    time.Time         // when ingest or reindex last wrote or saw the row
}

// JournalState is a row of journal_state (derived): one dist-tag or one deprecation of an npm
// package, or one yank of a pypi package, the fold of its journal in Seq order.
type JournalState struct {
	PackageID int64
	Kind      string // JournalDistTag, JournalDeprecated or JournalYanked
	Key       string // the dist-tag; the version; the version
	Value     string // the version; the message; the reason
	Seq       int64  // the entry that set it
}

// The kinds of an attachment (docs/spec/manifest-profile.md §6.1), the values of
// profile.Referrer*. A binary reads a kind it does not know as it is.
const (
	AttachmentSignature   = "signature"
	AttachmentSBOM        = "sbom"
	AttachmentScan        = "scan"
	AttachmentProvenance  = "provenance"
	AttachmentAttestation = "attestation"
	AttachmentOther       = "other"
)

// The labels of an attachment (docs/spec/manifest-profile.md §6.3), the values of attest's.
const (
	AttachmentVerified    = "verified"
	AttachmentUntrusted   = "untrusted"
	AttachmentUnsigned    = "unsigned"
	AttachmentInvalid     = "invalid"
	AttachmentUnsupported = "unsupported"
)

// The kinds of a row of blob_map.
const (
	// BlobMapBlob is the row of an encrypted layer: a blob of the registry.
	BlobMapBlob = "blob"
	// BlobMapManifest is the row of a stored manifest whose layers are encrypted.
	BlobMapManifest = "manifest"
)

// BlobMap is a row of blob_map (derived, #114; docs/spec/index-schema.md, The mapping of an
// encrypted repo): the digest a client knows of a blob or a manifest of an encrypted repo, the one
// the registry has, and what a read needs to decrypt the blob.
type BlobMap struct {
	ID     int64
	RepoID int64
	// Path is the package path, as Package.NameOCI, of the OCI repository that holds it.
	Path string
	// Kind is BlobMapBlob or BlobMapManifest.
	Kind string
	// Plain is the plaintext's digest, or the digest of the manifest as the client had it.
	Plain digest.Digest
	// Stored is the ciphertext's digest, or the stored manifest's.
	Stored digest.Digest
	// Size is the blob's, the same in both forms; the stored manifest's.
	Size int64
	// KeyFingerprint is the enc key that Wrapped opens with; "" for a manifest.
	KeyFingerprint string
	// Wrapped is the private options wrapped for that key, the value of the annotation
	// org.opencontainers.image.enc.keys.jwe or of a keys referrer's member; "" for a manifest.
	Wrapped string
	// PubOpts is the value of org.opencontainers.image.enc.pubopts; "" for a manifest.
	PubOpts string
	// IndexedAt is when a writer last wrote or saw the row.
	IndexedAt time.Time
}

// Attachment is a row of attachments (derived, #89): one layer of a referrer of a version's
// manifest, a signature, an SBOM, a scan report or another attestation, with its label
// (docs/spec/index-schema.md, Attachments).
type Attachment struct {
	ID            int64
	VersionID     int64
	Digest        digest.Digest     // the referrer manifest
	Tag           string            // cosign's legacy tag; "" for a referrer pushed by digest
	Layer         digest.Digest     // the attachment's layer; "" for a manifest without layers
	MediaType     string            // the layer's; the artifact type for a manifest without layers
	ArtifactType  string            // the manifest's artifactType, else its config's media type
	Kind          string            // Attachment{Signature,SBOM,Scan,Provenance,Attestation,Other}
	PredicateType string            // "" when none
	Size          int64             // the layer's
	Annotations   map[string]string // the manifest's
	Tool          string            // lajstrom.attachment.tool
	ToolVersion   string            // lajstrom.attachment.toolVersion
	Database      string            // lajstrom.attachment.database
	Input         digest.Digest     // lajstrom.attachment.input when it is a digest
	Verification  string            // Attachment{Verified,Untrusted,Unsigned,Invalid,Unsupported}
	SignedBy      string            // the trust entry whose key verified it; "" (NULL) unless verified
	Reason        string            // why it is not verified; "" when it is
	Trust         string            // the hash of the trust that labelled it (attest.Trust.Hash)
	CreatedAt     *time.Time        // the manifest's org.opencontainers.image.created; nil without one
	IndexedAt     time.Time         // when ingest or reindex last wrote or saw the row
	// Verdict is the verdict this attachment is, when it is one that verifies
	// (docs/spec/policy.md §2): ReplaceAttachments writes its verdicts row with it. Not read
	// back by ListAttachments.
	Verdict *Verdict
}

// The effects of a policy rule and the verdicts of a verdict (docs/spec/policy.md).
const (
	PolicyAllow      = "allow"
	PolicyDeny       = "deny"
	PolicyQuarantine = "quarantine" // a verdict's only
)

// PolicyRule is a row of policy_rules (control plane, docs/spec/policy.md §1): a pattern over
// coordinates that denies or allows. A rule never changes.
type PolicyRule struct {
	ID        int64
	RepoMask  string     // a repo mask of docs/spec/rbac.md §3
	Name      string     // a pattern of the package name; * any run of characters
	Version   string     // a version constraint (policy.md §1.2) as written; "*" every version
	Variant   string     // a pattern of the variant; "*" every variant
	Effect    string     // PolicyDeny or PolicyAllow
	Reason    string     // shown to clients
	CreatedBy string     // the principal that created it
	CreatedAt time.Time  // when
	ExpiresAt *time.Time // when it stops holding; nil: never
}

// Expired reports whether the rule's ExpiresAt is at or before now.
func (r *PolicyRule) Expired(now time.Time) bool {
	return r.ExpiresAt != nil && !r.ExpiresAt.After(now)
}

// Verdict is a row of verdicts (derived, docs/spec/policy.md §2): one verdict attestation that
// verifies, a layer of a referrer of the manifest Subject. On a managed repo it belongs to its
// attachments row (AttachmentID), on an oci repo to no row (AttachmentID 0).
type Verdict struct {
	ID           int64
	RepoID       int64
	AttachmentID int64         // the attachments row; 0 on an oci repo
	Path         string        // the package path of the OCI repository that holds the referrer
	Subject      digest.Digest // the judged manifest
	Referrer     digest.Digest // the referrer manifest
	Layer        digest.Digest // the attachment's layer; "" for a manifest without layers
	Verdict      string        // PolicyAllow, PolicyDeny or PolicyQuarantine
	Reason       string
	Source       string
	Scope        string     // a repo mask; "" the repo itself
	ValidUntil   *time.Time // nil: no end
	SignedBy     string     // the trust entry that verified it
	CreatedAt    *time.Time // the referrer manifest's org.opencontainers.image.created; nil without one
	IndexedAt    time.Time  // when ingest or reindex last wrote or saw the row
}

// Expired reports whether the verdict's ValidUntil is at or before now.
func (v *Verdict) Expired(now time.Time) bool { return v.ValidUntil != nil && !v.ValidUntil.After(now) }

// VerdictFilter selects verdicts for Reader.ListVerdicts. RepoID 0 and Subject "" do not
// filter; Scoped also selects, whatever RepoID says, the rows of any repo whose Scope is not
// "", which may hold in another repo than their own.
type VerdictFilter struct {
	RepoID  int64
	Subject digest.Digest
	Scoped  bool
}

// PolicySeen is a row of policy_seen (a cache, docs/spec/policy.md §4): when a proxy repo with a
// quarantine first saw a version in its upstream's document without a publish time.
type PolicySeen struct {
	RepoID  int64
	Name    string // the plugin's key of the name
	Version string
	Variant string
	SeenAt  time.Time
}

// VersionFacts is what Reader.ListVersionFacts answers of a versions row, one manifest, beyond
// the row itself: what the list of versions shows of a variant (#1132; docs/spec/index-schema.md,
// Pages of packages and versions).
type VersionFacts struct {
	VersionID int64
	Files     int64 // its files rows
	Size      int64 // the sizes of its files rows, added up
	// Attachments counts its attachments rows by Kind; a kind without rows is absent, and a
	// kind this binary does not know is counted as it is.
	Attachments map[string]int64
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
