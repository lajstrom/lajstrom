package sqlstore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The CI trust policies made through the API (docs/spec/rbac.md §11, docs/spec/index-schema.md,
// ci_policies): control plane, each write in a transaction that moves auth_version.

const ciPolicyCols = "id, name, issuer, claims, grants, state, expires_at, renewal_expires_at, created_by, created_at," +
	" updated_by, updated_at, approved_by, approved_at, revision"

func ciPolicyDests(p *index.CIPolicy) []any {
	return []any{&p.ID, &p.Name, &p.Issuer, jsonDest[map[string][]string]{&p.Claims}, jsonDest[[]index.CIGrant]{&p.Grants},
		&p.State, nullTimeDest{&p.ExpiresAt}, nullTimeDest{&p.RenewalExpiresAt}, &p.CreatedBy, timeDest{&p.CreatedAt},
		&p.UpdatedBy, timeDest{&p.UpdatedAt}, textDest{&p.ApprovedBy}, nullTimeDest{&p.ApprovedAt}, &p.Revision}
}

// ListCIPolicies returns every CI trust policy ordered by id.
func (c *conn) ListCIPolicies(ctx context.Context) ([]index.CIPolicy, error) {
	return getAll(ctx, c, "ci_policies", ciPolicyDests, "SELECT "+ciPolicyCols+" FROM ci_policies ORDER BY id")
}

// GetCIPolicy returns the CI trust policy called name.
func (c *conn) GetCIPolicy(ctx context.Context, name string) (*index.CIPolicy, error) {
	return getOne(ctx, c, "ci_policies "+name, ciPolicyDests, "SELECT "+ciPolicyCols+" FROM ci_policies WHERE name = ?", name)
}

// CreateCIPolicy inserts p at revision 1 and increments auth_version; CreatedAt is now when zero,
// UpdatedAt is CreatedAt, State "" is active and UpdatedBy "" is CreatedBy.
func (c *conn) CreateCIPolicy(ctx context.Context, p *index.CIPolicy) error {
	created := stamp(&p.CreatedAt, now())
	p.UpdatedAt, p.Revision = created, 1
	p.State, p.UpdatedBy = cmp.Or(p.State, index.CIPolicyActive), cmp.Or(p.UpdatedBy, p.CreatedBy)
	p.ExpiresAt, p.RenewalExpiresAt, p.ApprovedAt = stampNull(p.ExpiresAt), stampNull(p.RenewalExpiresAt), stampNull(p.ApprovedAt)
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.scanRow(ctx, "ci_policies "+p.Name,
			"INSERT INTO ci_policies (name, issuer, claims, grants, state, expires_at, renewal_expires_at, created_by, created_at,"+
				" updated_by, updated_at, approved_by, approved_at, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1) RETURNING id",
			[]any{p.Name, p.Issuer, claimsArg(p.Claims), grantsArg(p.Grants), p.State, c.d.encodeNullTime(p.ExpiresAt),
				c.d.encodeNullTime(p.RenewalExpiresAt), p.CreatedBy, c.d.EncodeTime(p.CreatedAt), p.UpdatedBy, c.d.EncodeTime(p.UpdatedAt),
				textArg(p.ApprovedBy), c.d.encodeNullTime(p.ApprovedAt)},
			&p.ID)
	})
}

// UpdateCIPolicy stores what changes of p by p.ID while the row is at p.Revision, sets UpdatedAt,
// moves the revision, fills the columns that never change from the row, and increments
// auth_version. A row at another revision is index.ErrConflict.
func (c *conn) UpdateCIPolicy(ctx context.Context, p *index.CIPolicy) error {
	what := fmt.Sprintf("ci_policies id %d", p.ID)
	p.UpdatedAt = now()
	p.ExpiresAt, p.RenewalExpiresAt, p.ApprovedAt = stampNull(p.ExpiresAt), stampNull(p.RenewalExpiresAt), stampNull(p.ApprovedAt)
	return c.withAuthChange(ctx, func(c *conn) error {
		n, err := c.exec(ctx, what, "UPDATE ci_policies SET issuer = ?, claims = ?, grants = ?, state = ?, expires_at = ?,"+
			" renewal_expires_at = ?, updated_by = ?, updated_at = ?, approved_by = ?, approved_at = ?, revision = revision + 1"+
			" WHERE id = ? AND revision = ?",
			p.Issuer, claimsArg(p.Claims), grantsArg(p.Grants), p.State, c.d.encodeNullTime(p.ExpiresAt),
			c.d.encodeNullTime(p.RenewalExpiresAt), p.UpdatedBy, c.d.EncodeTime(p.UpdatedAt), textArg(p.ApprovedBy),
			c.d.encodeNullTime(p.ApprovedAt), p.ID, p.Revision)
		if err != nil {
			return err
		}
		row, err := getOne(ctx, c, what, ciPolicyDests, "SELECT "+ciPolicyCols+" FROM ci_policies WHERE id = ?", p.ID)
		switch {
		case err != nil:
			return err
		case n != 1:
			return fmt.Errorf("%w: %s is at revision %d, not %d", index.ErrConflict, what, row.Revision, p.Revision)
		}
		p.Name, p.CreatedBy, p.CreatedAt, p.Revision = row.Name, row.CreatedBy, row.CreatedAt, row.Revision
		return nil
	})
}

// DeleteCIPolicy removes the policy and increments auth_version.
func (c *conn) DeleteCIPolicy(ctx context.Context, id int64) error {
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.execOne(ctx, fmt.Sprintf("ci_policies id %d", id), "DELETE FROM ci_policies WHERE id = ?", id)
	})
}

// claimsArg and grantsArg are the json columns of a policy, never NULL: no claim is {}, no grant
// [].
func claimsArg(m map[string][]string) any {
	if m == nil {
		m = map[string][]string{}
	}
	b, _ := json.Marshal(m) // a map of strings to strings always marshals
	return string(b)
}

func grantsArg(g []index.CIGrant) any {
	if g == nil {
		g = []index.CIGrant{}
	}
	b, _ := json.Marshal(g) // a slice of structs of strings always marshals
	return string(b)
}
