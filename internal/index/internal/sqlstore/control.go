package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Control plane: repos, users, tokens, bindings, keys, sessions, audit.

const repoCols = `id, format, name, registry, prefix, kind, anonymous_read, policy, upstream, "group", created_at, updated_at, revision`

func repoDests(r *index.Repo) []any {
	return []any{&r.ID, &r.Format, &r.Name, &r.Registry, &r.Prefix, &r.Kind, &r.AnonymousRead,
		rawDest{&r.Policy}, rawDest{&r.Upstream}, rawDest{&r.Group}, timeDest{&r.CreatedAt}, timeDest{&r.UpdatedAt}, &r.Revision}
}

// GetRepo returns the repo with the given format and name.
func (c *conn) GetRepo(ctx context.Context, format, name string) (*index.Repo, error) {
	return getOne(ctx, c, "repos "+format+"/"+name, repoDests,
		"SELECT "+repoCols+" FROM repos WHERE format = ? AND name = ?", format, name)
}

// GetRepoByID returns the repo with the given id.
func (c *conn) GetRepoByID(ctx context.Context, id int64) (*index.Repo, error) {
	return getOne(ctx, c, fmt.Sprintf("repos id %d", id), repoDests,
		"SELECT "+repoCols+" FROM repos WHERE id = ?", id)
}

// ListRepos returns the repos of format, or of every format when format is "".
func (c *conn) ListRepos(ctx context.Context, format string) ([]index.Repo, error) {
	if format == "" {
		return getAll(ctx, c, "repos", repoDests, "SELECT "+repoCols+" FROM repos ORDER BY format, name")
	}
	return getAll(ctx, c, "repos of "+format, repoDests,
		"SELECT "+repoCols+" FROM repos WHERE format = ? ORDER BY name", format)
}

// CreateRepo inserts r, at revision 1, and increments auth_version. UpdatedAt defaults to
// CreatedAt.
func (c *conn) CreateRepo(ctx context.Context, r *index.Repo) error {
	created := stamp(&r.CreatedAt, now())
	updated := stamp(&r.UpdatedAt, created)
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.scanRow(ctx, "repos "+r.Format+"/"+r.Name,
			`INSERT INTO repos (format, name, registry, prefix, kind, anonymous_read, policy, upstream, "group", created_at, updated_at)`+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, revision",
			[]any{r.Format, r.Name, r.Registry, r.Prefix, r.Kind, r.AnonymousRead, rawArg(r.Policy), rawArg(r.Upstream),
				rawArg(r.Group), c.d.EncodeTime(created), c.d.EncodeTime(updated)},
			&r.ID, &r.Revision)
	})
}

// UpdateRepo stores r by r.ID, sets UpdatedAt, moves the revision by one, fills Format, Name,
// Kind and CreatedAt from the row (they do not change) and Revision, and increments
// auth_version.
func (c *conn) UpdateRepo(ctx context.Context, r *index.Repo) error {
	r.UpdatedAt = now()
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.scanRow(ctx, fmt.Sprintf("repos id %d", r.ID),
			`UPDATE repos SET registry = ?, prefix = ?, anonymous_read = ?, policy = ?, upstream = ?, "group" = ?, updated_at = ?,`+
				" revision = revision + 1 WHERE id = ? RETURNING format, name, kind, created_at, revision",
			[]any{r.Registry, r.Prefix, r.AnonymousRead, rawArg(r.Policy), rawArg(r.Upstream), rawArg(r.Group),
				c.d.EncodeTime(r.UpdatedAt), r.ID},
			&r.Format, &r.Name, &r.Kind, timeDest{&r.CreatedAt}, &r.Revision)
	})
}

// ConvertRepo makes the proxy repo id a hosted one: kind hosted, upstream NULL, updated_at now,
// the revision moved by one, and increments auth_version, in one transaction that first refuses a repo of another kind and
// one with a fetch claim that beat at or after staleBefore (ErrConflict). The UPDATE names the
// kind again, so that a concurrent conversion changes the row once.
func (c *conn) ConvertRepo(ctx context.Context, id int64, staleBefore time.Time) error {
	what := fmt.Sprintf("repos id %d", id)
	staleBefore = Normalize(staleBefore)
	return c.withAuthChange(ctx, func(c *conn) error {
		r, err := c.GetRepoByID(ctx, id)
		if err != nil {
			return err
		}
		if r.Kind != index.KindProxy {
			return fmt.Errorf("%w: %s/%s is a %s repo, not a proxy", index.ErrConflict, r.Format, r.Name, r.Kind)
		}
		var live int
		if err := c.scanRow(ctx, what, "SELECT COUNT(*) FROM fetch_claims WHERE repo_id = ? AND heartbeat_at >= ?",
			[]any{id, c.d.EncodeTime(staleBefore)}, &live); err != nil {
			return err
		}
		if live > 0 {
			return fmt.Errorf("%w: %s/%s: %d fetch(es) from its upstream running", index.ErrConflict, r.Format, r.Name, live)
		}
		n, err := c.exec(ctx, what, "UPDATE repos SET kind = ?, upstream = NULL, updated_at = ?, revision = revision + 1 WHERE id = ? AND kind = ?",
			index.KindHosted, c.d.EncodeTime(now()), id, index.KindProxy)
		if err == nil && n == 0 {
			err = fmt.Errorf("%w: %s/%s was converted meanwhile", index.ErrConflict, r.Format, r.Name)
		}
		return err
	})
}

// DeleteRepo removes the repo, the foreign keys cascading to every row that refers to it, and
// increments auth_version. A member of a group is ErrConflict: it is looked up first, and
// group_members.member_id restricts the delete of one that a concurrent change made a member.
func (c *conn) DeleteRepo(ctx context.Context, id int64) error {
	what := fmt.Sprintf("repos id %d", id)
	return c.withAuthChange(ctx, func(c *conn) error {
		groups, err := c.ListGroupsOf(ctx, id)
		if err != nil {
			return err
		}
		if len(groups) > 0 {
			return fmt.Errorf("%w: %s is a member of %d group(s)", index.ErrConflict, what, len(groups))
		}
		return c.deleteRepoRow(ctx, what, id)
	})
}

// deleteRepoRow deletes the repos row of id: a violation of a foreign key is the restriction of
// group_members, ErrConflict, not a missing row.
func (c *conn) deleteRepoRow(ctx context.Context, what string, id int64) (err error) {
	defer observe(time.Now(), &err)
	res, err := c.q.ExecContext(ctx, c.d.Rebind("DELETE FROM repos WHERE id = ?"), id)
	switch {
	case err != nil && c.d.IsForeignKey(err):
		return fmt.Errorf("%w: %s is a member of a group", index.ErrConflict, what)
	case err != nil:
		return c.fail(err, what)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return c.fail(err, what)
		}
		return notFound(what)
	}
	return nil
}

// ListGroupMembers returns the members of a group in member order.
func (c *conn) ListGroupMembers(ctx context.Context, groupID int64) ([]index.Repo, error) {
	return getAll(ctx, c, fmt.Sprintf("group_members of %d", groupID), repoDests,
		"SELECT "+prefixed("r.", repoCols)+" FROM group_members m JOIN repos r ON r.id = m.member_id"+
			" WHERE m.group_id = ? ORDER BY m.position", groupID)
}

// ListGroupsOf returns the groups that have the repo as a member, by format and name.
func (c *conn) ListGroupsOf(ctx context.Context, memberID int64) ([]index.Repo, error) {
	return getAll(ctx, c, fmt.Sprintf("groups of %d", memberID), repoDests,
		"SELECT "+prefixed("r.", repoCols)+" FROM group_members m JOIN repos r ON r.id = m.group_id"+
			" WHERE m.member_id = ? ORDER BY r.format, r.name", memberID)
}

// SetGroupMembers replaces the group_members rows of a group with memberIDs, in order, and
// invalidates the group's index_cache, which an apt group renders from its members
// (docs/spec/proxy-and-group.md §8.6). The group is a member of no group, so the invalidation is
// its own alone.
func (c *conn) SetGroupMembers(ctx context.Context, groupID int64, memberIDs []int64) error {
	what := fmt.Sprintf("group_members of %d", groupID)
	return c.withTx(ctx, func(c *conn) error {
		if err := c.InvalidateIndexCache(ctx, groupID, ""); err != nil {
			return err
		}
		if _, err := c.exec(ctx, what, "DELETE FROM group_members WHERE group_id = ?", groupID); err != nil {
			return err
		}
		for i, id := range memberIDs {
			if _, err := c.exec(ctx, fmt.Sprintf("%s: member %d", what, id),
				"INSERT INTO group_members (group_id, member_id, position) VALUES (?, ?, ?)", groupID, id, i); err != nil {
				return err
			}
		}
		return nil
	})
}

// prefixed returns the comma-separated columns cols, each with the table prefix p.
func prefixed(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ", ")
}

// userCols are the columns of users that a snapshot carries, the history of the password and
// its change at first use among them (docs/spec/index-schema.md, A password's history), and
// userDests their destinations; userReadCols and userReadDests add those of the lockout, which a
// snapshot leaves out (docs/spec/index-schema.md, The lockout).
const (
	userCols = "id, uid, name, password_hash, kind, disabled, created_at, deleted_at, deleted_name, owner, revision, last_seen_at," +
		" must_change, password_history"
	userReadCols = userCols + ", password_failures, locked_until"
)

func userDests(u *index.User) []any {
	return []any{&u.ID, textDest{&u.UID}, &u.Name, textDest{&u.PasswordHash}, &u.Kind, &u.Disabled, timeDest{&u.CreatedAt},
		nullTimeDest{&u.DeletedAt}, textDest{&u.DeletedName}, textDest{&u.Owner}, &u.Revision, nullTimeDest{&u.LastSeenAt},
		&u.MustChange, jsonDest[[]string]{&u.PasswordHistory}}
}

func userReadDests(u *index.User) []any {
	return append(userDests(u), &u.PasswordFailures, nullTimeDest{&u.LockedUntil})
}

// GetUser returns the live user with the given name.
func (c *conn) GetUser(ctx context.Context, name string) (*index.User, error) {
	return getOne(ctx, c, "users "+name, userReadDests,
		"SELECT "+userReadCols+" FROM users WHERE name = ? AND deleted_at IS NULL", name)
}

// GetUserByUID returns the user with the given uid, live or deleted. A string that is not of
// the form of a uid is no user's.
func (c *conn) GetUserByUID(ctx context.Context, uid string) (*index.User, error) {
	if !index.IsUID(uid) {
		return nil, fmt.Errorf("%w: users uid %q", index.ErrNotFound, uid)
	}
	return getOne(ctx, c, "users uid "+uid, userReadDests, "SELECT "+userReadCols+" FROM users WHERE uid = ?", uid)
}

// GetUserByID returns the user with the given id, live or deleted.
func (c *conn) GetUserByID(ctx context.Context, id int64) (*index.User, error) {
	return getOne(ctx, c, fmt.Sprintf("users id %d", id), userReadDests, "SELECT "+userReadCols+" FROM users WHERE id = ?", id)
}

// ListUsers returns the live users ordered by name.
func (c *conn) ListUsers(ctx context.Context) ([]index.User, error) {
	return getAll(ctx, c, "users", userReadDests, "SELECT "+userReadCols+" FROM users WHERE deleted_at IS NULL ORDER BY name")
}

// CreateUser inserts u, at revision 1, with a new uid when UID is empty, and its MustChange and
// PasswordHistory with its password.
func (c *conn) CreateUser(ctx context.Context, u *index.User) error {
	stamp(&u.CreatedAt, now())
	if u.UID == "" {
		u.UID = index.NewUID(u.CreatedAt)
	}
	return c.scanRow(ctx, "users "+u.Name,
		"INSERT INTO users (uid, name, password_hash, kind, disabled, created_at, deleted_at, deleted_name, owner, must_change,"+
			" password_history) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, revision",
		[]any{u.UID, u.Name, textArg(u.PasswordHash), u.Kind, u.Disabled, c.d.EncodeTime(u.CreatedAt),
			c.d.encodeNullTime(stampNull(u.DeletedAt)), textArg(u.DeletedName), textArg(u.Owner), u.MustChange,
			historyArg(u.PasswordHistory)},
		&u.ID, &u.Revision)
}

// UpdateUser stores u by u.ID, its password, MustChange and PasswordHistory in the one statement
// (docs/spec/index-schema.md, A password's history), moves the revision by one, and fills UID,
// CreatedAt and Revision from the row.
func (c *conn) UpdateUser(ctx context.Context, u *index.User) error {
	return c.scanRow(ctx, fmt.Sprintf("users id %d", u.ID),
		"UPDATE users SET name = ?, password_hash = ?, kind = ?, disabled = ?, deleted_at = ?, deleted_name = ?, owner = ?,"+
			" must_change = ?, password_history = ?, revision = revision + 1 WHERE id = ? RETURNING uid, created_at, revision",
		[]any{u.Name, textArg(u.PasswordHash), u.Kind, u.Disabled, c.d.encodeNullTime(stampNull(u.DeletedAt)), textArg(u.DeletedName),
			textArg(u.Owner), u.MustChange, historyArg(u.PasswordHistory), u.ID},
		textDest{&u.UID}, timeDest{&u.CreatedAt}, &u.Revision)
}

// TouchUser sets LastSeenAt of the user to seenAt when it is NULL or earlier, and changes
// nothing otherwise: it only moves forward, as TouchToken does, so that of replicas that record
// an authentication at once, or a late one, none moves it back (docs/spec/rbac.md §1, Inactive
// users). It moves neither the revision nor auth_version: the record of an activity is no change
// of the user. A zero seenAt means now. Only when nothing changed does it read the row, to tell a
// later activity from no user.
func (c *conn) TouchUser(ctx context.Context, id int64, seenAt time.Time) error {
	what := fmt.Sprintf("users id %d", id)
	at := c.d.EncodeTime(stamp(&seenAt, now()))
	n, err := c.exec(ctx, what,
		"UPDATE users SET last_seen_at = ? WHERE id = ? AND (last_seen_at IS NULL OR last_seen_at < ?)", at, id, at)
	if err != nil || n > 0 {
		return err
	}
	var one int
	return c.scanRow(ctx, what, "SELECT 1 FROM users WHERE id = ?", []any{id}, &one)
}

// FailPassword counts a wrong password of the user at at (docs/spec/rbac.md §1, Local passwords;
// docs/spec/index-schema.md, The lockout), in one UPDATE: unless locked_until is later than at,
// it adds one to password_failures, and the failure that reaches limit sets locked_until to
// lockUntil and the count back to 0, in the same statement, and reports true. Every statement
// reads the row as the one before it left it, so that of replicas that count at once exactly one
// locks. A user whose lock is in force changes nothing and reports false; only then is the row
// read, to tell it from no user. Neither the revision nor auth_version moves.
func (c *conn) FailPassword(ctx context.Context, id int64, at, lockUntil time.Time, limit int) (bool, error) {
	what := fmt.Sprintf("users id %d", id)
	failedAt := c.d.EncodeTime(stamp(&at, now()))
	var count int
	err := c.scanRow(ctx, what, "UPDATE users SET"+
		" password_failures = CASE WHEN password_failures + 1 >= ? THEN 0 ELSE password_failures + 1 END,"+
		" locked_until = CASE WHEN password_failures + 1 >= ? THEN ? ELSE locked_until END"+
		" WHERE id = ? AND (locked_until IS NULL OR locked_until <= ?) RETURNING password_failures",
		[]any{limit, limit, c.d.EncodeTime(Normalize(lockUntil)), id, failedAt}, &count)
	if !errors.Is(err, index.ErrNotFound) {
		return err == nil && count == 0, err
	}
	var one int
	return false, c.scanRow(ctx, what, "SELECT 1 FROM users WHERE id = ?", []any{id}, &one)
}

// ResetPasswordFailures sets password_failures of the user back to 0 while no lock is in force at
// at; a lock in force changes nothing and wraps index.ErrConflict, so that a password verified
// while another replica locked the user is not taken. A count that is 0 already is not written:
// the row is read instead, as it is to tell a lock from no user.
func (c *conn) ResetPasswordFailures(ctx context.Context, id int64, at time.Time) error {
	what := fmt.Sprintf("users id %d", id)
	n, err := c.exec(ctx, what, "UPDATE users SET password_failures = 0"+
		" WHERE id = ? AND password_failures <> 0 AND (locked_until IS NULL OR locked_until <= ?)", id, c.d.EncodeTime(stamp(&at, now())))
	if err != nil || n > 0 {
		return err
	}
	var until *time.Time
	if err := c.scanRow(ctx, what, "SELECT locked_until FROM users WHERE id = ?", []any{id}, nullTimeDest{&until}); err != nil {
		return err
	}
	if until != nil && until.After(at) {
		return fmt.Errorf("%w: %s is locked until %s", index.ErrConflict, what, until.UTC().Format(time.RFC3339))
	}
	return nil
}

// UnlockUser sets password_failures of the user to 0 and locked_until to NULL, and returns the
// locked_until it ended when that was later than at, nil otherwise. In one transaction whose first
// statement writes the row, so that the row is held from there: of two unlocks at once, one ends
// the lock, and a lock set meanwhile is not cleared unseen.
func (c *conn) UnlockUser(ctx context.Context, id int64, at time.Time) (*time.Time, error) {
	what := fmt.Sprintf("users id %d", id)
	stamp(&at, now())
	var ended *time.Time
	err := c.withTx(ctx, func(c *conn) error {
		ended = nil
		var until *time.Time
		if err := c.scanRow(ctx, what, "UPDATE users SET password_failures = 0 WHERE id = ? RETURNING locked_until", []any{id},
			nullTimeDest{&until}); err != nil {
			return err
		}
		if until == nil {
			return nil
		}
		if until.After(at) {
			ended = until
		}
		_, err := c.exec(ctx, what, "UPDATE users SET locked_until = NULL WHERE id = ?", id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ended, nil
}

// tokCols, not tokenCols: gosec (G101) takes a long string named token… for a credential.
const tokCols = "id, kind, user_id, repo_id, repo_mask, name, hash, scopes, expires_at, last_used_at, created_at, revoked_at, dpop_jkt," +
	" auth_time, amr, acr"

func tokenDests(t *index.Token) []any {
	return []any{&t.ID, &t.Kind, nullIDDest{&t.UserID}, nullIDDest{&t.RepoID}, textDest{&t.RepoMask}, &t.Name, &t.Hash,
		jsonDest[[]string]{&t.Scopes}, nullTimeDest{&t.ExpiresAt}, nullTimeDest{&t.LastUsedAt},
		timeDest{&t.CreatedAt}, nullTimeDest{&t.RevokedAt}, textDest{&t.DPoPJKT}, nullTimeDest{&t.AuthTime},
		jsonDest[[]string]{&t.AMR}, textDest{&t.ACR}}
}

// GetTokenByHash returns the token with the hash, revoked and expired ones included.
func (c *conn) GetTokenByHash(ctx context.Context, hash string) (*index.Token, error) {
	return getOne(ctx, c, "tokens with the hash", tokenDests, "SELECT "+tokCols+" FROM tokens WHERE hash = ?", hash)
}

// ListTokens returns the tokens of userID and repoID, ordered by id; nil matches every token, so
// that both nil returns the deploy tokens over a mask too.
func (c *conn) ListTokens(ctx context.Context, userID, repoID *int64) ([]index.Token, error) {
	var w where
	if userID != nil {
		w.add("user_id = ?", *userID)
	}
	if repoID != nil {
		w.add("repo_id = ?", *repoID)
	}
	return getAll(ctx, c, "tokens", tokenDests, "SELECT "+tokCols+" FROM tokens"+w.clause()+" ORDER BY id", w.args...)
}

// ListTokenPage returns the tokens that f keeps whose id is above f.After, by id, at most
// f.Limit, on the primary key.
func (c *conn) ListTokenPage(ctx context.Context, f index.TokenFilter) ([]index.Token, error) {
	if f.Limit < 1 {
		return []index.Token{}, nil
	}
	var w where
	w.add("id > ?", f.After)
	if f.Kind != "" {
		w.add("kind = ?", f.Kind)
	}
	if f.UserID != nil {
		w.add("user_id = ?", *f.UserID)
	}
	if f.RepoID != nil {
		w.add("repo_id = ?", *f.RepoID)
	}
	if f.RepoMask != "" {
		w.add("repo_mask = ?", f.RepoMask)
	}
	if !f.LiveAt.IsZero() {
		w.add("revoked_at IS NULL AND (expires_at IS NULL OR expires_at >= ?)", c.d.EncodeTime(f.LiveAt))
	}
	return getAll(ctx, c, "a page of tokens", tokenDests, "SELECT "+tokCols+" FROM tokens"+w.clause()+" ORDER BY id LIMIT ?",
		append(w.args, f.Limit)...)
}

// CreateToken inserts t.
func (c *conn) CreateToken(ctx context.Context, t *index.Token) error {
	t.ExpiresAt, t.LastUsedAt, t.RevokedAt = stampNull(t.ExpiresAt), stampNull(t.LastUsedAt), stampNull(t.RevokedAt)
	t.AuthTime = stampNull(t.AuthTime)
	return c.scanRow(ctx, fmt.Sprintf("tokens %s %q", t.Kind, t.Name),
		"INSERT INTO tokens (kind, user_id, repo_id, repo_mask, name, hash, scopes, expires_at, last_used_at, created_at, revoked_at,"+
			" dpop_jkt, auth_time, amr, acr) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{t.Kind, idArg(t.UserID), idArg(t.RepoID), textArg(t.RepoMask), t.Name, t.Hash, scopesArg(t.Scopes),
			c.d.encodeNullTime(t.ExpiresAt), c.d.encodeNullTime(t.LastUsedAt),
			c.d.EncodeTime(stamp(&t.CreatedAt, now())), c.d.encodeNullTime(t.RevokedAt), textArg(t.DPoPJKT),
			c.d.encodeNullTime(t.AuthTime), scopesArg(t.AMR), textArg(t.ACR)},
		&t.ID)
}

// RevokeToken sets RevokedAt of the token while it is NULL; a zero at means now. A token revoked
// already keeps its time and wraps index.ErrConflict, so that of callers that revoke it at once,
// one does and writes its audit row (docs/spec/rbac.md §10, #1277). Only when nothing changed
// does it read the row, to tell a revoked token from no token.
func (c *conn) RevokeToken(ctx context.Context, id int64, at time.Time) error {
	what := fmt.Sprintf("tokens id %d", id)
	n, err := c.exec(ctx, what,
		"UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", c.d.EncodeTime(stamp(&at, now())), id)
	if err != nil || n > 0 {
		return err
	}
	var one int
	if err := c.scanRow(ctx, what, "SELECT 1 FROM tokens WHERE id = ?", []any{id}, &one); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is revoked already", index.ErrConflict, what)
}

// TouchToken sets LastUsedAt of the token to lastUsed when it is NULL or earlier, and changes
// nothing otherwise: it only moves forward, so that of replicas that record a use at once, or a
// late one, none moves it back (docs/spec/rbac.md §10, #1277). A zero lastUsed means now. Only
// when nothing changed does it read the row, to tell a later last use from no token.
func (c *conn) TouchToken(ctx context.Context, id int64, lastUsed time.Time) error {
	what := fmt.Sprintf("tokens id %d", id)
	at := c.d.EncodeTime(stamp(&lastUsed, now()))
	n, err := c.exec(ctx, what,
		"UPDATE tokens SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)", at, id, at)
	if err != nil || n > 0 {
		return err
	}
	var one int
	return c.scanRow(ctx, what, "SELECT 1 FROM tokens WHERE id = ?", []any{id}, &one)
}

const bindingCols = "id, principal, role, repo_mask, created_at"

func bindingDests(b *index.Binding) []any {
	return []any{&b.ID, &b.Principal, &b.Role, &b.RepoMask, timeDest{&b.CreatedAt}}
}

// ListBindings returns the bindings of principal, or all when principal is "", ordered by id.
func (c *conn) ListBindings(ctx context.Context, principal string) ([]index.Binding, error) {
	var w where
	if principal != "" {
		w.add("principal = ?", principal)
	}
	return getAll(ctx, c, "bindings", bindingDests, "SELECT "+bindingCols+" FROM bindings"+w.clause()+" ORDER BY id", w.args...)
}

// CreateBinding inserts b and increments auth_version.
func (c *conn) CreateBinding(ctx context.Context, b *index.Binding) error {
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.scanRow(ctx, "bindings "+b.Principal,
			"INSERT INTO bindings (principal, role, repo_mask, created_at) VALUES (?, ?, ?, ?) RETURNING id",
			[]any{b.Principal, b.Role, b.RepoMask, c.d.EncodeTime(stamp(&b.CreatedAt, now()))},
			&b.ID)
	})
}

// DeleteBinding removes the binding and increments auth_version.
func (c *conn) DeleteBinding(ctx context.Context, id int64) error {
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.execOne(ctx, fmt.Sprintf("bindings id %d", id), "DELETE FROM bindings WHERE id = ?", id)
	})
}

// AuthVersion returns the counter of auth_version.
func (c *conn) AuthVersion(ctx context.Context) (int64, error) {
	var v int64
	err := c.scanRow(ctx, "auth_version", "SELECT version FROM auth_version WHERE id = 1", nil, &v)
	return v, err
}

// withAuthChange runs fn, a write of bindings or repos, and increments auth_version in the same
// transaction (docs/spec/rbac.md §3). A write that fails leaves the counter as it was.
func (c *conn) withAuthChange(ctx context.Context, fn func(*conn) error) error {
	return c.withTx(ctx, func(c *conn) error {
		if err := fn(c); err != nil {
			return err
		}
		return c.execOne(ctx, "auth_version",
			"UPDATE auth_version SET version = version + 1, updated_at = ? WHERE id = 1", c.d.EncodeTime(now()))
	})
}

const keyCols = "id, repo_id, kind, public, private_enc, fingerprint, created_at, retired_at"

func keyDests(k *index.Key) []any {
	return []any{&k.ID, nullIDDest{&k.RepoID}, &k.Kind, blobDest{&k.Public}, blobDest{&k.PrivateEnc},
		&k.Fingerprint, timeDest{&k.CreatedAt}, nullTimeDest{&k.RetiredAt}}
}

// keyOwner adds the condition on repo_id: a repo's keys, or the server's when repoID is nil.
func keyOwner(w *where, repoID *int64) string {
	if repoID == nil {
		w.add("repo_id IS NULL")
		return "server"
	}
	w.add("repo_id = ?", *repoID)
	return fmt.Sprintf("repo %d", *repoID)
}

// GetKey returns the newest key of kind that is not retired.
func (c *conn) GetKey(ctx context.Context, repoID *int64, kind string) (*index.Key, error) {
	var w where
	owner := keyOwner(&w, repoID)
	w.add("kind = ?", kind)
	w.add("retired_at IS NULL")
	return getOne(ctx, c, "keys "+kind+" of "+owner, keyDests,
		"SELECT "+keyCols+" FROM keys"+w.clause()+" ORDER BY created_at DESC, id DESC LIMIT 1", w.args...)
}

// ListKeys returns the keys of a repo, or the server's when repoID is nil, ordered by id.
func (c *conn) ListKeys(ctx context.Context, repoID *int64) ([]index.Key, error) {
	var w where
	owner := keyOwner(&w, repoID)
	return getAll(ctx, c, "keys of "+owner, keyDests, "SELECT "+keyCols+" FROM keys"+w.clause()+" ORDER BY id", w.args...)
}

// CreateKey inserts k.
func (c *conn) CreateKey(ctx context.Context, k *index.Key) error {
	k.RetiredAt = stampNull(k.RetiredAt)
	return c.scanRow(ctx, "keys "+k.Kind+" "+k.Fingerprint,
		"INSERT INTO keys (repo_id, kind, public, private_enc, fingerprint, created_at, retired_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{idArg(k.RepoID), k.Kind, blobArg(k.Public), blobArg(k.PrivateEnc), k.Fingerprint,
			c.d.EncodeTime(stamp(&k.CreatedAt, now())), c.d.encodeNullTime(k.RetiredAt)},
		&k.ID)
}

// RetireKey sets RetiredAt of the key; a zero at means now.
func (c *conn) RetireKey(ctx context.Context, id int64, at time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("keys id %d", id),
		"UPDATE keys SET retired_at = ? WHERE id = ?", c.d.EncodeTime(stamp(&at, now())), id)
}

const sessionCols = "id, repo_id, key, principal, state, files, created_at, updated_at, expires_at"

func sessionDests(s *index.Session) []any {
	return []any{&s.ID, &s.RepoID, &s.Key, &s.Principal, &s.State, rawDest{&s.Files},
		timeDest{&s.CreatedAt}, timeDest{&s.UpdatedAt}, timeDest{&s.ExpiresAt}}
}

// GetSession returns the newest open session with key in a repo.
func (c *conn) GetSession(ctx context.Context, repoID int64, key string) (*index.Session, error) {
	return getOne(ctx, c, fmt.Sprintf("open sessions %q in repo %d", key, repoID), sessionDests,
		"SELECT "+sessionCols+" FROM sessions WHERE repo_id = ? AND key = ? AND state = 'open' ORDER BY id DESC LIMIT 1",
		repoID, key)
}

// GetSessionByID returns the session with the given id, in any state.
func (c *conn) GetSessionByID(ctx context.Context, id int64) (*index.Session, error) {
	return getOne(ctx, c, fmt.Sprintf("sessions id %d", id), sessionDests,
		"SELECT "+sessionCols+" FROM sessions WHERE id = ?", id)
}

// ListOpenSessions returns the open sessions of a repo that expire after at, but the blob
// uploads of the facade, whose id is above after, by id, at most limit, on the partial index
// sessions_open_key.
func (c *conn) ListOpenSessions(ctx context.Context, repoID int64, at time.Time, after int64, limit int) ([]index.Session, error) {
	if limit < 1 {
		return []index.Session{}, nil
	}
	return getAll(ctx, c, fmt.Sprintf("open sessions of repo %d", repoID), sessionDests,
		"SELECT "+sessionCols+" FROM sessions WHERE repo_id = ? AND state = 'open' AND expires_at > ? AND id > ?"+
			" AND key NOT LIKE 'blob-upload:%' ORDER BY id LIMIT ?",
		repoID, c.d.EncodeTime(Normalize(at)), after, limit)
}

// UpsertSession inserts s when s.ID is 0, and otherwise stores State, Files and ExpiresAt of
// row s.ID and fills the other fields from it. It sets UpdatedAt to now.
func (c *conn) UpsertSession(ctx context.Context, s *index.Session) error {
	s.UpdatedAt = now()
	s.ExpiresAt = Normalize(s.ExpiresAt)
	if s.ID == 0 {
		return c.scanRow(ctx, fmt.Sprintf("sessions %q in repo %d", s.Key, s.RepoID),
			"INSERT INTO sessions (repo_id, key, principal, state, files, created_at, updated_at, expires_at)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
			[]any{s.RepoID, s.Key, s.Principal, s.State, rawArg(s.Files),
				c.d.EncodeTime(stamp(&s.CreatedAt, s.UpdatedAt)), c.d.EncodeTime(s.UpdatedAt), c.d.EncodeTime(s.ExpiresAt)},
			&s.ID)
	}
	return c.scanRow(ctx, fmt.Sprintf("sessions id %d", s.ID),
		"UPDATE sessions SET state = ?, files = ?, expires_at = ?, updated_at = ? WHERE id = ?"+
			" RETURNING repo_id, key, principal, created_at",
		[]any{s.State, rawArg(s.Files), c.d.EncodeTime(s.ExpiresAt), c.d.EncodeTime(s.UpdatedAt), s.ID},
		&s.RepoID, &s.Key, &s.Principal, timeDest{&s.CreatedAt})
}

// TouchSession moves expires_at of an open session that has not expired at at, and returns the
// row. The UPDATE comes first so that it takes the row's lock on Postgres, held until the
// transaction ends; SQLite has one writer.
func (c *conn) TouchSession(ctx context.Context, id int64, at, expiresAt time.Time) (*index.Session, error) {
	var s index.Session
	err := c.scanRow(ctx, fmt.Sprintf("open sessions id %d", id),
		"UPDATE sessions SET expires_at = ?, updated_at = ? WHERE id = ? AND state = 'open' AND expires_at > ?"+
			" RETURNING "+sessionCols,
		[]any{c.d.EncodeTime(Normalize(expiresAt)), c.d.EncodeTime(now()), id, c.d.EncodeTime(Normalize(at))},
		sessionDests(&s)...)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteSession removes the session.
func (c *conn) DeleteSession(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("sessions id %d", id), "DELETE FROM sessions WHERE id = ?", id)
}

// ExpireSessions sets open sessions that expire before before to expired.
func (c *conn) ExpireSessions(ctx context.Context, before time.Time) (int, error) {
	return c.exec(ctx, "sessions",
		"UPDATE sessions SET state = 'expired', updated_at = ? WHERE state = 'open' AND expires_at < ?",
		c.d.EncodeTime(now()), c.d.EncodeTime(before))
}

const auditCols = "id, at, principal, principal_name, action, repo, subject, digest, ip, details, state, error"

func auditDests(ev *index.AuditEvent) []any {
	return []any{&ev.ID, timeDest{&ev.At}, &ev.Principal, textDest{&ev.PrincipalName}, &ev.Action, &ev.Repo, &ev.Subject,
		textDest{(*string)(&ev.Digest)}, &ev.IP, rawDest{&ev.Details}, &ev.State, textDest{&ev.Error}}
}

// defaultAuditLimit is the page size of ListAudit when the filter has none.
const defaultAuditLimit = 100

// ListAudit returns the events matching f, newest first, on audit_at; with f.Feed those whose
// id is above f.After, by id, on the primary key. Since is inclusive, Until exclusive.
func (c *conn) ListAudit(ctx context.Context, f index.AuditFilter) ([]index.AuditEvent, error) {
	var w where
	order := " ORDER BY at DESC, id DESC"
	if f.Feed {
		w.add("id > ?", f.After)
		order = " ORDER BY id"
	}
	for _, eq := range []struct{ col, v string }{{"principal", f.Principal}, {"repo", f.Repo}, {"action", f.Action}, {"state", f.State}} {
		if eq.v != "" {
			w.add(eq.col+" = ?", eq.v)
		}
	}
	if !f.Since.IsZero() {
		w.add("at >= ?", c.d.EncodeTime(f.Since))
	}
	if !f.Until.IsZero() {
		w.add("at < ?", c.d.EncodeTime(f.Until))
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	args := append(w.args, limit, max(f.Offset, 0))
	return getAll(ctx, c, "audit", auditDests,
		"SELECT "+auditCols+" FROM audit"+w.clause()+order+" LIMIT ? OFFSET ?", args...)
}

// AppendAudit inserts ev; At is set when zero, State to ok when empty. The row as inserted goes
// to the observer (wroteAudit).
func (c *conn) AppendAudit(ctx context.Context, ev *index.AuditEvent) error {
	var uid any // the uid of a user principal, whose name the row gets when it has none
	if rest, ok := strings.CutPrefix(ev.Principal, "user:"); ok && index.IsUID(rest) {
		uid = rest
	}
	if ev.State == "" {
		ev.State = index.AuditOK
	}
	ev.Error = auditError(ev.Error)
	var row index.AuditEvent
	err := c.scanRow(ctx, "audit "+ev.Action,
		"INSERT INTO audit (at, principal, principal_name, action, repo, subject, digest, ip, details, state, error)"+
			" VALUES (?, ?, COALESCE(?, (SELECT COALESCE(deleted_name, name) FROM users WHERE CAST(uid AS TEXT) = ?)), ?, ?, ?, ?, ?, ?, ?, ?)"+
			" RETURNING "+auditCols,
		[]any{c.d.EncodeTime(stamp(&ev.At, now())), ev.Principal, textArg(ev.PrincipalName), uid, ev.Action, ev.Repo, ev.Subject,
			textArg(string(ev.Digest)), ev.IP, rawArg(ev.Details), ev.State, textArg(ev.Error)},
		auditDests(&row)...)
	if err != nil {
		return err
	}
	ev.ID, ev.PrincipalName = row.ID, row.PrincipalName
	c.wroteAudit(row)
	return nil
}

// CompleteAudit stores the state, the error and the details of a pending or unknown row.
func (c *conn) CompleteAudit(ctx context.Context, ev *index.AuditEvent) error {
	ev.Error = auditError(ev.Error)
	err := c.scanRow(ctx, fmt.Sprintf("pending audit id %d", ev.ID),
		"UPDATE audit SET state = ?, error = ?, details = ? WHERE id = ? AND state IN ('pending', 'unknown') RETURNING "+auditCols,
		[]any{ev.State, textArg(ev.Error), rawArg(ev.Details), ev.ID}, auditDests(ev)...)
	if err != nil {
		return err
	}
	c.wroteAudit(*ev)
	return nil
}

// maxAuditError is how much of an error a row keeps: an error of the registry may quote a
// whole answer.
const maxAuditError = 1024

// auditError is s cut to maxAuditError bytes, without a broken character at the end.
func auditError(s string) string {
	if len(s) <= maxAuditError {
		return s
	}
	return strings.ToValidUTF8(s[:maxAuditError], "")
}

// ExpirePendingAudit sets the pending rows older than before to unknown, and hands them to the
// observer as they are now.
func (c *conn) ExpirePendingAudit(ctx context.Context, before time.Time) (int, error) {
	rows, err := getAll(ctx, c, "audit", auditDests,
		"UPDATE audit SET state = 'unknown' WHERE state = 'pending' AND at < ? RETURNING "+auditCols, c.d.EncodeTime(before))
	if err != nil {
		return 0, err
	}
	c.wroteAudit(rows...)
	return len(rows), nil
}

// prunable is the condition of the audit rows that a prune may delete with the cut-off of its one
// parameter (docs/spec/rbac.md §5, A prune): before it, not pending, and not a push whose subject
// is a digest, a referrer's, which the repair of a subject's referrers index reads (#628). A
// subject is a digest when it starts with an algorithm of go-digest and a colon; a push of a
// version has "<name> <version>".
const prunable = "at < ? AND state <> 'pending' AND NOT (action = 'push' AND" +
	" (subject LIKE 'sha256:%' OR subject LIKE 'sha384:%' OR subject LIKE 'sha512:%'))"

// SpanAudit counts the rows that PruneAudit would delete with before, with the first and the
// last of their ids and times, in one statement on audit_at.
func (c *conn) SpanAudit(ctx context.Context, before time.Time) (index.AuditSpan, error) {
	var (
		s           index.AuditSpan
		first, last *time.Time
	)
	err := c.scanRow(ctx, "audit", "SELECT COUNT(*), COALESCE(MIN(id), 0), COALESCE(MAX(id), 0), MIN(at), MAX(at) FROM audit WHERE "+prunable,
		[]any{c.d.EncodeTime(before)}, &s.Rows, &s.FirstID, &s.LastID, nullTimeDest{&first}, nullTimeDest{&last})
	if err != nil {
		return index.AuditSpan{}, err
	}
	if first != nil && last != nil {
		s.FirstAt, s.LastAt = *first, *last
	}
	return s, nil
}

// PruneAudit deletes at most limit of the rows that SpanAudit counts with before whose id is at
// most upTo, the lowest ids first, in one statement: a transaction of its own on the pool.
func (c *conn) PruneAudit(ctx context.Context, before time.Time, upTo int64, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	return c.exec(ctx, "audit",
		"DELETE FROM audit WHERE id IN (SELECT id FROM audit WHERE "+prunable+" AND id <= ? ORDER BY id LIMIT ?)",
		c.d.EncodeTime(before), upTo, limit)
}
