package auth

import (
	"context"
	"errors"
	"math/rand/v2"
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
