package sqlstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The snapshots of the control plane (#76; docs/spec/index-schema.md, Snapshots of the control
// plane): the whole of the tables a snapshot carries, read in one transaction and written back
// with their ids.

var _ index.Snapshotter = (*Store)(nil)

// SnapshotTables are the tables ImportControlPlane writes, in the order of their references; each
// must be empty unless the import is forced.
var SnapshotTables = []string{"repos", "group_members", "users", "identities", "identity_groups", "tokens", "bindings", "keys",
	"audit"}

// ForcedTables are the tables a forced import empties, every row that refers to another before
// it: every table but KeptTables. A test of each engine lists the tables of the schema and fails
// when a migration adds one that neither list names.
var ForcedTables = []string{
	"identity_groups", "oidc_logins", "identities", "group_members", "tokens", "bindings", "keys", "sessions", "audit",
	"bootstrap_token", "journal_state", "journal", "files", "versions", "packages", "index_cache", "index_cache_generations",
	"oci_repos", "proxy_meta", "tag_claims", "fetch_claims", "reindex_runs", "prune_sightings", "repos", "users",
}

// KeptTables are the tables a forced import leaves: the schema's version, the counter it
// increments, and the rows of the serve processes, which the restore checks before it runs.
var KeptTables = []string{"schema_migrations", "auth_version", "replicas"}

// sequenced are the tables of a snapshot with an id of their own, whose sequence an import moves
// past the ids it writes.
var sequenced = []string{"repos", "users", "identities", "tokens", "bindings", "keys", "audit"}

// ExportControlPlane reads the control plane in one transaction that reads one snapshot
// (Dialect.ReadSnapshot), run again when the database aborts it all the same, up to TxAttempts
// times in all.
func (s *Store) ExportControlPlane(ctx context.Context, opts index.ExportOptions) (*index.ControlPlane, error) {
	var cp *index.ControlPlane
	read := func(c *conn) (err error) {
		if s.d.ReadSnapshot != "" {
			if _, err := c.exec(ctx, "the snapshot of the export", s.d.ReadSnapshot); err != nil {
				return err
			}
		}
		cp, err = c.exportControlPlane(ctx, opts)
		return err
	}
	for attempt := 1; ; attempt++ {
		err := s.begin(ctx, nil, read)
		if err == nil {
			return cp, nil
		}
		if attempt == TxAttempts || s.d.IsAborted == nil || !s.d.IsAborted(err) || ctx.Err() != nil {
			return nil, err
		}
	}
}

// exportControlPlane reads the tables of a snapshot on c, a transaction.
func (c *conn) exportControlPlane(ctx context.Context, opts index.ExportOptions) (*index.ControlPlane, error) {
	cp := &index.ControlPlane{WithAudit: opts.WithAudit}
	if err := c.scanRow(ctx, "schema_migrations", "SELECT COALESCE(MAX(version), 0) FROM schema_migrations", nil,
		&cp.Schema); err != nil {
		return nil, err
	}
	t, err := c.DatabaseTime(ctx)
	if err != nil {
		return nil, err
	}
	cp.ReadAt = Normalize(t)
	reads := []func() error{
		func() (err error) {
			cp.Repos, err = getAll(ctx, c, "repos", repoDests, "SELECT "+repoCols+" FROM repos ORDER BY id")
			return err
		},
		func() (err error) {
			cp.GroupMembers, err = getAll(ctx, c, "group_members", func(g *index.GroupMember) []any {
				return []any{&g.GroupID, &g.MemberID, &g.Position}
			}, "SELECT group_id, member_id, position FROM group_members ORDER BY group_id, position")
			return err
		},
		func() (err error) {
			cp.Users, err = getAll(ctx, c, "users", userDests, "SELECT "+userCols+" FROM users ORDER BY id")
			return err
		},
		func() (err error) {
			cp.Identities, err = getAll(ctx, c, "identities", identityDests, "SELECT "+identityCols+" FROM identities ORDER BY id")
			return err
		},
		func() (err error) {
			cp.IdentityGroups, err = getAll(ctx, c, "identity_groups", identityGroupDests, "SELECT g.identity_id, i.user_id, u.uid,"+
				" g.name, i.groups_expire_at FROM identity_groups g JOIN identities i ON i.id = g.identity_id"+
				" JOIN users u ON u.id = i.user_id ORDER BY g.identity_id, g.name")
			return err
		},
		func() (err error) {
			cp.Tokens, err = getAll(ctx, c, "tokens", tokenDests, "SELECT "+tokCols+" FROM tokens ORDER BY id")
			return err
		},
		func() (err error) {
			cp.Bindings, err = getAll(ctx, c, "bindings", bindingDests, "SELECT "+bindingCols+" FROM bindings ORDER BY id")
			return err
		},
		func() (err error) {
			cp.Keys, err = getAll(ctx, c, "keys", keyDests, "SELECT "+keyCols+" FROM keys ORDER BY id")
			return err
		},
		func() error { return c.exportAudit(ctx, cp, opts) },
	}
	for _, read := range reads {
		if err := read(); err != nil {
			return nil, err
		}
	}
	return cp, nil
}

// exportAudit reads the audit rows that opts selects into cp: none without WithAudit, every row
// with a zero AuditSince, those at or after it otherwise.
func (c *conn) exportAudit(ctx context.Context, cp *index.ControlPlane, opts index.ExportOptions) (err error) {
	switch {
	case !opts.WithAudit:
	case opts.AuditSince.IsZero():
		cp.Audit, err = getAll(ctx, c, "audit", auditDests, "SELECT "+auditCols+" FROM audit ORDER BY id")
	default:
		cp.AuditSince = Normalize(opts.AuditSince)
		cp.Audit, err = getAll(ctx, c, "audit", auditDests, "SELECT "+auditCols+" FROM audit WHERE at >= ? ORDER BY id",
			c.d.EncodeTime(cp.AuditSince))
	}
	return err
}

// identityGroupDests are the columns of a row of identity_groups with its identity's and user's.
func identityGroupDests(g *index.IdentityGroup) []any {
	return []any{&g.IdentityID, &g.UserID, textDest{&g.UID}, &g.Name, nullTimeDest{&g.ExpiresAt}}
}

// ImportControlPlane inserts the rows of cp with their ids, in one transaction.
func (s *Store) ImportControlPlane(ctx context.Context, cp *index.ControlPlane, opts index.ImportOptions) error {
	versions, err := index.MigrationVersions(s.d.Name)
	if err != nil {
		return err
	}
	if newest := slices.Max(versions); cp.Schema > newest {
		return fmt.Errorf("%w: the snapshot is of migration %d, and this binary knows %d at the newest: restore it with a binary "+
			"that knows %d", index.ErrSchemaTooNew, cp.Schema, newest, cp.Schema)
	}
	return s.withTx(ctx, func(c *conn) error {
		if err := c.emptyControlPlane(ctx, opts.Force); err != nil {
			return err
		}
		if err := c.importRows(ctx, cp); err != nil {
			return err
		}
		if err := c.moveSequences(ctx); err != nil {
			return err
		}
		if err := c.execOne(ctx, "auth_version", "UPDATE auth_version SET version = version + 1, updated_at = ? WHERE id = 1",
			c.d.EncodeTime(now())); err != nil {
			return err
		}
		if opts.Event != nil {
			return c.AppendAudit(ctx, opts.Event)
		}
		return nil
	})
}

// moveSequences moves the sequence of the id of each table of sequenced past the greatest id it
// holds, and never back (Dialect.NextID): it takes the next value, and when that is not past the
// greatest id, as many more as there are ids up to it. The values taken are gaps, as those of a
// rolled back insert are.
func (c *conn) moveSequences(ctx context.Context) error {
	if c.d.NextID == "" {
		return nil
	}
	for _, t := range sequenced {
		what, next := "the sequence of "+t, strings.ReplaceAll(c.d.NextID, "%s", t)
		var got, greatest int64
		if err := c.scanRow(ctx, what, next+", (SELECT COALESCE(MAX(id), 0) FROM "+t+")", nil, &got, &greatest); err != nil {
			return err
		}
		if got < greatest {
			if _, err := c.exec(ctx, what, next+" FROM generate_series(1, ?)", greatest-got); err != nil {
				return err
			}
		}
	}
	return nil
}

// emptyControlPlane empties ForcedTables when force, and otherwise fails with ErrNotEmpty, naming
// them, when a table of SnapshotTables has a row.
func (c *conn) emptyControlPlane(ctx context.Context, force bool) error {
	if force {
		for _, t := range ForcedTables {
			if _, err := c.exec(ctx, t, "DELETE FROM "+t); err != nil {
				return err
			}
		}
		return nil
	}
	var full []string
	for _, t := range SnapshotTables {
		var one int
		err := c.scanRow(ctx, t, "SELECT 1 FROM "+t+" LIMIT 1", nil, &one)
		switch {
		case err == nil:
			full = append(full, t)
		case !errors.Is(err, index.ErrNotFound):
			return err
		}
	}
	if len(full) > 0 {
		return fmt.Errorf("%w: %s %s rows; restore into an empty database, or force it to replace what it holds", index.ErrNotEmpty,
			strings.Join(full, ", "), map[bool]string{true: "has", false: "have"}[len(full) == 1])
	}
	return nil
}

// insertRow is one row of an import: what names it in an error, cols are its table's columns, as
// many as args.
type insertRow struct {
	what, table, cols string
	args              []any
}

// importRows inserts the rows of cp with their ids, every table after those it refers to.
func (c *conn) importRows(ctx context.Context, cp *index.ControlPlane) error {
	for _, r := range c.rowsOf(cp) {
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(r.args)), ", ")
		if _, err := c.exec(ctx, r.what, "INSERT INTO "+r.table+" ("+r.cols+") VALUES ("+marks+")", r.args...); err != nil {
			return err
		}
	}
	return nil
}

// rowsOf returns the rows of cp in the order importRows inserts them. A column that cp's schema
// may lack takes its default: a repo's kind hosted, a user's kind local, an audit row's state ok.
func (c *conn) rowsOf(cp *index.ControlPlane) []insertRow {
	var rows []insertRow
	add := func(what, table, cols string, args ...any) {
		rows = append(rows, insertRow{what: what, table: table, cols: cols, args: args})
	}
	for _, r := range cp.Repos {
		add(fmt.Sprintf("repos %s/%s", r.Format, r.Name), "repos", repoCols, r.ID, r.Format, r.Name, r.Registry, r.Prefix,
			cmp.Or(r.Kind, index.KindHosted), r.AnonymousRead, rawArg(r.Policy), rawArg(r.Upstream), rawArg(r.Group),
			c.d.EncodeTime(r.CreatedAt), c.d.EncodeTime(cmp.Or(r.UpdatedAt, r.CreatedAt)))
	}
	for _, g := range cp.GroupMembers {
		add(fmt.Sprintf("group_members %d of %d", g.MemberID, g.GroupID), "group_members", "group_id, member_id, position",
			g.GroupID, g.MemberID, g.Position)
	}
	for _, u := range cp.Users {
		add("users "+u.Name, "users", userCols, u.ID, u.UID, u.Name, textArg(u.PasswordHash), cmp.Or(u.Kind, "local"), u.Disabled,
			c.d.EncodeTime(u.CreatedAt), c.d.encodeNullTime(u.DeletedAt), textArg(u.DeletedName))
	}
	for _, i := range cp.Identities {
		add(fmt.Sprintf("identities id %d", i.ID), "identities", identityCols, i.ID, i.UserID, i.Issuer, i.Subject,
			c.d.EncodeTime(i.CreatedAt), c.d.EncodeTime(cmp.Or(i.LoginAt, i.CreatedAt)), c.d.encodeNullTime(i.GroupsExpireAt))
	}
	for _, g := range cp.IdentityGroups {
		add(fmt.Sprintf("identity_groups %q of %d", g.Name, g.IdentityID), "identity_groups", "identity_id, name", g.IdentityID, g.Name)
	}
	for _, t := range cp.Tokens {
		add(fmt.Sprintf("tokens id %d", t.ID), "tokens", tokCols, t.ID, t.Kind, idArg(t.UserID), idArg(t.RepoID), t.Name, t.Hash,
			scopesArg(t.Scopes), c.d.encodeNullTime(t.ExpiresAt), c.d.encodeNullTime(t.LastUsedAt), c.d.EncodeTime(t.CreatedAt),
			c.d.encodeNullTime(t.RevokedAt))
	}
	for _, b := range cp.Bindings {
		add(fmt.Sprintf("bindings id %d", b.ID), "bindings", bindingCols, b.ID, b.Principal, b.Role, b.RepoMask, c.d.EncodeTime(b.CreatedAt))
	}
	for _, k := range cp.Keys {
		add("keys "+k.Kind+" "+k.Fingerprint, "keys", keyCols, k.ID, idArg(k.RepoID), k.Kind, blobArg(k.Public), blobArg(k.PrivateEnc),
			k.Fingerprint, c.d.EncodeTime(k.CreatedAt), c.d.encodeNullTime(k.RetiredAt))
	}
	for _, e := range cp.Audit {
		add(fmt.Sprintf("audit id %d", e.ID), "audit", auditCols, e.ID, c.d.EncodeTime(e.At), e.Principal, textArg(e.PrincipalName),
			e.Action, e.Repo, e.Subject, textArg(string(e.Digest)), e.IP, rawArg(e.Details), cmp.Or(e.State, index.AuditOK),
			textArg(e.Error))
	}
	return rows
}
