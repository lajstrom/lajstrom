package auth

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Asking the IdP again about each OIDC login (docs/spec/rbac.md §8, #1282; ADR 0037): the
// callback seals the login's refresh token into a row of oidc_refresh, the mint of the login's
// token joins the row to the token, and serve refreshes it every auth.oidc.refresh.interval.

// RefreshAAD is the additional data of the sealed refresh token of the oidc_refresh row id: a
// ciphertext moved to another row does not open.
func RefreshAAD(id int64) string { return "oidc_refresh:" + strconv.FormatInt(id, 10) }

// RefreshDue returns when a refresh d after base is due: base plus d, less up to a tenth of d at
// random, so that no login goes longer than d unasked and the logins of one morning spread out.
func RefreshDue(base time.Time, d time.Duration) time.Time {
	return base.Add(d - rand.N(d/10+1)) //nolint:gosec // the jitter of a schedule, not a secret
}

// LoginRefresh joins the token of an OIDC login, at its mint, to the oidc_refresh row its
// callback made (docs/spec/rbac.md §8, Asking the IdP again): Login is the id of the oidc_logins
// row of the login, the code login's or, for a terminal, the device request's; Interval is
// auth.oidc.refresh.interval. A zero Interval joins nothing.
type LoginRefresh struct {
	Login    int64
	Interval time.Duration
}

// join returns the step of the mint's transaction that gives the row of the login its token and
// its first due time, the interval after the mint less the jitter; nil when r joins nothing. A
// login without a row keeps no refresh token: the IdP issued none, or the replica of its callback
// does not refresh, and its token lasts its lifetime.
func (r LoginRefresh) join(ctx context.Context) func(index.Writer, *index.Token) error {
	if r.Interval <= 0 {
		return nil
	}
	return func(w index.Writer, t *index.Token) error {
		err := w.JoinOIDCRefresh(ctx, r.Login, t.ID, RefreshDue(t.CreatedAt, r.Interval))
		if errors.Is(err, index.ErrNotFound) {
			return nil
		}
		return err
	}
}

// The audit rows of serve's refresh of the OIDC logins (docs/spec/rbac.md §5, §8, #1282).
const (
	// SourceRefresh is their source.
	SourceRefresh = "refresh"
	// ReasonRefused is the reason of the token.revoke of a login whose refresh the IdP refused.
	ReasonRefused = "refused"
	// ActionUserGroups is the action of a refresh that changed an identity's groups.
	ActionUserGroups = "user.groups"
)

// RevokeRefused revokes on w, in the caller's transaction, the token tokenID of an OIDC login
// whose refresh the IdP refused (docs/spec/rbac.md §8, Asking the IdP again): the server revokes
// it, not a person, so its token.revoke row is by anonymous, source refresh, with the details of
// Revoke and reason refused and refreshedAt, the IdP's last confirmation of the login. The user
// and its other tokens are as they were. A token revoked already, by another replica too, stays
// as it is, and the answer is nil: no row. The caller passes the row to WriteAudit after the
// commit.
func (s *Tokens) RevokeRefused(ctx context.Context, w index.Writer, tokenID int64, refreshedAt time.Time) (*index.AuditEvent, error) {
	ctx = WithPrincipal(ctx, Anonymous)
	row, own, err := find(ctx, w, tokenID)
	if err != nil || row.RevokedAt != nil {
		return nil, err
	}
	if err := w.RevokeToken(ctx, row.ID, s.now()); errors.Is(err, index.ErrConflict) {
		return nil, nil // revoked meanwhile, with its row
	} else if err != nil {
		return nil, err
	}
	ev, err := s.record(ctx, w, ActionTokenRevoke, own, Origin{Source: SourceRefresh}, map[string]any{
		"id": row.ID, "kind": row.Kind, "name": row.Name, "reason": ReasonRefused,
		"refreshedAt": refreshedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// WriteAudit passes ev, a row of RevokeRefused, to the audit writer after the commit of its
// transaction; nothing for nil.
func (s *Tokens) WriteAudit(ctx context.Context, ev *index.AuditEvent) {
	if ev != nil {
		s.writeAudit(ctx, *ev)
	}
}

// RefreshGroups replaces on w, in the caller's transaction, the groups of the identity of issuer
// and subject with groups, those of the ID token of a refresh of its login tokenID, when they
// differ from its own (docs/spec/rbac.md §8, Asking the IdP again): SetIdentityGroups, which
// moves auth_version, with the identity's login and the end of its groups as they are, and a
// user.groups row by anonymous, source refresh, subject the user's, with issuer, subject,
// groups, previous and token. The same groups in another order write nothing, and the answer is
// nil. The caller passes the row to WriteAudit after the commit.
func (s *Users) RefreshGroups(ctx context.Context, w index.Writer, issuer, subject string, groups []string, tokenID int64) (*index.AuditEvent, error) {
	id, err := w.GetIdentity(ctx, issuer, subject)
	if err != nil {
		return nil, err
	}
	all, err := w.ListIdentityGroups(ctx)
	if err != nil {
		return nil, err
	}
	previous := []string{}
	for _, g := range all {
		if g.IdentityID == id.ID {
			previous = append(previous, g.Name)
		}
	}
	if groups == nil {
		groups = []string{}
	}
	if slices.Equal(slices.Sorted(slices.Values(previous)), slices.Sorted(slices.Values(groups))) {
		return nil, nil
	}
	end := id.LoginAt
	if id.GroupsExpireAt != nil {
		end = *id.GroupsExpireAt
	}
	if err := w.SetIdentityGroups(ctx, id.ID, groups, id.LoginAt, end); err != nil {
		return nil, err
	}
	u, err := w.GetUserByID(ctx, id.UserID)
	if err != nil {
		return nil, err
	}
	ev, err := s.record(WithPrincipal(ctx, Anonymous), w, ActionUserGroups, u, Origin{Source: SourceRefresh}, map[string]any{
		"issuer": issuer, "subject": subject, "groups": groups, "previous": previous, "token": tokenID,
	})
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// WriteAudit passes ev, a row of RefreshGroups, to the audit writer after the commit of its
// transaction; nothing for nil.
func (s *Users) WriteAudit(ctx context.Context, ev *index.AuditEvent) {
	if ev != nil {
		s.writeAudit(ctx, *ev)
	}
}
