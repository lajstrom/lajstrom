package sqlstore

import (
	"context"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// OIDC (docs/spec/rbac.md §8): identities and their groups (control plane), and the login
// requests of oidc_logins (coordination).

const identityCols = "id, user_id, issuer, subject, created_at, login_at, groups_expire_at"

func identityDests(i *index.Identity) []any {
	return []any{&i.ID, &i.UserID, &i.Issuer, &i.Subject, timeDest{&i.CreatedAt}, timeDest{&i.LoginAt},
		nullTimeDest{&i.GroupsExpireAt}}
}

// GetIdentity returns the identity of issuer and subject.
func (c *conn) GetIdentity(ctx context.Context, issuer, subject string) (*index.Identity, error) {
	return getOne(ctx, c, "identities of "+issuer, identityDests,
		"SELECT "+identityCols+" FROM identities WHERE issuer = ? AND subject = ?", issuer, subject)
}

// ListIdentityGroups returns every identity's groups with its user, ordered by uid and name.
func (c *conn) ListIdentityGroups(ctx context.Context) ([]index.IdentityGroup, error) {
	return getAll(ctx, c, "identity_groups", func(g *index.IdentityGroup) []any {
		return []any{&g.IdentityID, &g.UserID, textDest{&g.UID}, &g.Name, nullTimeDest{&g.ExpiresAt}}
	}, "SELECT g.identity_id, i.user_id, u.uid, g.name, i.groups_expire_at FROM identity_groups g"+
		" JOIN identities i ON i.id = g.identity_id JOIN users u ON u.id = i.user_id ORDER BY u.uid, g.name")
}

// CreateIdentity inserts i, CreatedAt and LoginAt now when zero.
func (c *conn) CreateIdentity(ctx context.Context, i *index.Identity) error {
	created := stamp(&i.CreatedAt, now())
	stamp(&i.LoginAt, created)
	i.GroupsExpireAt = stampNull(i.GroupsExpireAt)
	return c.scanRow(ctx, "identities of "+i.Issuer,
		"INSERT INTO identities (user_id, issuer, subject, created_at, login_at, groups_expire_at)"+
			" VALUES (?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{i.UserID, i.Issuer, i.Subject, c.d.EncodeTime(i.CreatedAt), c.d.EncodeTime(i.LoginAt),
			c.d.encodeNullTime(i.GroupsExpireAt)},
		&i.ID)
}

// DeleteIdentity removes the identity; its groups go with it (ON DELETE CASCADE).
func (c *conn) DeleteIdentity(ctx context.Context, id int64) error {
	return c.execOne(ctx, fmt.Sprintf("identities id %d", id), "DELETE FROM identities WHERE id = ?", id)
}

// SetIdentityGroups replaces the identity's groups, stores the time of its login and of the
// groups' end, and increments auth_version, in one transaction.
func (c *conn) SetIdentityGroups(ctx context.Context, id int64, names []string, loginAt, groupsExpireAt time.Time) error {
	what := fmt.Sprintf("identity_groups of %d", id)
	return c.withAuthChange(ctx, func(c *conn) error {
		if err := c.execOne(ctx, fmt.Sprintf("identities id %d", id),
			"UPDATE identities SET login_at = ?, groups_expire_at = ? WHERE id = ?",
			c.d.EncodeTime(Normalize(loginAt)), c.d.EncodeTime(Normalize(groupsExpireAt)), id); err != nil {
			return err
		}
		if _, err := c.exec(ctx, what, "DELETE FROM identity_groups WHERE identity_id = ?", id); err != nil {
			return err
		}
		for _, name := range names {
			if _, err := c.exec(ctx, what, "INSERT INTO identity_groups (identity_id, name) VALUES (?, ?)", id, name); err != nil {
				return err
			}
		}
		return nil
	})
}

const oidcLoginCols = "id, kind, state, binding, nonce, verifier, device_id, device_code, user_code, status, user_id," +
	" polled_at, created_at, expires_at"

func oidcLoginDests(l *index.OIDCLogin) []any {
	return []any{&l.ID, &l.Kind, textDest{&l.State}, textDest{&l.Binding}, textDest{&l.Nonce}, textDest{&l.Verifier},
		nullIDDest{&l.DeviceID}, textDest{&l.DeviceCode}, textDest{&l.UserCode}, &l.Status, nullIDDest{&l.UserID},
		nullTimeDest{&l.PolledAt}, timeDest{&l.CreatedAt}, timeDest{&l.ExpiresAt}}
}

// GetOIDCLogin returns the row with id.
func (c *conn) GetOIDCLogin(ctx context.Context, id int64) (*index.OIDCLogin, error) {
	return getOne(ctx, c, fmt.Sprintf("oidc_logins id %d", id), oidcLoginDests,
		"SELECT "+oidcLoginCols+" FROM oidc_logins WHERE id = ?", id)
}

// GetOIDCLoginByState returns the row of the state's hash.
func (c *conn) GetOIDCLoginByState(ctx context.Context, state string) (*index.OIDCLogin, error) {
	return c.oidcLoginBy(ctx, "state", state)
}

// GetOIDCLoginByDeviceCode returns the row of the device code's hash.
func (c *conn) GetOIDCLoginByDeviceCode(ctx context.Context, deviceCode string) (*index.OIDCLogin, error) {
	return c.oidcLoginBy(ctx, "device_code", deviceCode)
}

// GetOIDCLoginByUserCode returns the row of the user code.
func (c *conn) GetOIDCLoginByUserCode(ctx context.Context, userCode string) (*index.OIDCLogin, error) {
	return c.oidcLoginBy(ctx, "user_code", userCode)
}

// oidcLoginBy returns the row whose column col, one of the unique ones, is value; "" is NULL,
// which no row is found by. The value is a hash or a user code, and is not named in errors.
func (c *conn) oidcLoginBy(ctx context.Context, col, value string) (*index.OIDCLogin, error) {
	what := "oidc_logins by " + col
	if value == "" {
		return nil, notFound(what)
	}
	return getOne(ctx, c, what, oidcLoginDests, "SELECT "+oidcLoginCols+" FROM oidc_logins WHERE "+col+" = ?", value)
}

// CreateOIDCLogin inserts l, CreatedAt now when zero.
func (c *conn) CreateOIDCLogin(ctx context.Context, l *index.OIDCLogin) error {
	stamp(&l.CreatedAt, now())
	l.ExpiresAt, l.PolledAt = Normalize(l.ExpiresAt), stampNull(l.PolledAt)
	return c.scanRow(ctx, "oidc_logins "+l.Kind,
		"INSERT INTO oidc_logins (kind, state, binding, nonce, verifier, device_id, device_code, user_code, status, user_id,"+
			" polled_at, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{l.Kind, textArg(l.State), textArg(l.Binding), textArg(l.Nonce), textArg(l.Verifier), idArg(l.DeviceID),
			textArg(l.DeviceCode), textArg(l.UserCode), l.Status, idArg(l.UserID), c.d.encodeNullTime(l.PolledAt),
			c.d.EncodeTime(l.CreatedAt), c.d.EncodeTime(l.ExpiresAt)},
		&l.ID)
}

// UpdateOIDCLogin stores Status, UserID and PolledAt of l when the row's status is from: one
// UPDATE, so that of two requests that read the same status one changes it.
func (c *conn) UpdateOIDCLogin(ctx context.Context, l *index.OIDCLogin, from string) error {
	what := fmt.Sprintf("oidc_logins id %d", l.ID)
	l.PolledAt = stampNull(l.PolledAt)
	n, err := c.exec(ctx, what, "UPDATE oidc_logins SET status = ?, user_id = ?, polled_at = ? WHERE id = ? AND status = ?",
		l.Status, idArg(l.UserID), c.d.encodeNullTime(l.PolledAt), l.ID, from)
	switch {
	case err != nil:
		return err
	case n == 1:
		return nil
	}
	if _, err := c.GetOIDCLogin(ctx, l.ID); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is not %s any more", index.ErrConflict, what, from)
}

// ExpireOIDCLogins deletes the rows that expired before before.
func (c *conn) ExpireOIDCLogins(ctx context.Context, before time.Time) (int, error) {
	return c.exec(ctx, "oidc_logins", "DELETE FROM oidc_logins WHERE expires_at < ?", c.d.EncodeTime(Normalize(before)))
}
