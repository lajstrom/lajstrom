package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The CI trust policies made through the API (docs/spec/rbac.md §11, docs/spec/index-schema.md,
// ci_policies): control plane, each write in a transaction that moves auth_version.

const ciPolicyCols = "id, name, issuer, claims, grants, created_by, created_at, updated_at"

func ciPolicyDests(p *index.CIPolicy) []any {
	return []any{&p.ID, &p.Name, &p.Issuer, jsonDest[map[string][]string]{&p.Claims}, jsonDest[[]index.CIGrant]{&p.Grants},
		&p.CreatedBy, timeDest{&p.CreatedAt}, timeDest{&p.UpdatedAt}}
}

// ListCIPolicies returns every CI trust policy ordered by id.
func (c *conn) ListCIPolicies(ctx context.Context) ([]index.CIPolicy, error) {
	return getAll(ctx, c, "ci_policies", ciPolicyDests, "SELECT "+ciPolicyCols+" FROM ci_policies ORDER BY id")
}

// GetCIPolicy returns the CI trust policy called name.
func (c *conn) GetCIPolicy(ctx context.Context, name string) (*index.CIPolicy, error) {
	return getOne(ctx, c, "ci_policies "+name, ciPolicyDests, "SELECT "+ciPolicyCols+" FROM ci_policies WHERE name = ?", name)
}

// CreateCIPolicy inserts p and increments auth_version; CreatedAt is now when zero and UpdatedAt
// is CreatedAt.
func (c *conn) CreateCIPolicy(ctx context.Context, p *index.CIPolicy) error {
	created := stamp(&p.CreatedAt, now())
	p.UpdatedAt = created
	return c.withAuthChange(ctx, func(c *conn) error {
		return c.scanRow(ctx, "ci_policies "+p.Name,
			"INSERT INTO ci_policies (name, issuer, claims, grants, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)"+
				" RETURNING id",
			[]any{p.Name, p.Issuer, claimsArg(p.Claims), grantsArg(p.Grants), p.CreatedBy, c.d.EncodeTime(p.CreatedAt),
				c.d.EncodeTime(p.UpdatedAt)},
			&p.ID)
	})
}

// UpdateCIPolicy stores Issuer, Claims and Grants of p by p.ID, sets UpdatedAt, fills the columns
// that never change from the row, and increments auth_version.
func (c *conn) UpdateCIPolicy(ctx context.Context, p *index.CIPolicy) error {
	what := fmt.Sprintf("ci_policies id %d", p.ID)
	p.UpdatedAt = now()
	return c.withAuthChange(ctx, func(c *conn) error {
		if err := c.execOne(ctx, what, "UPDATE ci_policies SET issuer = ?, claims = ?, grants = ?, updated_at = ? WHERE id = ?",
			p.Issuer, claimsArg(p.Claims), grantsArg(p.Grants), c.d.EncodeTime(p.UpdatedAt), p.ID); err != nil {
			return err
		}
		row, err := getOne(ctx, c, what, ciPolicyDests, "SELECT "+ciPolicyCols+" FROM ci_policies WHERE id = ?", p.ID)
		if err != nil {
			return err
		}
		p.Name, p.CreatedBy, p.CreatedAt = row.Name, row.CreatedBy, row.CreatedAt
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
