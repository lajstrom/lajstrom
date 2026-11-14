package index

import (
	"context"
	"time"
)

// Snapshots of the control plane (#76; docs/spec/index-schema.md, Snapshots of the control
// plane; docs/spec/manifest-profile.md §9): lajstrom admin snapshot create reads the control
// plane whole and writes it into the registry, lajstrom admin snapshot restore writes it back
// into an empty database, and reindex rebuilds the derived tables after it.

// Snapshotter reads and writes the control plane whole. Both engines implement it beside Store,
// as they do ApplyMigrations: serve does not use it.
type Snapshotter interface {
	// ExportControlPlane reads the control plane in one transaction, repeatable read on
	// Postgres, so that its rows are of one moment: the user and the repo of every token, the
	// repo of every key, the rows of every group member and identity are in it. It returns
	// before the caller talks to the registry, so no transaction is held across a request to it.
	// Schema is the newest migration applied to the database, ReadAt the database's time; the
	// audit rows are those opts selects.
	ExportControlPlane(ctx context.Context, opts ExportOptions) (*ControlPlane, error)

	// ImportControlPlane inserts the rows of cp with their ids, in one transaction. The tables
	// it writes (repos, group_members, users, identities, identity_groups, tokens, bindings,
	// keys and audit) must be empty: otherwise the error wraps ErrNotEmpty and names the tables
	// that have rows, and nothing changes. With opts.Force it first empties every table but
	// schema_migrations, auth_version and replicas, in the same transaction. The sequence of
	// each id is moved past the greatest id of cp, auth_version is incremented, and opts.Event,
	// when set, is appended to audit last. A cp whose Schema is newer than the newest migration
	// of the binary wraps ErrSchemaTooNew, and nothing changes; an older one is written as it is,
	// a column added since taking its default. A row that refers to a row cp lacks wraps
	// ErrNotFound, an id or a unique key given twice ErrConflict, and nothing changes.
	ImportControlPlane(ctx context.Context, cp *ControlPlane, opts ImportOptions) error
}

// ExportOptions select what ExportControlPlane reads besides the tables it always reads.
type ExportOptions struct {
	// WithAudit adds the audit rows whose At is at or after AuditSince; the zero AuditSince is
	// every row. Without it the snapshot has no audit row.
	WithAudit  bool
	AuditSince time.Time
}

// ImportOptions are the options of ImportControlPlane.
type ImportOptions struct {
	// Force empties the database first, instead of refusing one that is not empty: the restore
	// over a database that serve started on, or that holds a control plane to be replaced.
	// The derived rows go with the repos; reindex rebuilds them.
	Force bool
	// Event is appended to audit in the transaction of the import, after the rows of cp: the
	// record of the restore. nil appends nothing.
	Event *AuditEvent
}

// ControlPlane is the content of a snapshot: every row of the control-plane tables it carries,
// each with its id, so that the references between them (a token's user and repo, a key's
// repo, a group's members, an identity's user) and the principals user:<uid> and token:<id> of
// bindings, audit rows and manifests stay true after a restore. It holds no secret in clear, as
// the tables hold none: users carry argon2id hashes, tokens the sha256 of their secrets, keys
// their private keys encrypted with the master key. Each slice is ordered by ID, but
// GroupMembers by GroupID and Position, and IdentityGroups by IdentityID and Name.
type ControlPlane struct {
	// Schema is the newest migration applied to the database it was read from.
	Schema int
	// ReadAt is the database's time of the read.
	ReadAt time.Time

	Repos        []Repo
	GroupMembers []GroupMember
	// Users holds the live and the deleted users.
	Users      []User
	Identities []Identity
	// IdentityGroups are the rows of identity_groups: of each, IdentityID and Name are the row;
	// ExportControlPlane fills the others as ListIdentityGroups does, and ImportControlPlane
	// reads only those two.
	IdentityGroups []IdentityGroup
	Tokens         []Token
	Bindings       []Binding
	Keys           []Key

	// WithAudit and AuditSince are the ExportOptions of the read: Audit holds the rows they
	// selected, and none without WithAudit.
	WithAudit  bool
	AuditSince time.Time
	Audit      []AuditEvent
}

// GroupMember is a row of group_members (control plane, docs/spec/index-schema.md, Proxy and
// group repos): the repo MemberID is a member of the group GroupID at Position, from 0.
type GroupMember struct {
	GroupID  int64
	MemberID int64
	Position int
}
