package sqlstore

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The read of the access report (docs/spec/rbac.md §14, docs/spec/index-schema.md, The access
// report; #1279): who has what, of one moment.

// accessUserCols are the columns of users that the access report reads: those of userReadCols but
// the password's hash and its history, which the report never shows, and the count of wrong
// passwords.
const accessUserCols = "id, uid, name, kind, disabled, created_at, deleted_at, deleted_name, owner, revision, last_seen_at," +
	" must_change, locked_until"

func accessUserDests(u *index.User) []any {
	return []any{&u.ID, textDest{&u.UID}, &u.Name, &u.Kind, &u.Disabled, timeDest{&u.CreatedAt}, nullTimeDest{&u.DeletedAt},
		textDest{&u.DeletedName}, textDest{&u.Owner}, &u.Revision, nullTimeDest{&u.LastSeenAt}, &u.MustChange,
		nullTimeDest{&u.LockedUntil}}
}

// ReadAccess reads the rows of the access report in one transaction that reads one snapshot
// (Dialect.ReadSnapshot, read only on Postgres), as ExportControlPlane does, run again when the
// database aborts it all the same, up to TxAttempts times in all. Inside a transaction it reads
// on that transaction.
func (c *conn) ReadAccess(ctx context.Context) (*index.Access, error) {
	if c.db == nil {
		return c.readAccess(ctx)
	}
	var a *index.Access
	read := func(in *conn) (err error) {
		if c.d.ReadSnapshot != "" {
			if _, err := in.exec(ctx, "the snapshot of the access report", c.d.ReadSnapshot); err != nil {
				return err
			}
		}
		a, err = in.readAccess(ctx)
		return err
	}
	for attempt := 1; ; attempt++ {
		err := c.begin(ctx, nil, read)
		if err == nil {
			return a, nil
		}
		if attempt == TxAttempts || c.d.IsAborted == nil || !c.d.IsAborted(err) || ctx.Err() != nil {
			return nil, err
		}
	}
}

// readAccess reads the rows of the access report on c, one statement per table, at the
// database's time: the live users; the groups of their identities that have not lapsed then;
// every binding; the tokens live then; every CI trust policy of the API; and the repos with
// anonymous_read or a live deploy token, in one statement.
func (c *conn) readAccess(ctx context.Context) (*index.Access, error) {
	t, err := c.DatabaseTime(ctx)
	if err != nil {
		return nil, err
	}
	a := &index.Access{ReadAt: Normalize(t)}
	at := c.d.EncodeTime(a.ReadAt)
	var repos []index.Repo
	reads := []func() error{
		func() (err error) {
			a.Users, err = getAll(ctx, c, "users", accessUserDests,
				"SELECT "+accessUserCols+" FROM users WHERE deleted_at IS NULL ORDER BY name")
			return err
		},
		func() (err error) {
			a.IdentityGroups, err = getAll(ctx, c, "identity_groups", identityGroupDests, "SELECT g.identity_id, i.user_id, u.uid,"+
				" g.name, i.groups_expire_at FROM identity_groups g JOIN identities i ON i.id = g.identity_id"+
				" JOIN users u ON u.id = i.user_id WHERE u.deleted_at IS NULL AND i.groups_expire_at > ? ORDER BY u.uid, g.name", at)
			return err
		},
		func() (err error) {
			a.Bindings, err = getAll(ctx, c, "bindings", bindingDests, "SELECT "+bindingCols+" FROM bindings ORDER BY id")
			return err
		},
		func() (err error) {
			a.Tokens, err = getAll(ctx, c, "tokens", tokenDests, "SELECT "+tokCols+" FROM tokens"+
				" WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at >= ?) ORDER BY id", at)
			return err
		},
		func() (err error) {
			a.CIPolicies, err = c.ListCIPolicies(ctx)
			return err
		},
		func() (err error) {
			repos, err = getAll(ctx, c, "repos", repoDests, "SELECT "+repoCols+" FROM repos WHERE anonymous_read = ? OR id IN"+
				" (SELECT repo_id FROM tokens WHERE repo_id IS NOT NULL AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at >= ?))"+
				" ORDER BY id", true, at)
			return err
		},
	}
	for _, read := range reads {
		if err := read(); err != nil {
			return nil, err
		}
	}
	a.TokenRepos, a.AnonymousRepos = splitAccessRepos(repos, a.Tokens)
	return a, nil
}

// splitAccessRepos returns, of repos by id, those that a deploy token of tokens is bound to, by
// id, and those with anonymous_read, by format and name.
func splitAccessRepos(repos []index.Repo, tokens []index.Token) (bound, anonymous []index.Repo) {
	ids := map[int64]bool{}
	for _, t := range tokens {
		if t.RepoID != nil {
			ids[*t.RepoID] = true
		}
	}
	bound, anonymous = []index.Repo{}, []index.Repo{}
	for _, r := range repos {
		if ids[r.ID] {
			bound = append(bound, r)
		}
		if r.AnonymousRead {
			anonymous = append(anonymous, r)
		}
	}
	slices.SortFunc(anonymous, func(x, y index.Repo) int {
		return cmp.Or(strings.Compare(x.Format, y.Format), strings.Compare(x.Name, y.Name))
	})
	return bound, anonymous
}
