package sqlstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Control plane: repos, users, tokens, bindings, keys, sessions, audit.

const repoCols = "id, format, name, registry, prefix, kind, anonymous_read, policy, upstream, created_at, updated_at"

func repoDests(r *index.Repo) []any {
	return []any{&r.ID, &r.Format, &r.Name, &r.Registry, &r.Prefix, &r.Kind, &r.AnonymousRead,
		rawDest{&r.Policy}, rawDest{&r.Upstream}, timeDest{&r.CreatedAt}, timeDest{&r.UpdatedAt}}
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

// CreateRepo inserts r and increments auth_version. UpdatedAt defaults to CreatedAt.
func (c *conn) CreateRepo(ctx context.Context, r *index.Repo) error {
	created := stamp(&r.CreatedAt, now())
	updated := stamp(&r.UpdatedAt, created)
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.scanRow(ctx, "repos "+r.Format+"/"+r.Name,
			"INSERT INTO repos (format, name, registry, prefix, kind, anonymous_read, policy, upstream, created_at, updated_at)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
			[]any{r.Format, r.Name, r.Registry, r.Prefix, r.Kind, r.AnonymousRead, rawArg(r.Policy), rawArg(r.Upstream),
				c.d.EncodeTime(created), c.d.EncodeTime(updated)},
			&r.ID)
	})
}

// UpdateRepo stores r by r.ID, sets UpdatedAt, fills Format, Name, Kind and CreatedAt from the
// row (they do not change), and increments auth_version.
func (c *conn) UpdateRepo(ctx context.Context, r *index.Repo) error {
	r.UpdatedAt = now()
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.scanRow(ctx, fmt.Sprintf("repos id %d", r.ID),
			"UPDATE repos SET registry = ?, prefix = ?, anonymous_read = ?, policy = ?, upstream = ?, updated_at = ?"+
				" WHERE id = ? RETURNING format, name, kind, created_at",
			[]any{r.Registry, r.Prefix, r.AnonymousRead, rawArg(r.Policy), rawArg(r.Upstream), c.d.EncodeTime(r.UpdatedAt), r.ID},
			&r.Format, &r.Name, &r.Kind, timeDest{&r.CreatedAt})
	})
}

// DeleteRepo removes the repo, the foreign keys cascading to every row that refers to it, and
// increments auth_version.
func (c *conn) DeleteRepo(ctx context.Context, id int64) error {
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.execOne(ctx, fmt.Sprintf("repos id %d", id), "DELETE FROM repos WHERE id = ?", id)
	})
}

const userCols = "id, uid, name, password_hash, kind, disabled, created_at, deleted_at, deleted_name"

func userDests(u *index.User) []any {
	return []any{&u.ID, textDest{&u.UID}, &u.Name, textDest{&u.PasswordHash}, &u.Kind, &u.Disabled, timeDest{&u.CreatedAt},
		nullTimeDest{&u.DeletedAt}, textDest{&u.DeletedName}}
}

// GetUser returns the live user with the given name.
func (c *conn) GetUser(ctx context.Context, name string) (*index.User, error) {
	return getOne(ctx, c, "users "+name, userDests,
		"SELECT "+userCols+" FROM users WHERE name = ? AND deleted_at IS NULL", name)
}

// GetUserByUID returns the user with the given uid, live or deleted. A string that is not of
// the form of a uid is no user's.
func (c *conn) GetUserByUID(ctx context.Context, uid string) (*index.User, error) {
	if !index.IsUID(uid) {
		return nil, fmt.Errorf("%w: users uid %q", index.ErrNotFound, uid)
	}
	return getOne(ctx, c, "users uid "+uid, userDests, "SELECT "+userCols+" FROM users WHERE uid = ?", uid)
}

// GetUserByID returns the user with the given id, live or deleted.
func (c *conn) GetUserByID(ctx context.Context, id int64) (*index.User, error) {
	return getOne(ctx, c, fmt.Sprintf("users id %d", id), userDests, "SELECT "+userCols+" FROM users WHERE id = ?", id)
}

// ListUsers returns the live users ordered by name.
func (c *conn) ListUsers(ctx context.Context) ([]index.User, error) {
	return getAll(ctx, c, "users", userDests, "SELECT "+userCols+" FROM users WHERE deleted_at IS NULL ORDER BY name")
}

// CreateUser inserts u, with a new uid when UID is empty.
func (c *conn) CreateUser(ctx context.Context, u *index.User) error {
	stamp(&u.CreatedAt, now())
	if u.UID == "" {
		u.UID = index.NewUID(u.CreatedAt)
	}
	return c.scanRow(ctx, "users "+u.Name,
		"INSERT INTO users (uid, name, password_hash, kind, disabled, created_at, deleted_at, deleted_name)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{u.UID, u.Name, textArg(u.PasswordHash), u.Kind, u.Disabled, c.d.EncodeTime(u.CreatedAt),
			c.d.encodeNullTime(stampNull(u.DeletedAt)), textArg(u.DeletedName)},
		&u.ID)
}

// UpdateUser stores u by u.ID and fills UID and CreatedAt from the row.
func (c *conn) UpdateUser(ctx context.Context, u *index.User) error {
	return c.scanRow(ctx, fmt.Sprintf("users id %d", u.ID),
		"UPDATE users SET name = ?, password_hash = ?, kind = ?, disabled = ?, deleted_at = ?, deleted_name = ?"+
			" WHERE id = ? RETURNING uid, created_at",
		[]any{u.Name, textArg(u.PasswordHash), u.Kind, u.Disabled, c.d.encodeNullTime(stampNull(u.DeletedAt)), textArg(u.DeletedName), u.ID},
		textDest{&u.UID}, timeDest{&u.CreatedAt})
}

// tokCols, not tokenCols: gosec (G101) takes a long string named token… for a credential.
const tokCols = "id, kind, user_id, repo_id, name, hash, scopes, expires_at, last_used_at, created_at, revoked_at"

func tokenDests(t *index.Token) []any {
	return []any{&t.ID, &t.Kind, nullIDDest{&t.UserID}, nullIDDest{&t.RepoID}, &t.Name, &t.Hash,
		jsonDest[[]string]{&t.Scopes}, nullTimeDest{&t.ExpiresAt}, nullTimeDest{&t.LastUsedAt},
		timeDest{&t.CreatedAt}, nullTimeDest{&t.RevokedAt}}
}

// GetTokenByHash returns the token with the hash, revoked and expired ones included.
func (c *conn) GetTokenByHash(ctx context.Context, hash string) (*index.Token, error) {
	return getOne(ctx, c, "tokens with the hash", tokenDests, "SELECT "+tokCols+" FROM tokens WHERE hash = ?", hash)
}

// ListTokens returns the tokens of userID and repoID, ordered by id; nil matches every token.
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

// CreateToken inserts t.
func (c *conn) CreateToken(ctx context.Context, t *index.Token) error {
	t.ExpiresAt, t.LastUsedAt, t.RevokedAt = stampNull(t.ExpiresAt), stampNull(t.LastUsedAt), stampNull(t.RevokedAt)
	return c.scanRow(ctx, fmt.Sprintf("tokens %s %q", t.Kind, t.Name),
		"INSERT INTO tokens (kind, user_id, repo_id, name, hash, scopes, expires_at, last_used_at, created_at, revoked_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{t.Kind, idArg(t.UserID), idArg(t.RepoID), t.Name, t.Hash, scopesArg(t.Scopes),
			c.d.encodeNullTime(t.ExpiresAt), c.d.encodeNullTime(t.LastUsedAt),
			c.d.EncodeTime(stamp(&t.CreatedAt, now())), c.d.encodeNullTime(t.RevokedAt)},
		&t.ID)
}

// RevokeToken sets RevokedAt of the token; a zero at means now.
func (c *conn) RevokeToken(ctx context.Context, id int64, at time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("tokens id %d", id),
		"UPDATE tokens SET revoked_at = ? WHERE id = ?", c.d.EncodeTime(stamp(&at, now())), id)
}

// TouchToken sets LastUsedAt of the token; a zero lastUsed means now.
func (c *conn) TouchToken(ctx context.Context, id int64, lastUsed time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("tokens id %d", id),
		"UPDATE tokens SET last_used_at = ? WHERE id = ?", c.d.EncodeTime(stamp(&lastUsed, now())), id)
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

const auditCols = "id, at, principal, principal_name, action, repo, subject, digest, ip, details"

func auditDests(ev *index.AuditEvent) []any {
	return []any{&ev.ID, timeDest{&ev.At}, &ev.Principal, textDest{&ev.PrincipalName}, &ev.Action, &ev.Repo, &ev.Subject,
		textDest{(*string)(&ev.Digest)}, &ev.IP, rawDest{&ev.Details}}
}

// defaultAuditLimit is the page size of ListAudit when the filter has none.
const defaultAuditLimit = 100

// ListAudit returns the events matching f, newest first. Since is inclusive, Until exclusive.
func (c *conn) ListAudit(ctx context.Context, f index.AuditFilter) ([]index.AuditEvent, error) {
	var w where
	for _, eq := range []struct{ col, v string }{{"principal", f.Principal}, {"repo", f.Repo}, {"action", f.Action}} {
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
		"SELECT "+auditCols+" FROM audit"+w.clause()+" ORDER BY at DESC, id DESC LIMIT ? OFFSET ?", args...)
}

// AppendAudit inserts ev; At is set when zero.
func (c *conn) AppendAudit(ctx context.Context, ev *index.AuditEvent) error {
	var uid any // the uid of a user principal, whose name the row gets when it has none
	if rest, ok := strings.CutPrefix(ev.Principal, "user:"); ok && index.IsUID(rest) {
		uid = rest
	}
	return c.scanRow(ctx, "audit "+ev.Action,
		"INSERT INTO audit (at, principal, principal_name, action, repo, subject, digest, ip, details)"+
			" VALUES (?, ?, COALESCE(?, (SELECT COALESCE(deleted_name, name) FROM users WHERE CAST(uid AS TEXT) = ?)), ?, ?, ?, ?, ?, ?)"+
			" RETURNING id, principal_name",
		[]any{c.d.EncodeTime(stamp(&ev.At, now())), ev.Principal, textArg(ev.PrincipalName), uid, ev.Action, ev.Repo, ev.Subject,
			textArg(string(ev.Digest)), ev.IP, rawArg(ev.Details)},
		&ev.ID, textDest{&ev.PrincipalName})
}
